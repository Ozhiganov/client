package service

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/cenkalti/backoff"
	"github.com/keybase/client/go/chat"
	"github.com/keybase/client/go/engine"
	"github.com/keybase/client/go/gregor"
	grclient "github.com/keybase/client/go/gregor/client"
	"github.com/keybase/client/go/gregor/storage"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/gregor1"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/clockwork"
	"github.com/keybase/go-codec/codec"
	"github.com/keybase/go-framed-msgpack-rpc/rpc"
	jsonw "github.com/keybase/go-jsonw"
)

const GregorRequestTimeout time.Duration = 30 * time.Second

var ErrGregorTimeout = errors.New("Network request timed out.")

const GregorConnectionRetryInterval time.Duration = 2 * time.Second

type IdentifyUIHandler struct {
	libkb.Contextified
	connID      libkb.ConnectionID
	alwaysAlive bool
}

var _ libkb.GregorInBandMessageHandler = (*IdentifyUIHandler)(nil)

func NewIdentifyUIHandler(g *libkb.GlobalContext, connID libkb.ConnectionID) IdentifyUIHandler {
	return IdentifyUIHandler{
		Contextified: libkb.NewContextified(g),
		connID:       connID,
		alwaysAlive:  false,
	}
}

func (h IdentifyUIHandler) IsAlive() bool {
	return (h.alwaysAlive || h.G().ConnectionManager.LookupConnection(h.connID) != nil)
}

func (h IdentifyUIHandler) Name() string {
	return "IdentifyUIHandler"
}

func (h *IdentifyUIHandler) toggleAlwaysAlive(alive bool) {
	h.alwaysAlive = alive
}

type gregorFirehoseHandler struct {
	libkb.Contextified
	connID libkb.ConnectionID
	cli    keybase1.GregorUIClient
}

func newGregorFirehoseHandler(g *libkb.GlobalContext, connID libkb.ConnectionID, xp rpc.Transporter) *gregorFirehoseHandler {
	return &gregorFirehoseHandler{
		Contextified: libkb.NewContextified(g),
		connID:       connID,
		cli:          keybase1.GregorUIClient{Cli: rpc.NewClient(xp, libkb.ErrorUnwrapper{})},
	}
}

func (h *gregorFirehoseHandler) IsAlive() bool {
	return h.G().ConnectionManager.LookupConnection(h.connID) != nil
}

func (h *gregorFirehoseHandler) PushState(s gregor1.State, r keybase1.PushReason) {
	err := h.cli.PushState(context.Background(), keybase1.PushStateArg{State: s, Reason: r})
	if err != nil {
		h.G().Log.Error(fmt.Sprintf("Error in firehose push state: %s", err))
	}
}

func (h *gregorFirehoseHandler) PushOutOfBandMessages(m []gregor1.OutOfBandMessage) {
	err := h.cli.PushOutOfBandMessages(context.Background(), m)
	if err != nil {
		h.G().Log.Error(fmt.Sprintf("Error in firehose push out-of-band messages: %s", err))
	}
}

type gregorHandler struct {
	libkb.Contextified

	// This lock is to protect ibmHandlers and gregorCli and firehoseHandlers. Only public methods
	// should grab it.
	sync.Mutex
	ibmHandlers      []libkb.GregorInBandMessageHandler
	gregorCli        *grclient.Client
	firehoseHandlers []libkb.GregorFirehoseHandler
	badger           *Badger

	// This mutex protects the con object
	connMutex     sync.Mutex
	conn          *rpc.Connection
	uri           *rpc.FMPURI
	startPingLoop sync.Once

	cli              rpc.GenericClient
	sessionID        gregor1.SessionID
	skipRetryConnect bool
	freshReplay      bool

	identNotifier *chat.IdentifyNotifier
	chatSync      *chat.Syncer

	transportForTesting *connTransport

	// Function for determining if a new BroadcastMessage should trigger
	// a pushState call to firehose handlers
	pushStateFilter func(m gregor.Message) bool

	shutdownCh chan struct{}
}

var _ libkb.GregorDismisser = (*gregorHandler)(nil)
var _ libkb.GregorListener = (*gregorHandler)(nil)

type gregorLocalDb struct {
	db *libkb.JSONLocalDb
}

func newLocalDB(g *libkb.GlobalContext) *gregorLocalDb {
	return &gregorLocalDb{db: g.LocalDb}
}

func dbKey(u gregor.UID) libkb.DbKey {
	return libkb.DbKey{Typ: libkb.DBGregor, Key: hex.EncodeToString(u.Bytes())}
}

func (db *gregorLocalDb) Store(u gregor.UID, b []byte) error {
	return db.db.PutRaw(dbKey(u), b)
}

func (db *gregorLocalDb) Load(u gregor.UID) (res []byte, e error) {
	res, _, err := db.db.GetRaw(dbKey(u))
	return res, err
}

func newGregorHandler(g *libkb.GlobalContext) (*gregorHandler, error) {
	gh := &gregorHandler{
		Contextified:    libkb.NewContextified(g),
		freshReplay:     true,
		pushStateFilter: func(m gregor.Message) bool { return true },
		badger:          nil,
		identNotifier:   chat.NewIdentifyNotifier(g),
		chatSync:        chat.NewSyncer(g),
	}

	// Attempt to create a gregor client initially, if we are not logged in
	// or don't have user/device info in G, then this won't work
	if err := gh.resetGregorClient(); err != nil {
		g.Log.Warning("unable to create push service client: %s", err)
	}

	return gh, nil
}

