import test from 'node:test'
import assert from 'node:assert/strict'

import { CROPS, CROP_MAP, seasonMinutes, seasonDurationMs, TIME_SCALES } from './config.js'
import { CROP_ID_TO_KEY, CROP_KEY_TO_ID, cropIdToKey, cropKeyToId } from './applyPatch.js'

test('CROPS 含设计 29 种且 slug 唯一', () => {
  assert.equal(CROPS.length, 29)
  const slugs = new Set(CROPS.map((c) => c.id))
  assert.equal(slugs.size, 29)
  assert.ok(CROP_MAP.bailuobo)
  assert.ok(CROP_MAP.xiaomai)
  assert.ok(CROP_MAP.pingguo)
  assert.ok(CROP_MAP.yaoqianshu)
})

test('展示顺序与 numeric ID 解耦：小麦第4项/苹果第15项，但 pingguo=4', () => {
  assert.equal(CROPS[3].id, 'xiaomai', '展示第 4 项应为小麦')
  assert.equal(CROPS[14].id, 'pingguo', '展示第 15 项应为苹果')
  assert.equal(CROP_KEY_TO_ID.pingguo, 4)
  assert.equal(CROP_KEY_TO_ID.xiaomai, 15)
  assert.equal(CROP_ID_TO_KEY[4], 'pingguo')
  assert.equal(CROP_ID_TO_KEY[15], 'xiaomai')
  assert.equal(cropIdToKey(4), 'pingguo')
  assert.equal(cropKeyToId('pingguo'), 4)
  assert.equal(cropIdToKey(15), 'xiaomai')
  assert.equal(cropKeyToId('xiaomai'), 15)
  assert.equal(cropIdToKey(0), null)
  assert.equal(cropKeyToId(null), 0)
})

test('小麦/苹果数值与设计文档一致', () => {
  assert.equal(CROP_MAP.xiaomai.seedPrice, 168)
  assert.equal(CROP_MAP.xiaomai.cycleMinutes, 840)
  assert.equal(CROP_MAP.xiaomai.cycleH, 14)
  assert.equal(CROP_MAP.xiaomai.yield, 18)
  assert.equal(CROP_MAP.pingguo.seedPrice, 578)
  assert.equal(CROP_MAP.pingguo.seasons, 2)
  assert.equal(CROP_MAP.pingguo.cycleMinutes, 1800)
  assert.equal(CROP_MAP.pingguo.cycleH, 30)
  assert.equal(CROP_MAP.pingguo.unlock, 10)
  assert.equal(CROP_MAP.renshen.hidden, true)
  assert.equal(CROP_MAP.renshen.seedPrice, 0)
})

test('季时长整数分钟与档位换算无截断', () => {
  const strawberry = CROP_MAP.caomei
  assert.equal(strawberry.cycleMinutes, 2100)
  assert.equal(seasonMinutes(strawberry, 0), 1400)
  assert.equal(seasonMinutes(strawberry, 1), 700)

  const orange = CROP_MAP.chengzi
  assert.equal(orange.cycleMinutes, 3540)
  assert.equal(seasonMinutes(orange, 0), 1770)
  assert.equal(seasonMinutes(orange, 1), 885)
  assert.equal(seasonMinutes(orange, 2), 885)

  for (const key of ['demo', 'fast', 'authentic']) {
    const hourMs = TIME_SCALES[key].hourMs
    assert.equal(hourMs % 60, 0, `${key} hourMs must divide by 60`)
    assert.equal(seasonDurationMs(strawberry, 0, key), (1400 * hourMs) / 60)
    assert.equal(seasonDurationMs(strawberry, 1, key), (700 * hourMs) / 60)
  }
})
