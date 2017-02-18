// @flow
import {apiserverGetRpc} from '../constants/types/flow-types'
import {throttle} from 'lodash'

type URLMap = {[key: string]: string}
type Info = {
  urlMap: ?URLMap,
  callbacks: Array<(username: string, urlMap: ?URLMap) => void>,
  done: boolean,
  error: boolean,
}

// Done
const _usernameToURL: {[key: string]: ?Info} = {}
// Not done
const _pendingUsernameToURL: {[key: string]: ?Info} = {}

const _getUserImages = throttle(() => {
  const usersToResolve = Object.keys(_pendingUsernameToURL)
  if (!usersToResolve.length) {
    return
  }

  // Move pending to non-pending state
  usersToResolve.forEach(username => {
    const info: ?Info = _pendingUsernameToURL[username]
    _usernameToURL[username] = info
    delete _pendingUsernameToURL[username]
  })

  apiserverGetRpc({
    callback: (error, response) => {
      if (error) {
        usersToResolve.forEach(username => {
          const info = _usernameToURL[username]
          const urlMap = null
          if (info) {
            info.done = true
            info.error = true
            info.callbacks.forEach(cb => cb(username, urlMap))
            info.callbacks = []
          }
        })
      } else {
        JSON.parse(response.body).pictures.forEach((picMap, idx) => {
          const username = usersToResolve[idx]
          const urlMap = {
            '200': picMap['square_200'],
            '360': picMap['square_360'],
            '40': picMap['square_40'],
          }
          const info = _usernameToURL[username]
          if (info) {
            info.done = true
            info.urlMap = urlMap
            info.callbacks.forEach(cb => cb(username, urlMap))
            info.callbacks = []
          }
        })
      }
    },
    param: {
      args: [
        {key: 'usernames', value: usersToResolve.join(',')},
        {key: 'formats', value: 'square_360,square_200,square_40'},
      ],
      endpoint: 'image/username_pic_lookups',
    },
  })
}, 200)

function validUsername (name: ?string) {
  if (!name) {
    return false
  }

  return !!name.match(/^([a-z0-9][a-z0-9_]{1,15})$/i)
}

export function getUserImageMap (username: string): ?URLMap {
  if (!validUsername(username)) {
    return null
  }

  const info = _usernameToURL[username]
  return info ? info.urlMap : null
}

export function loadUserImageMap (username: string, callback: (username: string, urlMap: ?URLMap) => void) {
  if (!validUsername(username)) {
    // set immediate so its always async
    setImmediate(() => {
      callback(username, {})
    })
    return
  }

  const info = _usernameToURL[username] || _pendingUsernameToURL[username]
  if (info) {
    if (!info.done) {
      info.callbacks.push(callback)
    } else {
      setImmediate(() => {
        callback(username, info.urlMap)
      })
    }
  } else {
    _pendingUsernameToURL[username] = {
      callbacks: [callback],
      done: false,
      error: false,
      requested: false,
      urlMap: null,
    }
    _getUserImages()
  }
}