func (g *gregorHandler) resetGregorClient() (err error) {
	defer g.G().Trace("gregorHandler#newGregorClient", func() error { return err })()
	of := gregor1.ObjFactory{}
	sm := storage.NewMemEngine(of, clockwork.NewRealClock())

	var guid gregor.UID
	var gdid gregor.DeviceID
	var b []byte

	uid := g.G().Env.GetUID()
	if !uid.Exists() {
		err = errors.New("no UID; probably not logged in")
		return err
	}
	if b = uid.ToBytes(); b == nil {
		err = errors.New("Can't convert UID to byte array")
		return err
	}
	if guid, err = of.MakeUID(b); err != nil {
		return err
	}

	did := g.G().Env.GetDeviceID()
	if !did.Exists() {
		err = errors.New("no device ID; probably not logged in")
		return err
	}
	if b, err = hex.DecodeString(did.String()); err != nil {
		return err
	}
	if gdid, err = of.MakeDeviceID(b); err != nil {
		return err
	}

	// Create client object
	gcli := grclient.NewClient(guid, gdid, sm, newLocalDB(g.G()), g.G().Env.GetGregorSaveInterval(), g.G().Log)

	// Bring up local state
	g.Debug("restoring state from leveldb")
	if err = gcli.Restore(); err != nil {
		// If this fails, we'll keep trying since the server can bail us out
		g.Debug("restore local state failed: %s", err)
	}

	g.gregorCli = gcli
	return nil
}

func (g *gregorHandler) getGregorCli() (*grclient.Client, error) {
	if g.gregorCli == nil {
		return nil, errors.New("client unset")
	}
	return g.gregorCli, nil
}

func (g *gregorHandler) getRPCCli() rpc.GenericClient {
	return g.cli
}

func (g *gregorHandler) Debug(s string, args ...interface{}) {
	g.G().Log.Debug("PushHandler: "+s, args...)
}

func (g *gregorHandler) Warning(s string, args ...interface{}) {
	g.G().Log.Warning("PushHandler: "+s, args...)
}

func (g *gregorHandler) Errorf(s string, args ...interface{}) {
	g.G().Log.Errorf("PushHandler: "+s, args...)
}

func (g *gregorHandler) SetPushStateFilter(f func(m gregor.Message) bool) {
	g.pushStateFilter = f
}

func (g *gregorHandler) Connect(uri *rpc.FMPURI) (err error) {

	defer g.G().Trace("gregorHandler#Connect", func() error { return err })()

	// Create client interface to gregord; the user needs to be logged in for this
	// to work
	if err = g.resetGregorClient(); err != nil {
		return err
	}

	// In case we need to interrupt auth'ing or the ping loop,
	// set up this channel.
	g.shutdownCh = make(chan struct{})

	g.uri = uri
	if uri.UseTLS() {
		err = g.connectTLS()
	} else {
		err = g.connectNoTLS()
	}

	return err
}

func (g *gregorHandler) HandlerName() string {
	return "keybase service"
}

// PushHandler adds a new ibm handler to our list. This is usually triggered
// when an external entity (like Electron) connects to the service, and we can
// safely send Gregor information to it
func (g *gregorHandler) PushHandler(handler libkb.GregorInBandMessageHandler) {
	g.Lock()
	defer g.Unlock()

	g.G().Log.Debug("pushing inband handler %s to position %d", handler.Name(), len(g.ibmHandlers))

	g.ibmHandlers = append(g.ibmHandlers, handler)

	// Only try replaying if we are logged in, it's possible that a handler can
	// attach before that is true (like if we start the service logged out and
	// Electron connects)
	if g.IsConnected() {
		if _, err := g.replayInBandMessages(context.TODO(), gregor1.IncomingClient{Cli: g.cli},
			time.Time{}, handler); err != nil {
			g.Errorf("replayInBandMessages on PushHandler failed: %s", err)
		}

		if g.badger != nil {
			s, err := g.getState()
			if err != nil {
				g.Warning("Cannot get state in PushHandler: %s", err)
				return
			}
			g.badger.PushState(s)
		}
	}
}

// PushFirehoseHandler pushes a new firehose handler onto the list of currently
// active firehose handles. We can have several of these active at once. All
// get the "firehose" of gregor events. They're removed lazily as their underlying
// connections die.
func (g *gregorHandler) PushFirehoseHandler(handler libkb.GregorFirehoseHandler) {
	g.Lock()
	defer g.Unlock()
	g.firehoseHandlers = append(g.firehoseHandlers, handler)

	s, err := g.getState()
	if err != nil {
		g.Warning("Cannot push state in firehose handler: %s", err)
		return
	}
	handler.PushState(s, keybase1.PushReason_RECONNECTED)
}

// iterateOverFirehoseHandlers applies the function f to all live fireshose handlers
// and then resets the list to only include the live ones.
func (g *gregorHandler) iterateOverFirehoseHandlers(f func(h libkb.GregorFirehoseHandler)) {
	var freshHandlers []libkb.GregorFirehoseHandler
	for _, h := range g.firehoseHandlers {
		if h.IsAlive() {
			f(h)
			freshHandlers = append(freshHandlers, h)
		}
	}
	g.firehoseHandlers = freshHandlers
	return
}

func (g *gregorHandler) pushState(r keybase1.PushReason) {
	s, err := g.getState()
	if err != nil {
		g.Warning("Cannot push state in firehose handler: %s", err)
		return
	}
	g.iterateOverFirehoseHandlers(func(h libkb.GregorFirehoseHandler) { h.PushState(s, r) })

	if g.badger != nil {
		g.badger.PushState(s)
	}
}

func (g *gregorHandler) pushOutOfBandMessages(m []gregor1.OutOfBandMessage) {
	g.iterateOverFirehoseHandlers(func(h libkb.GregorFirehoseHandler) { h.PushOutOfBandMessages(m) })
}

