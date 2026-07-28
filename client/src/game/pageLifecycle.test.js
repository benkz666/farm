import test from 'node:test'
import assert from 'node:assert/strict'

import { bindPageUnload } from './pageLifecycle.js'

function makeEnv() {
  const listeners = new Map()
  return {
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set())
      listeners.get(type).add(fn)
    },
    removeEventListener(type, fn) {
      listeners.get(type)?.delete(fn)
    },
    emit(type, ev) {
      for (const fn of [...(listeners.get(type) || [])]) fn(ev)
    },
    listenerCount(type) {
      return listeners.get(type)?.size || 0
    },
    hasListener(type, fn) {
      return listeners.get(type)?.has(fn) || false
    },
  }
}

test('pagehide：清 tick interval、移除 pointermove、dispose scene，并保留网络关闭', () => {
  const env = makeEnv()
  let intervalCleared = null
  const pointerMove = () => {}
  const removed = []
  const sceneDisposed = []
  const reconnectDisposed = []
  const netClosed = []
  let reconnectBinding = { dispose: () => reconnectDisposed.push(1) }

  env.addEventListener('pointermove', pointerMove)

  bindPageUnload({
    addEventListener: env.addEventListener,
    removeEventListener: (type, fn) => {
      removed.push([type, fn])
      env.removeEventListener(type, fn)
    },
    clearInterval: (id) => { intervalCleared = id },
    tickIntervalId: 42,
    onPointerMove: pointerMove,
    getReconnectBinding: () => reconnectBinding,
    setReconnectBinding: (v) => { reconnectBinding = v },
    scene: { dispose: () => sceneDisposed.push(1) },
    getNetClient: () => ({ close: () => netClosed.push(1) }),
  })

  assert.equal(env.hasListener('pointermove', pointerMove), true)
  env.emit('pagehide')

  assert.equal(intervalCleared, 42)
  assert.ok(removed.some(([t, fn]) => t === 'pointermove' && fn === pointerMove))
  assert.equal(env.hasListener('pointermove', pointerMove), false)
  assert.deepEqual(sceneDisposed, [1])
  assert.deepEqual(reconnectDisposed, [1])
  assert.equal(reconnectBinding, null)
  assert.deepEqual(netClosed, [1])
})


test('pagehide 幂等：重复触发不二次 dispose / close', () => {
  const env = makeEnv()
  const sceneDisposed = []
  const netClosed = []
  let reconnectBinding = { dispose: () => {} }

  bindPageUnload({
    addEventListener: env.addEventListener,
    removeEventListener: env.removeEventListener,
    clearInterval: () => {},
    tickIntervalId: 1,
    onPointerMove: () => {},
    getReconnectBinding: () => reconnectBinding,
    setReconnectBinding: (v) => { reconnectBinding = v },
    scene: { dispose: () => sceneDisposed.push(1) },
    getNetClient: () => ({ close: () => netClosed.push(1) }),
  })

  env.emit('pagehide')
  env.emit('pagehide')

  assert.deepEqual(sceneDisposed, [1])
  assert.deepEqual(netClosed, [1])
})
