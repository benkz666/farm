import test from 'node:test'
import assert from 'node:assert/strict'

import { canExpandLand, hudSignature } from './ui.js'
import { taskCardViewModel } from './taskCardView.js'

const state = {
  gold: 770,
  exp: 18,
  dog: null,
  dogBowl: 0,
  mails: [],
  friendRequests: [],
  tasks: [],
  unlockedPlots: 6,
}

test('hudSignature 仅在 HUD 可见数据变化时改变', () => {
  const before = hudSignature(state, false)

  assert.equal(hudSignature({ ...state }, false), before)
  assert.notEqual(hudSignature({ ...state, gold: 771 }, false), before)
  assert.notEqual(hudSignature({ ...state, exp: 19 }, false), before)
})

test('hudSignature 在可领任务（含每日登录）出现时变化以驱动红点', () => {
  const before = hudSignature(state, false)
  const withDaily = {
    ...state,
    tasks: [{ id: 4, progress: 1, target: 1, done: true, claimed: false }],
  }
  assert.notEqual(hudSignature(withDaily, false), before)
  assert.equal(
    hudSignature({
      ...withDaily,
      tasks: [{ id: 4, progress: 1, target: 1, done: true, claimed: true }],
    }, false),
    before,
  )
})

test('扩地入口只在自己的农场满足下一块土地等级和金币要求时显示', () => {
  assert.equal(canExpandLand(state, false), false, 'Lv.0 与 770 金币不能显示扩地入口')
  assert.equal(canExpandLand({ ...state, exp: 1000, gold: 10_000 }, false), true)
  assert.equal(canExpandLand({ ...state, exp: 1000, gold: 9_999 }, false), false)
  assert.equal(canExpandLand({ ...state, exp: 1000, gold: 10_000 }, true), false, '好友农场始终隐藏')
})

test('扩地金币比较不会把超过 2^53 的余额转成 Number', () => {
  assert.equal(
    canExpandLand({ ...state, exp: 1000, gold: '9007199254740993' }, false),
    true,
  )
  assert.notEqual(
    hudSignature({ ...state, gold: '9007199254740992' }, false),
    hudSignature({ ...state, gold: '9007199254740993' }, false),
  )
})

test('任务面板领取决策统一走 claimTask，Task 4 无独立 614 动作', () => {
  const tasks = [
    { id: 1, title: '完成一次播种', progress: 1, target: 1, rewardCoin: 20, done: true, claimed: false },
    { id: 4, title: '每日登录', progress: 1, target: 1, rewardCoin: 100, done: true, claimed: false },
  ]
  const actions = tasks.map((t) => taskCardViewModel(t).claimAction)
  assert.deepEqual(actions, [
    { type: 'claimTask', taskId: 1 },
    { type: 'claimTask', taskId: 4 },
  ])
  assert.ok(actions.every((a) => a && a.type === 'claimTask' && a.type !== 'claimDailyLogin'))
})