// replayInBandMessages will replay all the messages in the current state from
// the given time. If a handler is specified, it will only replay using it,
// otherwise it will try all of them. gregorHandler needs to be locked when calling
// this function.
func (g *gregorHandler) replayInBandMessages(ctx context.Context, cli gregor1.IncomingInterface,
	t time.Time, handler libkb.GregorInBandMessageHandler) ([]gregor.InBandMessage, error) {

	var msgs []gregor.InBandMessage
	var err error

	gcli, err := g.getGregorCli()
	if err != nil {
		return nil, err
	}

	if t.IsZero() {
		g.Debug("replayInBandMessages: fresh replay: using state items")
		state, err := gcli.StateMachineState(nil)
		if err != nil {
			g.Debug("unable to fetch state for replay: %s", err)
			return nil, err
		}
		if msgs, err = gcli.InBandMessagesFromState(state); err != nil {
			g.Debug("unable to fetch messages from state for replay: %s", err)
			return nil, err
		}
	} else {
		g.Debug("replayInBandMessages: incremental replay: using ibms since")
		if msgs, err = gcli.StateMachineInBandMessagesSince(t); err != nil {
			g.Debug("unable to fetch messages for replay: %s", err)
			return nil, err
		}
	}

	g.Debug("replaying %d messages", len(msgs))
	for _, msg := range msgs {
		// If we have a handler, just run it on that, otherwise run it against
		// all of the handlers we know about
		if handler == nil {
			err = g.handleInBandMessage(ctx, cli, msg)
		} else {
			_, err = g.handleInBandMessageWithHandler(ctx, cli, msg, handler)
		}

		// If an error happens when replaying, don't kill everything else that
		// follows, just make a warning.
		if err != nil {
			g.Debug("Failure in message replay: %s", err.Error())
			err = nil
		}
	}

	return msgs, nil
}

func (g *gregorHandler) IsConnected() bool {
	return g.conn != nil && g.conn.IsConnected()
}

// serverSync is called from
// gregord. This can happen either on initial startup, or after a reconnect. Needs
// to be called with gregorHandler locked.
func (g *gregorHandler) serverSync(ctx context.Context,
	cli gregor1.IncomingInterface) ([]gregor.InBandMessage, []gregor.InBandMessage, error) {

	gcli, err := g.getGregorCli()
	if err != nil {
		return nil, nil, err
	}

	// Get time of the last message we synced (unless this is our first time syncing)
	var t time.Time
	if !g.freshReplay {
		pt := gcli.StateMachineLatestCTime()
		if pt != nil {
			t = *pt
		}
		g.Debug("starting replay from: %s", t)
	} else {
		g.Debug("performing a fresh replay")
	}

	// Sync down everything from the server
	consumedMsgs, err := gcli.Sync(cli)
	if err != nil {
		g.Errorf("error syncing from the server, reason: %s", err)
		return nil, nil, err
	}

	// Replay in-band messages
	replayedMsgs, err := g.replayInBandMessages(ctx, cli, t, nil)
	if err != nil {
		g.Errorf("replay messages failed: %s", err)
		return nil, nil, err
	}

	// All done with fresh replays
	g.freshReplay = false

	g.pushState(keybase1.PushReason_RECONNECTED)

	return replayedMsgs, consumedMsgs, nil
}

func (g *gregorHandler) makeReconnectOobm() gregor1.Message {
	return gregor1.Message{
		Oobm_: &gregor1.OutOfBandMessage{
			System_: "internal.reconnect",
		},
	}
}

// OnConnect is called by the rpc library to indicate we have connected to
// gregord
func (g *gregorHandler) OnConnect(ctx context.Context, conn *rpc.Connection,
	cli rpc.GenericClient, srv *rpc.Server) error {
	g.Lock()
	defer g.Unlock()

	timeoutCli := WrapGenericClientWithTimeout(cli, GregorRequestTimeout, ErrGregorTimeout)

	g.Debug("connected")
	g.Debug("registering protocols")
	if err := srv.Register(gregor1.OutgoingProtocol(g)); err != nil {
		return err
	}

	// Use the client parameter instead of conn.GetClient(), since we can get stuck
	// in a recursive loop if we keep retrying on reconnect.
	if err := g.auth(ctx, timeoutCli); err != nil {
		return err
	}

	// Sync down events since we have been dead
	replayedMsgs, consumedMsgs, err := g.serverSync(ctx, gregor1.IncomingClient{Cli: timeoutCli})
	if err != nil {
		g.Errorf("sync failure: %s", err)
	} else {
		g.Debug("sync success: replayed: %d consumed: %d",
			len(replayedMsgs), len(consumedMsgs))
	}

	// Sync chat data using a Syncer object
	gcli, err := g.getGregorCli()
	if err == nil {
		chatCli := chat1.RemoteClient{Cli: cli}
		uid := gcli.User.(gregor1.UID)
		if err := g.chatSync.Connected(ctx, chatCli, uid); err != nil {
			return err
		}
	}

	// Sync badge state in the background
	if g.badger != nil {
		go func(badger *Badger) {
			badger.Resync(context.Background(), &chat1.RemoteClient{Cli: g.cli})
		}(g.badger)
	}

	// Broadcast reconnect oobm. Spawn this off into a goroutine so that we don't delay
	// reconnection any longer than we have to.
	go func(m gregor1.Message) {
		g.BroadcastMessage(context.Background(), m)
	}(g.makeReconnectOobm())

	return nil
}

func (g *gregorHandler) OnConnectError(err error, reconnectThrottleDuration time.Duration) {
	g.Debug("connect error %s, reconnect throttle duration: %s", err, reconnectThrottleDuration)
}

func (g *gregorHandler) OnDisconnected(ctx context.Context, status rpc.DisconnectStatus) {
	g.Debug("disconnected: %v", status)

	// Alert chat syncer that we are now disconnected
	g.chatSync.Disconnected(ctx)
}

func (g *gregorHandler) OnDoCommandError(err error, nextTime time.Duration) {
	g.Debug("do command error: %s, nextTime: %s", err, nextTime)
}

func (g *gregorHandler) ShouldRetry(name string, err error) bool {
	g.Debug("should retry: name %s, err %v (returning false)", name, err)
	return false
}

func (g *gregorHandler) ShouldRetryOnConnect(err error) bool {
	if err == nil {
		return false
	}

	g.Debug("should retry on connect, err %v", err)
	if g.skipRetryConnect {
		g.Debug("should retry on connect, skip retry flag set, returning false")
		g.skipRetryConnect = false
		return false
	}

	return true
}

