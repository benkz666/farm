import test from 'node:test'
import assert from 'node:assert/strict'

import { clearSave, defaultState, loadGame, saveGame } from './state.js'

const SAVE_KEY = 'farm3d_save_v1'

function mockLocalStorage() {
  const store = new Map()
  globalThis.localStorage = {
    getItem(key) {
      return store.has(key) ? store.get(key) : null
    },
    setItem(key, value) {
      store.set(key, String(value))
    },
    removeItem(key) {
      store.delete(key)
    },
  }
  return store
}

test('defaultState 不含 NPC 假好友', () => {
  const state = defaultState()
  assert.deepEqual(state.friends, [])
})

test('saveGame 不再写入 localStorage 权威存档', () => {
  const store = mockLocalStorage()
  const state = defaultState()
  state.gold = 9999
  state.plots[0].state = 'tilled'
  saveGame(state)
  assert.equal(store.has(SAVE_KEY), false)
  assert.equal(loadGame(), null)
})

test('loadGame 忽略遗留权威存档', () => {
  const store = mockLocalStorage()
  store.set(
    SAVE_KEY,
    JSON.stringify({
      ...defaultState(),
      version: 1,
      gold: 4242,
      friends: [{ id: 'npc1', name: '小芳', plots: [] }],
    }),
  )
  assert.equal(loadGame(), null)
  clearSave()
  assert.equal(store.has(SAVE_KEY), false)
})
