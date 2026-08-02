import test from 'node:test'
import assert from 'node:assert/strict'

import { createFarmAdvanceScheduler, nextFarmSyncAt } from './farmAdvanceScheduler.js'
import { PLOT, makePlot } from './state.js'

function growingPlot() {
  const plot = makePlot(0)
  plot.state = PLOT.GROWING
  plot.plantTime = 1_000
  plot.seasonMs = 10_000
  plot.matureTime = 11_000
  return plot
}

test('下一次同步只选择最近的风险窗口和成熟点', () => {
  const growing = growingPlot()
  assert.equal(nextFarmSyncAt([growing], 1_500), 2_000)
  assert.equal(nextFarmSyncAt([growing], 10_500), 11_000)
  assert.equal(nextFarmSyncAt([growing], 11_000), 11_000)

  const mature = growingPlot()
  mature.state = PLOT.MATURE
  assert.equal(nextFarmSyncAt([mature], 20_000), 0)
  assert.equal(nextFarmSyncAt([mature], 41_000), 0)
})

test('调度器到成熟边界只触发一次权威同步，成熟后停止排程', async () => {
  let now = 1_500
  let timer = null
  let syncCalls = 0
  const plot = growingPlot()
  const scheduler = createFarmAdvanceScheduler({
    now: () => now,
    getPlots: () => [plot],
    isActive: () => true,
    setTimeout(fn, delay) {
      timer = { fn, delay }
      return 1
    },
    clearTimeout() {
      timer = null
    },
    async sync() {
      syncCalls++
      plot.state = PLOT.MATURE
      now = 11_000
    },
  })

  scheduler.schedule()
  assert.equal(timer.delay, 500)
  const runBoundary = timer.fn
  timer = null
  await runBoundary()
  assert.equal(syncCalls, 1)
  assert.equal(timer, null)
  scheduler.dispose()
  assert.equal(timer, null)
})

test('好友农场即使没有生长边界也会周期校准，最近的生长边界优先', async () => {
  let now = 20_000
  let timer = null
  let syncCalls = 0
  const mature = growingPlot()
  mature.state = PLOT.MATURE
  const plots = [mature]
  const scheduler = createFarmAdvanceScheduler({
    now: () => now,
    getPlots: () => plots,
    isActive: () => true,
    getConsistencyIntervalMs: () => 3_000,
    setTimeout(fn, delay) {
      timer = { fn, delay }
      return 1
    },
    clearTimeout() {
      timer = null
    },
    async sync() {
      syncCalls++
      now += 3_000
    },
  })

  scheduler.schedule()
  assert.equal(timer.delay, 3_000)
  const runConsistencyCheck = timer.fn
  timer = null
  await runConsistencyCheck()
  assert.equal(syncCalls, 1)
  assert.equal(timer.delay, 3_000)

  const growing = growingPlot()
  growing.plantTime = now
  growing.seasonMs = 10_000
  growing.matureTime = now + 10_000
  plots.splice(0, 1, growing)
  scheduler.schedule()
  assert.equal(timer.delay, 1_000, '风险窗口早于 3 秒校准周期时应优先同步')
})

test('页面恢复时可立即校准并重新建立周期排程', async () => {
  let timer = null
  let syncCalls = 0
  const scheduler = createFarmAdvanceScheduler({
    now: () => 10_000,
    getPlots: () => [],
    isActive: () => true,
    getConsistencyIntervalMs: () => 3_000,
    setTimeout(fn, delay) {
      timer = { fn, delay }
      return 1
    },
    clearTimeout() {
      timer = null
    },
    async sync() {
      syncCalls++
    },
  })

  scheduler.schedule()
  assert.equal(timer.delay, 3_000)
  await scheduler.reconcileNow()
  assert.equal(syncCalls, 1)
  assert.equal(timer.delay, 3_000)
})