// BroadcastMessage is called when we receive a new messages from gregord. Grabs
// the lock protect the state machine and handleInBandMessage
func (g *gregorHandler) BroadcastMessage(ctx context.Context, m gregor1.Message) error {
	g.Lock()
	defer g.Unlock()

	// Handle the message
	ibm := m.ToInBandMessage()
	if ibm != nil {
		gcli, err := g.getGregorCli()
		if err != nil {
			return err
		}
		// Check to see if this is already in our state
		msgID := ibm.Metadata().MsgID()
		state, err := gcli.StateMachineState(nil)
		if err != nil {
			return err
		}
		if _, ok := state.GetItem(msgID); ok {
			g.Debug("msgID: %s already in state, ignoring", msgID)
			return errors.New("ignored repeat message")
		}

		g.Debug("broadcast: in-band message: msgID: %s Ctime: %s", msgID, ibm.Metadata().CTime())
		err = g.handleInBandMessage(ctx, gregor1.IncomingClient{Cli: g.cli}, ibm)

		// Send message to local state machine
		gcli.StateMachineConsumeMessage(m)

		// Forward to electron or whichever UI is listening for the new gregor state
		if g.pushStateFilter(m) {
			g.pushState(keybase1.PushReason_NEW_DATA)
		}

		return err
	}

	obm := m.ToOutOfBandMessage()
	if obm != nil {
		g.Debug("broadcast: out-of-band message: uid: %s",
			m.ToOutOfBandMessage().UID())
		return g.handleOutOfBandMessage(ctx, obm)
	}

	g.Warning("BroadcastMessage: both in-band and out-of-band message nil")
	return errors.New("invalid gregor message")
}

// handleInBandMessage runs a message on all the alive handlers. gregorHandler
// must be locked when calling this function.
func (g *gregorHandler) handleInBandMessage(ctx context.Context, cli gregor1.IncomingInterface,
	ibm gregor.InBandMessage) (err error) {

	defer g.G().Trace(fmt.Sprintf("gregorHandler#handleInBandMessage with %d handlers", len(g.ibmHandlers)), func() error { return err })()

	var freshHandlers []libkb.GregorInBandMessageHandler

	// Loop over all handlers and run the messages against any that are alive
	// If the handler is not alive, we prune it from our list
	for i, handler := range g.ibmHandlers {
		g.Debug("trying handler %s at position %d", handler.Name(), i)
		if handler.IsAlive() {
			if handled, err := g.handleInBandMessageWithHandler(ctx, cli, ibm, handler); err != nil {
				if handled {
					// Don't stop handling errors on a first failure.
					g.Errorf("failed to run %s handler: %s", handler.Name(), err)
				} else {
					g.Debug("handleInBandMessage() failed to run %s handler: %s", handler.Name(), err)
				}
			}
			freshHandlers = append(freshHandlers, handler)
		} else {
			g.Debug("skipping handler as it's marked dead: %s", handler.Name())
		}
	}

	if len(g.ibmHandlers) != len(freshHandlers) {
		g.Debug("Change # of live handlers from %d to %d", len(g.ibmHandlers), len(freshHandlers))
		g.ibmHandlers = freshHandlers
	}
	return nil
}

// handleInBandMessageWithHandler runs a message against the specified handler
func (g *gregorHandler) handleInBandMessageWithHandler(ctx context.Context, cli gregor1.IncomingInterface,
	ibm gregor.InBandMessage, handler libkb.GregorInBandMessageHandler) (bool, error) {
	g.Debug("handleInBand: %+v", ibm)

	gcli, err := g.getGregorCli()
	if err != nil {
		return false, err
	}
	state, err := gcli.StateMachineState(nil)
	if err != nil {
		return false, err
	}

	sync := ibm.ToStateSyncMessage()
	if sync != nil {
		g.Debug("state sync message")
		return false, nil
	}

	update := ibm.ToStateUpdateMessage()
	if update != nil {
		g.Debug("state update message")

		item := update.Creation()
		if item != nil {
			id := item.Metadata().MsgID().String()
			g.Debug("msg ID %s created ctime: %s", id,
				item.Metadata().CTime())

			category := ""
			if item.Category() != nil {
				category = item.Category().String()
				g.Debug("item %s has category %s", id, category)
			}

			if handled, err := handler.Create(ctx, cli, category, item); err != nil {
				return handled, err
			}
		}

		dismissal := update.Dismissal()
		if dismissal != nil {
			g.Debug("received dismissal")
			for _, id := range dismissal.MsgIDsToDismiss() {
				item, present := state.GetItem(id)
				if !present {
					g.Debug("tried to dismiss item %s, not present", id.String())
					continue
				}
				g.Debug("dismissing item %s", id.String())

				category := ""
				if item.Category() != nil {
					category = item.Category().String()
					g.Debug("dismissal %s has category %s", id, category)
				}

				if handled, err := handler.Dismiss(ctx, cli, category, item); handled && err != nil {
					return handled, err
				}
			}
			if len(dismissal.RangesToDismiss()) > 0 {
				g.Debug("message range dismissing not implemented")
			}
		}

		return true, nil
	}

	return false, nil
}

func (h IdentifyUIHandler) Create(ctx context.Context, cli gregor1.IncomingInterface, category string,
	item gregor.Item) (bool, error) {

	switch category {
	case "show_tracker_popup":
		return true, h.handleShowTrackerPopupCreate(ctx, cli, item)
	}

	return false, nil
}

func (h IdentifyUIHandler) Dismiss(ctx context.Context, cli gregor1.IncomingInterface, category string,
	item gregor.Item) (bool, error) {

	switch category {
	case "show_tracker_popup":
		return true, h.handleShowTrackerPopupDismiss(ctx, cli, item)
	}

	return false, nil
}

