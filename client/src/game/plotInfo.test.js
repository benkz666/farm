import test from 'node:test'
import assert from 'node:assert/strict'

import { CROP_MAP } from './config.js'
import { PLOT, makePlot } from './state.js'
import { computePlotInfo, cropOf, stageOf } from './plotInfo.js'

function makeWitheredPlot(cropId) {
  const p = makePlot(0)
  p.state = PLOT.WITHERED
  p.cropId = cropId
  return p
}

test('WITHERED 地块 crop_id 被清空（null）时不抛错且渲染为枯萎态', () => {
  const plot = makeWitheredPlot(null)
  const now = Date.now()
  const info = computePlotInfo(plot, { unlocked: true, index: 0, now })
  assert.equal(info.state, PLOT.WITHERED)
  assert.equal(info.cropDef, null, '缺少作物定义时 cropDef 必须为 null 而非 undefined')
  assert.equal(info.stage, 0)
  assert.equal(info.totalStages, 3)
})

test('WITHERED 地块 crop_id=0（服务端清空值）同样安全且 cropDef===null', () => {
  // applyPatch 当前会把服务端 crop_id=0 规范化为 null（cropIdToKey(0) === null），
  // 但 computePlotInfo 必须在直接收到 0 时也安全：CROP_MAP[0] 为 undefined，
  // 经 `?? null` 规整为 null，且不进入 stageOf 崩溃路径。
  const plot = makeWitheredPlot(0)
  assert.equal(plot.cropId, 0, '前置：确保真实传入数字 0 而非 null')
  const info = computePlotInfo(plot, { unlocked: true, index: 0, now: Date.now() })
  assert.equal(info.state, PLOT.WITHERED)
  assert.equal(info.cropDef, null, 'crop_id=0 时 cropDef 必须规整为 null 而非 undefined')
  assert.equal(info.stage, 0)
  assert.equal(info.totalStages, 3)
})

test('GROWING 有效作物仍按 stageOf 计算阶段（不改变有效 crop 显示）', () => {
  const plot = makePlot(0)
  plot.state = PLOT.GROWING
  plot.cropId = 'xiaomai'
  plot.plantTime = Date.now() - 1000
  plot.seasonMs = 10000
  const now = Date.now()
  const info = computePlotInfo(plot, { unlocked: true, index: 0, now })
  assert.equal(info.cropDef, CROP_MAP.xiaomai)
  const expected = stageOf(plot, now)
  assert.equal(info.totalStages, expected.total)
  assert.ok(info.stage >= 0 && info.stage < info.totalStages)
})

test('cropOf 对 crop_id=null 返回 undefined（确认根因入口）', () => {
  assert.equal(cropOf(makeWitheredPlot(null)), undefined)
})
