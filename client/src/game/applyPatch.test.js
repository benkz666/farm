import test from 'node:test'
import assert from 'node:assert/strict'

import { PLOT, defaultState } from './state.js'
import { applyFarmDelta, applyPatch, plotStateFromNum } from './applyPatch.js'

test('未知服务端地块状态不会伪装成荒地', () => {
  assert.equal(plotStateFromNum(99), PLOT.UNKNOWN)
})

test('EnterFarm 使用服务端下发的权威时间档', () => {
  const state = defaultState()
  applyPatch(state, {
    time_profile: 'authentic',
    time_profile_mutable: true,
    snapshot: { owner_uid: 1, unlocked_plots: 6, plots: [] },
  })
  assert.equal(state.timeScale, 'authentic')
  assert.equal(state.timeScaleMutable, true)

  applyPatch(state, { time_profile: 'unknown', time_profile_mutable: false, farm_seq: 1 })
  assert.equal(state.timeScale, 'authentic')
  assert.equal(state.timeScaleMutable, false)
})

test('地块快照应用服务端健康度与真实本季时间基准', () => {
  const state = defaultState()
  applyFarmDelta(state, {
    owner_uid: 42,
    farm_seq: 8,
    plots: [{
      index: 0,
      state: 2,
      crop_id: 1,
      season_start_at: 1_000,
      mature_at: 11_000,
      season_duration: 10_000,
      last_settle_at: 4_000,
      last_water_at: 1_000,
      health: 73,
    }],
  })

  const plot = state.plots[0]
  assert.equal(plot.plantTime, 1_000)
  assert.equal(plot.settleTime, 4_000)
  assert.equal(plot.health, 73)
  assert.equal(plot.penalty, 27)
  assert.equal(plot.waterUntil, 4_500)
})

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

test('SELF 农场快照保留超过 2^53 的金币精度', () => {
  const state = defaultState()
  applyPatch(state, {
    relation: 'SELF',
    snapshot: {
      owner_uid: '1785402171458126005',
      coin: '9007199254740993',
      unlocked_plots: 6,
      plots: [],
    },
  })
  assert.equal(state.gold, '9007199254740993')
})

test('收获补丁写入单作物图鉴次数和牌子阶段', () => {
  const state = defaultState()

  applyPatch(state, {
    patch: {
      codex_progress: {
        crop_id: 1,
        harvest_count: 20,
        tier: 'silver',
        next_target: 50,
      },
    },
  })

  assert.deepEqual(state.codex, ['bailuobo'])
  assert.deepEqual(state.codexProgress.bailuobo, {
    harvestCount: 20,
    tier: 'silver',
    nextTarget: 50,
  })
})