func (h IdentifyUIHandler) handleShowTrackerPopupCreate(ctx context.Context, cli gregor1.IncomingInterface,
	item gregor.Item) error {

	h.G().Log.Debug("handleShowTrackerPopupCreate: %+v", item)
	if item.Body() == nil {
		return errors.New("gregor handler for show_tracker_popup: nil message body")
	}
	body, err := jsonw.Unmarshal(item.Body().Bytes())
	if err != nil {
		h.G().Log.Debug("body failed to unmarshal", err)
		return err
	}
	uidString, err := body.AtPath("uid").GetString()
	if err != nil {
		h.G().Log.Debug("failed to extract uid", err)
		return err
	}
	uid, err := keybase1.UIDFromString(uidString)
	if err != nil {
		h.G().Log.Debug("failed to convert UID from string", err)
		return err
	}

	identifyUI, err := h.G().UIRouter.GetIdentifyUI()
	if err != nil {
		h.G().Log.Debug("failed to get IdentifyUI", err)
		return err
	}
	if identifyUI == nil {
		h.G().Log.Debug("got nil IdentifyUI")
		return errors.New("got nil IdentifyUI")
	}
	secretUI, err := h.G().UIRouter.GetSecretUI(0)
	if err != nil {
		h.G().Log.Debug("failed to get SecretUI", err)
		return err
	}
	if secretUI == nil {
		h.G().Log.Debug("got nil SecretUI")
		return errors.New("got nil SecretUI")
	}
	engineContext := engine.Context{
		IdentifyUI: identifyUI,
		SecretUI:   secretUI,
	}

	identifyReason := keybase1.IdentifyReason{
		Type: keybase1.IdentifyReasonType_TRACK,
		// TODO: text here?
	}
	identifyArg := keybase1.Identify2Arg{Uid: uid, Reason: identifyReason}
	identifyEng := engine.NewIdentify2WithUID(h.G(), &identifyArg)
	identifyEng.SetResponsibleGregorItem(item)
	return identifyEng.Run(&engineContext)
}

func (h IdentifyUIHandler) handleShowTrackerPopupDismiss(ctx context.Context, cli gregor1.IncomingInterface,
	item gregor.Item) error {

	h.G().Log.Debug("handleShowTrackerPopupDismiss: %+v", item)
	if item.Body() == nil {
		return errors.New("gregor dismissal for show_tracker_popup: nil message body")
	}
	body, err := jsonw.Unmarshal(item.Body().Bytes())
	if err != nil {
		h.G().Log.Debug("body failed to unmarshal", err)
		return err
	}
	uidString, err := body.AtPath("uid").GetString()
	if err != nil {
		h.G().Log.Debug("failed to extract uid", err)
		return err
	}
	uid, err := keybase1.UIDFromString(uidString)
	if err != nil {
		h.G().Log.Debug("failed to convert UID from string", err)
		return err
	}
	user, err := libkb.LoadUser(libkb.NewLoadUserByUIDArg(ctx, h.G(), uid))
	if err != nil {
		h.G().Log.Debug("failed to load user from UID", err)
		return err
	}

	identifyUI, err := h.G().UIRouter.GetIdentifyUI()
	if err != nil {
		h.G().Log.Debug("failed to get IdentifyUI", err)
		return err
	}
	if identifyUI == nil {
		h.G().Log.Debug("got nil IdentifyUI")
		return errors.New("got nil IdentifyUI")
	}

	reason := keybase1.DismissReason{
		Type: keybase1.DismissReasonType_HANDLED_ELSEWHERE,
	}
	identifyUI.Dismiss(user.GetName(), reason)

	return nil
}

func (g *gregorHandler) handleOutOfBandMessage(ctx context.Context, obm gregor.OutOfBandMessage) error {
	g.Debug("handleOutOfBand: %+v", obm)

	if obm.System() == nil {
		return errors.New("nil system in out of band message")
	}

	if tmp, ok := obm.(gregor1.OutOfBandMessage); ok {
		g.pushOutOfBandMessages([]gregor1.OutOfBandMessage{tmp})
	} else {
		g.G().Log.Warning("Got non-exportable out-of-band message")
	}

	switch obm.System().String() {
	case "kbfs.favorites":
		return g.kbfsFavorites(ctx, obm)
	case "chat.activity":
		return g.newChatActivity(ctx, obm)
	case "chat.tlffinalize":
		return g.chatTlfFinalize(ctx, obm)
	case "internal.reconnect":
		g.G().Log.Debug("reconnected to push server")
		return nil
	default:
		return fmt.Errorf("unhandled system: %s", obm.System())
	}
}

func (g *gregorHandler) Shutdown() {
	g.G().Log.Debug("gregor shutdown")
	g.connMutex.Lock()
	defer g.connMutex.Unlock()

	if g.conn == nil {
		return
	}
	close(g.shutdownCh)
	g.conn.Shutdown()
	g.conn = nil
}

func (g *gregorHandler) Reset() error {
	g.Shutdown()
	return g.resetGregorClient()
}

func (g *gregorHandler) kbfsFavorites(ctx context.Context, m gregor.OutOfBandMessage) error {
	if m.Body() == nil {
		return errors.New("gregor handler for kbfs.favorites: nil message body")
	}
	body, err := jsonw.Unmarshal(m.Body().Bytes())
	if err != nil {
		return err
	}

	action, err := body.AtPath("action").GetString()
	if err != nil {
		return err
	}

	switch action {
	case "create", "delete":
		return g.notifyFavoritesChanged(ctx, m.UID())
	default:
		return fmt.Errorf("unhandled kbfs.favorites action %q", action)
	}
}

func (g *gregorHandler) notifyFavoritesChanged(ctx context.Context, uid gregor.UID) error {
	kbUID, err := keybase1.UIDFromString(hex.EncodeToString(uid.Bytes()))
	if err != nil {
		return err
	}
	g.G().NotifyRouter.HandleFavoritesChanged(kbUID)
	return nil
}

func (g *gregorHandler) chatTlfFinalize(ctx context.Context, m gregor.OutOfBandMessage) error {
	if m.Body() == nil {
		return errors.New("gregor handler for chat.tlffinalize: nil message body")
	}

	g.G().Log.Debug("push handler: tlf finalize received")

	var update chat1.TLFFinalizeUpdate
	reader := bytes.NewReader(m.Body().Bytes())
	dec := codec.NewDecoder(reader, &codec.MsgpackHandle{WriteExt: true})
	err := dec.Decode(&update)
	if err != nil {
		return err
	}

	// Update inbox
	if err := g.G().InboxSource.TlfFinalize(ctx, m.UID().Bytes(), update.InboxVers, update.ConvIDs,
		update.FinalizeInfo); err != nil {
		g.G().Log.Error("push handler: tlf finalize: unable to update inbox: %s", err.Error())
	}

	// Send notify for each conversation ID
	uid := m.UID().String()
	for _, convID := range update.ConvIDs {
		g.G().NotifyRouter.HandleChatTLFFinalize(context.Background(), keybase1.UID(uid),
			convID, update.FinalizeInfo)
	}

	return nil
}

