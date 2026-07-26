import test from 'node:test'
import assert from 'node:assert/strict'

import { PLOT, defaultState } from './state.js'
import { applyFarmDelta } from './applyPatch.js'

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
