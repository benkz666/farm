import test from 'node:test'
import assert from 'node:assert/strict'

import { applyTaskNotify, mapServerTasks } from './taskSync.js'

test('mapServerTasks 映射服务端四项任务字段', () => {
  const mapped = mapServerTasks([
    { id: 4, title: '每日登录', progress: 1, target: 1, reward_coin: 100, claimed: false },
  ])
  assert.deepEqual(mapped, [{
    id: 4,
    taskId: '4',
    title: '每日登录',
    progress: 1,
    target: 1,
    rewardCoin: 100,
    done: true,
    claimed: false,
  }])
})

test('applyTaskNotify 按 id 合并/替换权威任务，不改动其它任务', () => {
  const tasks = mapServerTasks([
    { id: 1, title: '完成一次播种', progress: 0, target: 1, reward_coin: 20, claimed: false },
    { id: 2, title: '完成一次收获', progress: 0, target: 1, reward_coin: 30, claimed: false },
  ])
  const next = applyTaskNotify(tasks, {
    id: 1,
    title: '完成一次播种',
    progress: 1,
    target: 1,
    reward_coin: 20,
    claimed: false,
  })
  assert.equal(next[0].progress, 1)
  assert.equal(next[0].done, true)
  assert.equal(next[1].progress, 0)
})

test('applyTaskNotify 只更新任务状态，返回值不含金币字段', () => {
  const gold = { value: 900 }
  const tasks = mapServerTasks([
    { id: 2, title: '完成一次收获', progress: 0, target: 1, reward_coin: 30, claimed: false },
  ])
  const next = applyTaskNotify(tasks, {
    id: 2,
    title: '完成一次收获',
    progress: 1,
    target: 1,
    reward_coin: 30,
    claimed: false,
    coin: 9999,
  })
  assert.equal(gold.value, 900)
  assert.equal(Object.prototype.hasOwnProperty.call(next[0], 'coin'), false)
  assert.equal(next[0].rewardCoin, 30)
  assert.equal(next[0].done, true)
})

test('applyTaskNotify 对新 id 追加任务项', () => {
  const next = applyTaskNotify([], {
    id: 3,
    title: '拜访一次好友农场',
    progress: 1,
    target: 1,
    reward_coin: 40,
    claimed: false,
  })
  assert.equal(next.length, 1)
  assert.equal(next[0].id, 3)
  assert.equal(next[0].done, true)
})