func (g *gregorHandler) newChatActivity(ctx context.Context, m gregor.OutOfBandMessage) error {
	if m.Body() == nil {
		return errors.New("gregor handler for chat.activity: nil message body")
	}

	var activity chat1.ChatActivity
	var gm chat1.GenericPayload
	reader := bytes.NewReader(m.Body().Bytes())
	dec := codec.NewDecoder(reader, &codec.MsgpackHandle{WriteExt: true})
	err := dec.Decode(&gm)
	if err != nil {
		return err
	}

	g.G().Log.Debug("push handler: chat activity: action %s", gm.Action)

	var identBreaks []keybase1.TLFIdentifyFailure
	ctx = chat.Context(ctx, keybase1.TLFIdentifyBehavior_CHAT_GUI, &identBreaks, g.identNotifier)

	action := gm.Action
	reader.Reset(m.Body().Bytes())
	switch action {
	case "newMessage":
		var nm chat1.NewMessagePayload
		err = dec.Decode(&nm)
		if err != nil {
			g.G().Log.Error("push handler: chat activity: error decoding newMessage: %s", err.Error())
			return err
		}

		g.G().Log.Debug("push handler: chat activity: newMessage: convID: %s sender: %s",
			nm.ConvID, nm.Message.ClientHeader.Sender)
		if nm.Message.ClientHeader.OutboxID != nil {
			g.G().Log.Debug("push handler: chat activity: newMessage: outboxID: %s",
				hex.EncodeToString(*nm.Message.ClientHeader.OutboxID))
		} else {
			g.G().Log.Debug("push handler: chat activity: newMessage: outboxID is empty")
		}
		uid := m.UID().Bytes()

		decmsg, append, err := g.G().ConvSource.Push(ctx, nm.ConvID, gregor1.UID(uid), nm.Message)
		if err != nil {
			g.G().Log.Error("push handler: chat activity: unable to storage message: %s", err.Error())
		}
		if err = g.G().InboxSource.NewMessage(ctx, uid, nm.InboxVers, nm.ConvID, nm.Message); err != nil {
			g.G().Log.Error("push handler: chat activity: unable to update inbox: %s", err.Error())
		}

		activity = chat1.NewChatActivityWithIncomingMessage(chat1.IncomingMessage{
			Message: decmsg,
			ConvID:  nm.ConvID,
		})

		// If this message was not "appended", meaning there is a hole between what we have in cache,
		// and this message, then we send out a notification that this thread should be considered
		// stale
		if !append {
			g.G().Log.Debug("push handler: chat activity: newMessage: non-append message, alerting")
			kuid := keybase1.UID(m.UID().String())
			g.G().NotifyRouter.HandleChatThreadsStale(context.Background(), kuid,
				[]chat1.ConversationID{nm.ConvID})
		}

		if g.badger != nil && nm.UnreadUpdate != nil {
			g.badger.PushChatUpdate(*nm.UnreadUpdate, nm.InboxVers)
		}
	case "readMessage":
		var nm chat1.ReadMessagePayload
		err = dec.Decode(&nm)
		if err != nil {
			g.G().Log.Error("push handler: chat activity: error decoding: %s", err.Error())
			return err
		}
		g.G().Log.Debug("push handler: chat activity: readMessage: convID: %s msgID: %d",
			nm.ConvID, nm.MsgID)

		uid := m.UID().Bytes()
		if err = g.G().InboxSource.ReadMessage(ctx, uid, nm.InboxVers, nm.ConvID, nm.MsgID); err != nil {
			g.G().Log.Error("push handler: chat activity: unable to update inbox: %s", err.Error())
		}

		activity = chat1.NewChatActivityWithReadMessage(chat1.ReadMessageInfo{
			MsgID:  nm.MsgID,
			ConvID: nm.ConvID,
		})

		if g.badger != nil && nm.UnreadUpdate != nil {
			g.badger.PushChatUpdate(*nm.UnreadUpdate, nm.InboxVers)
		}
	case "setStatus":
		var nm chat1.SetStatusPayload
		err = dec.Decode(&nm)
		if err != nil {
			g.G().Log.Error("push handler: chat activity: error decoding: %s", err.Error())
			return err
		}
		g.G().Log.Debug("push handler: chat activity: setStatus: convID: %s status: %d",
			nm.ConvID, nm.Status)

		uid := m.UID().Bytes()
		if err = g.G().InboxSource.SetStatus(ctx, uid, nm.InboxVers, nm.ConvID, nm.Status); err != nil {
			g.G().Log.Error("push handler: chat activity: unable to update inbox: %s", err.Error())
		}
		activity = chat1.NewChatActivityWithSetStatus(chat1.SetStatusInfo{
			ConvID: nm.ConvID,
			Status: nm.Status,
		})

		if g.badger != nil && nm.UnreadUpdate != nil {
			g.badger.PushChatUpdate(*nm.UnreadUpdate, nm.InboxVers)
		}
	case "newConversation":
		var nm chat1.NewConversationPayload
		err = dec.Decode(&nm)
		if err != nil {
			g.G().Log.Error("push handler: chat activity: error decoding: %s", err.Error())
			return err
		}
		g.G().Log.Debug("push handler: chat activity: newConversation: convID: %s ", nm.ConvID)

		uid := m.UID().Bytes()

		// We need to get this conversation and then localize it
		var inbox chat1.Inbox
		if inbox, _, err = g.G().InboxSource.ReadRemote(ctx, uid, nil, &chat1.GetInboxLocalQuery{
			ConvID: &nm.ConvID,
		}, nil); err != nil {
			g.G().Log.Error("push handler: chat activity: unable to read conversation: %s", err.Error())
			return err
		}
		if len(inbox.Convs) != 1 {
			g.G().Log.Error("push handler: chat activity: unable to find conversation")
			return fmt.Errorf("unable to find conversation")
		}
		updateConv := inbox.ConvsUnverified[0]
		if err = g.G().InboxSource.NewConversation(ctx, uid, nm.InboxVers, updateConv); err != nil {
			g.G().Log.Error("push handler: chat activity: unable to update inbox: %s", err.Error())
		}

		activity = chat1.NewChatActivityWithNewConversation(chat1.NewConversationInfo{
			Conv: inbox.Convs[0],
		})

		if g.badger != nil && nm.UnreadUpdate != nil {
			g.badger.PushChatUpdate(*nm.UnreadUpdate, nm.InboxVers)
		}
	default:
		return fmt.Errorf("unhandled chat.activity action %q", action)
	}

	return g.notifyNewChatActivity(ctx, m.UID(), &activity)
}

