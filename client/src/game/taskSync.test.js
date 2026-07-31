import test from 'node:test'
import assert from 'node:assert/strict'

import { applyTaskNotify, mapServerTasks } from './taskSync.js'

test('mapServerTasks 映射服务端任务类别与权威字段', () => {
  const mapped = mapServerTasks([
    { id: 4, day_key: 20260731, kind: 'fixed', title: '每日登录', progress: 1, target: 1, reward_coin: 100, claimed: false },
  ])
  assert.deepEqual(mapped, [{
    id: 4,
    dayKey: 20260731,
    kind: 'fixed',
    taskId: '4',
    title: '每日登录',
    progress: 1,
    target: 1,
    rewardCoin: 100,
    done: true,
    claimed: false,
  }])
})

test('未提供类别时不推导任务详情，仅保留服务端返回的详情', () => {
  const [task] = mapServerTasks([
    { id: 5, day_key: 20260731, title: '浇水 10 次', progress: 1, target: 10, reward_coin: 200, claimed: false },
  ])
  assert.equal(task.kind, 'random')
})

test('缺少服务端任务详情时不生成客户端默认任务', () => {
  assert.deepEqual(mapServerTasks([
    { id: 5, day_key: 20260731, progress: 1, target: 10, reward_coin: 200, claimed: false },
    { id: 6, day_key: 20260731, title: '施肥 1 次', progress: 1, claimed: false },
  ]), [])
  assert.equal(applyTaskNotify([], {
    id: 5,
    day_key: 20260731,
    progress: 1,
    target: 10,
    reward_coin: 200,
    claimed: false,
  }), null)
})

test('applyTaskNotify 按 id 合并/替换权威任务，不改动其它任务', () => {
  const tasks = mapServerTasks([
    { id: 1, day_key: 20260731, title: '完成一次播种', progress: 0, target: 1, reward_coin: 20, claimed: false },
    { id: 2, day_key: 20260731, title: '完成一次收获', progress: 0, target: 1, reward_coin: 30, claimed: false },
  ])
  const next = applyTaskNotify(tasks, {
    id: 1,
    day_key: 20260731,
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
    { id: 2, day_key: 20260731, title: '完成一次收获', progress: 0, target: 1, reward_coin: 30, claimed: false },
  ])
  const next = applyTaskNotify(tasks, {
    id: 2,
    day_key: 20260731,
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
    day_key: 20260731,
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
