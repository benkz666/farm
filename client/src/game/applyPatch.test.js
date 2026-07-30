import test from 'node:test'
import assert from 'node:assert/strict'

import { PLOT, defaultState } from './state.js'
import { applyFarmDelta, applyPatch } from './applyPatch.js'

test('FarmDelta 只投影变更地块', () => {
  const state = defaultState()
  state.plots[1].state = PLOT.GROWING

  applyFarmDelta(state, {
    owner_uid: 42,
    farm_seq: 7,
    plots: [{ index: 0, state: 1, crop_id: 0 }],
  })

  assert.equal(state.plots[0].state, PLOT.TILLED)
  assert.equal(state.plots[1].state, PLOT.GROWING)
})

test('FRIEND 农场快照只投影地块，不覆盖访客自己的金币/经验/背包', () => {
  const state = defaultState()
  state.gold = 333
  state.exp = 757
  state.inventory.seeds = { bailuobo: 2 }
  state.warehouse = { bailuobo: 5 }
  state.unlockedPlots = 6

  applyPatch(
    state,
    {
      relation: 'FRIEND',
      snapshot: {
        owner_uid: 99,
        coin: 99999,
        exp: 12,
        unlocked_plots: 8,
        bag: { 'seed:1': 99 },
        warehouse: { 'fruit:1': 88 },
        plots: [{ index: 0, state: 1, crop_id: 0 }],
      },
    },
    { farmViewOnly: true },
  )

  assert.equal(state.gold, 333)
  assert.equal(state.exp, 757)
  assert.deepEqual(state.inventory.seeds, { bailuobo: 2 })
  assert.deepEqual(state.warehouse, { bailuobo: 5 })
  assert.equal(state.unlockedPlots, 8)
  assert.equal(state.plots[0].state, PLOT.TILLED)
})

test('SELF 农场快照仍权威覆盖金币', () => {
  const state = defaultState()
  state.gold = 1
  applyPatch(state, {
    relation: 'SELF',
    snapshot: { owner_uid: 1, coin: 4242, unlocked_plots: 6, plots: [] },
  })
  assert.equal(state.gold, 4242)
})