func (g *gregorHandler) notifyNewChatActivity(ctx context.Context, uid gregor.UID, activity *chat1.ChatActivity) error {
	kbUID, err := keybase1.UIDFromString(hex.EncodeToString(uid.Bytes()))
	if err != nil {
		return err
	}
	g.G().NotifyRouter.HandleNewChatActivity(ctx, kbUID, activity)
	return nil
}

func (g *gregorHandler) auth(ctx context.Context, cli rpc.GenericClient) (err error) {
	var token string
	var uid keybase1.UID

	// Check to see if we have been shutdown,
	select {
	case <-g.shutdownCh:
		g.Debug("server is dead, not authenticating")
		return errors.New("server is dead, not authenticating")
	default:
		// if we were going to block, then that means we are still alive
	}

	// Continue on and authenticate
	aerr := g.G().LoginState().LocalSession(func(s *libkb.Session) {
		token = s.GetToken()
		uid = s.GetUID()
	}, "gregor handler - login session")
	if token == "" {
		return errors.New("blank session token would have been sent to gregor")
	}
	if aerr != nil {
		g.skipRetryConnect = true
		return aerr
	}
	g.Debug("have session token")

	g.Debug("authenticating")
	ac := gregor1.AuthClient{Cli: cli}
	auth, err := ac.AuthenticateSessionToken(ctx, gregor1.SessionToken(token))
	if err != nil {
		g.Debug("auth error: %s", err)
		return err
	}

	g.Debug("auth result: %+v", auth)
	if !bytes.Equal(auth.Uid, uid.ToBytes()) {
		g.skipRetryConnect = true
		return fmt.Errorf("auth result uid %x doesn't match session uid %q", auth.Uid, uid)
	}
	g.sessionID = auth.Sid

	return nil
}

