import test from 'node:test'
import assert from 'node:assert/strict'

import { LOGIC_DAY_MIN_MS, logicDayMs, logicDayPhase, logicDayStart } from './config.js'

test('demo 档逻辑日长度为下限 5 分钟', () => {
  assert.equal(logicDayMs('demo'), LOGIC_DAY_MIN_MS)
})

test('logicDayStart / logicDayPhase 与服务端 LogicDayID 同口径（epoch 整除）', () => {
  const dayMs = logicDayMs('demo')
  const t0 = 10 * dayMs + 90_000 // 进入第 10 逻辑日后 90s
  assert.equal(logicDayStart(t0, 'demo'), 10 * dayMs)
  assert.ok(Math.abs(logicDayPhase(t0, 'demo') - 90_000 / dayMs) < 1e-12)
})

test('同一时刻两名玩家算出相同昼夜 phase（与登录起点无关）', () => {
  const now = 1_700_000_123_456
  const a = logicDayPhase(now, 'demo')
  const b = logicDayPhase(now, 'demo')
  assert.equal(a, b)
  assert.ok(a >= 0 && a < 1)
})

test('跨越逻辑日边界时 phase 回绕到 0 附近', () => {
  const dayMs = logicDayMs('demo')
  const justBefore = dayMs - 1
  const justAfter = dayMs
  assert.ok(logicDayPhase(justBefore, 'demo') > 0.99)
  assert.equal(logicDayPhase(justAfter, 'demo'), 0)
})

test('fast 档按 24 缩放小时滚动，不套用 5 分钟下限误算', () => {
  const dayMs = logicDayMs('fast')
  assert.equal(dayMs, 24 * 60_000)
  const now = 3 * dayMs + dayMs / 4
  assert.equal(logicDayPhase(now, 'fast'), 0.25)
})