func (g *gregorHandler) pingLoop() {

	duration := g.G().Env.GetGregorPingInterval()
	timeout := g.G().Env.GetGregorPingTimeout()
	g.Debug("ping loop: starting up: duration: %v timeout: %v", duration, timeout)
	defer g.Debug("ping loop: terminating")

	for {
		select {
		case <-g.G().Clock().After(duration):

			if !g.IsConnected() {
				g.Debug("ping loop: skipping ping since not connected")
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			_, err := gregor1.IncomingClient{Cli: g.cli}.Ping(ctx)
			cancel()
			if err != nil {
				if err == ErrGregorTimeout {
					g.Debug("ping loop: timeout: terminating connection")
					g.Shutdown()

					if err := g.Connect(g.uri); err != nil {
						g.Debug("ping loop: error connecting: %s", err.Error())
					}
				}
			}

		case <-g.shutdownCh:
			return
		}
	}

}

func (g *gregorHandler) connectTLS() error {
	uri := g.uri
	g.Debug("connecting to gregord via TLS at %s", uri)
	rawCA := g.G().Env.GetBundledCA(uri.Host)
	if len(rawCA) == 0 {
		return fmt.Errorf("No bundled CA for %s", uri.Host)
	}
	g.Debug("Using CA for gregor: %s", libkb.ShortCA(rawCA))

	g.connMutex.Lock()
	opts := rpc.ConnectionOpts{
		WrapErrorFunc:    libkb.WrapError,
		ReconnectBackoff: backoff.NewConstantBackOff(GregorConnectionRetryInterval),
	}
	g.conn = rpc.NewTLSConnection(uri.HostPort, []byte(rawCA), libkb.ErrorUnwrapper{}, g, libkb.NewRPCLogFactory(g.G()), g.G().Log, opts)
	g.connMutex.Unlock()

	// The client we get here will reconnect to gregord on disconnect if necessary.
	// We should grab it here instead of in OnConnect, since the connection is not
	// fully established in OnConnect. Anything that wants to make calls outside
	// of OnConnect should use g.cli, everything else should the client that is
	// a paramater to OnConnect
	g.cli = WrapGenericClientWithTimeout(g.conn.GetClient(), GregorRequestTimeout, ErrGregorTimeout)

	// Start up ping loop to keep the connection to gregord alive, and to kick
	// off the reconnect logic in the RPC library
	g.startPingLoop.Do(func() { go g.pingLoop() })

	return nil
}

func (g *gregorHandler) connectNoTLS() error {
	uri := g.uri
	g.Debug("connecting to gregord without TLS at %s", uri)
	t := newConnTransport(g.G(), uri.HostPort)
	g.transportForTesting = t
	g.connMutex.Lock()
	opts := rpc.ConnectionOpts{
		WrapErrorFunc:    libkb.WrapError,
		ReconnectBackoff: backoff.NewConstantBackOff(GregorConnectionRetryInterval),
	}
	g.conn = rpc.NewConnectionWithTransport(g, t, libkb.ErrorUnwrapper{}, g.G().Log, opts)
	g.connMutex.Unlock()
	g.cli = WrapGenericClientWithTimeout(g.conn.GetClient(), GregorRequestTimeout, ErrGregorTimeout)

	// Start up ping loop to keep the connection to gregord alive, and to kick
	// off the reconnect logic in the RPC library
	g.startPingLoop.Do(func() { go g.pingLoop() })

	return nil
}

func NewGregorMsgID() (gregor1.MsgID, error) {
	r, err := libkb.RandBytes(16) // TODO: Create a shared function for this.
	if err != nil {
		return nil, err
	}
	return gregor1.MsgID(r), nil
}

func (g *gregorHandler) templateMessage() (*gregor1.Message, error) {
	uid := g.G().Env.GetUID()
	if uid.IsNil() {
		return nil, fmt.Errorf("Can't create new gregor items without a current UID.")
	}
	gregorUID := gregor1.UID(uid.ToBytes())

	newMsgID, err := NewGregorMsgID()
	if err != nil {
		return nil, err
	}

	return &gregor1.Message{
		Ibm_: &gregor1.InBandMessage{
			StateUpdate_: &gregor1.StateUpdateMessage{
				Md_: gregor1.Metadata{
					Uid_:   gregorUID,
					MsgID_: newMsgID,
				},
			},
		},
	}, nil
}

func (g *gregorHandler) DismissItem(id gregor.MsgID) error {
	if id == nil {
		return nil
	}
	var err error
	defer g.G().Trace(fmt.Sprintf("gregorHandler.DismissItem(%s)", id.String()),
		func() error { return err },
	)()

	dismissal, err := g.templateMessage()
	if err != nil {
		return err
	}

	dismissal.Ibm_.StateUpdate_.Dismissal_ = &gregor1.Dismissal{
		MsgIDs_: []gregor1.MsgID{gregor1.MsgID(id.Bytes())},
	}

	incomingClient := gregor1.IncomingClient{Cli: g.cli}
	// TODO: Should the interface take a context from the caller?
	err = incomingClient.ConsumeMessage(context.TODO(), *dismissal)
	return err
}

func (g *gregorHandler) InjectItem(cat string, body []byte) (gregor.MsgID, error) {
	var err error
	defer g.G().Trace(fmt.Sprintf("gregorHandler.InjectItem(%s)", cat),
		func() error { return err },
	)()

	creation, err := g.templateMessage()
	if err != nil {
		return nil, err
	}
	creation.Ibm_.StateUpdate_.Creation_ = &gregor1.Item{
		Category_: gregor1.Category(cat),
		Body_:     gregor1.Body(body),
	}

	incomingClient := gregor1.IncomingClient{Cli: g.cli}
	// TODO: Should the interface take a context from the caller?
	err = incomingClient.ConsumeMessage(context.TODO(), *creation)
	return creation.Ibm_.StateUpdate_.Md_.MsgID_, err
}

func (g *gregorHandler) InjectOutOfBandMessage(system string, body []byte) error {
	var err error
	defer g.G().Trace(fmt.Sprintf("gregorHandler.InjectOutOfBandMessage(%s)", system),
		func() error { return err },
	)()

	uid := g.G().Env.GetUID()
	if uid.IsNil() {
		err = fmt.Errorf("Can't create new gregor items without a current UID.")
		return err
	}
	gregorUID := gregor1.UID(uid.ToBytes())

	msg := gregor1.Message{
		Oobm_: &gregor1.OutOfBandMessage{
			Uid_:    gregorUID,
			System_: gregor1.System(system),
			Body_:   gregor1.Body(body),
		},
	}

	incomingClient := gregor1.IncomingClient{Cli: g.cli}
	// TODO: Should the interface take a context from the caller?
	err = incomingClient.ConsumeMessage(context.TODO(), msg)
	return err
}

func (g *gregorHandler) simulateCrashForTesting() {
	g.transportForTesting.Reset()
	gregor1.IncomingClient{Cli: g.cli}.Ping(context.Background())
}

type gregorRPCHandler struct {
	libkb.Contextified
	xp rpc.Transporter
	gh *gregorHandler
}

func newGregorRPCHandler(xp rpc.Transporter, g *libkb.GlobalContext, gh *gregorHandler) *gregorRPCHandler {
	return &gregorRPCHandler{
		Contextified: libkb.NewContextified(g),
		xp:           xp,
		gh:           gh,
	}
}

func (g *gregorHandler) getState() (res gregor1.State, err error) {
	var s gregor.State

	if g == nil || g.gregorCli == nil {
		return res, errors.New("gregor service not available (are you in standalone?)")
	}

	s, err = g.gregorCli.StateMachineState(nil)
	if err != nil {
		return res, err
	}

	ps, err := s.Export()
	if err != nil {
		return res, err
	}

	var ok bool
	if res, ok = ps.(gregor1.State); !ok {
		return res, errors.New("failed to convert state to exportable format")
	}

	return res, nil
}

func (g *gregorRPCHandler) GetState(_ context.Context) (res gregor1.State, err error) {
	return g.gh.getState()
}

func WrapGenericClientWithTimeout(client rpc.GenericClient, timeout time.Duration, timeoutErr error) rpc.GenericClient {
	return &timeoutClient{client, timeout, timeoutErr}
}

type timeoutClient struct {
	inner      rpc.GenericClient
	timeout    time.Duration
	timeoutErr error
}

var _ rpc.GenericClient = (*timeoutClient)(nil)

func (t *timeoutClient) Call(ctx context.Context, method string, arg interface{}, res interface{}) error {
	var timeoutCancel context.CancelFunc
	ctx, timeoutCancel = context.WithTimeout(ctx, t.timeout)
	defer timeoutCancel()
	err := t.inner.Call(ctx, method, arg, res)
	if err == context.DeadlineExceeded {
		return t.timeoutErr
	}
	return err
}

func (t *timeoutClient) Notify(ctx context.Context, method string, arg interface{}) error {
	var timeoutCancel context.CancelFunc
	ctx, timeoutCancel = context.WithTimeout(ctx, t.timeout)
	defer timeoutCancel()
	err := t.inner.Notify(ctx, method, arg)
	if err == context.DeadlineExceeded {
		return t.timeoutErr
	}
	return err
}
