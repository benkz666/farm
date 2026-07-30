import test from 'node:test'
import assert from 'node:assert/strict'

import { taskCardViewModel } from './taskCardView.js'

test('普通完成未领任务：claimAction 为 claimTask(id)，无 614 分支', () => {
  const vm = taskCardViewModel({
    id: 1,
    taskId: '1',
    title: '完成一次播种',
    progress: 1,
    target: 1,
    rewardCoin: 20,
    done: true,
    claimed: false,
  })
  assert.equal(vm.statusTag, 'claimable')
  assert.deepEqual(vm.claimAction, { type: 'claimTask', taskId: 1 })
  assert.equal(vm.claimAction?.type === 'claimDailyLogin', false)
})

test('Task 4 每日登录与普通任务同一卡片状态与 onClaimTask(id) 决策', () => {
  const plant = taskCardViewModel({
    id: 1,
    title: '完成一次播种',
    progress: 1,
    target: 1,
    rewardCoin: 20,
    done: true,
    claimed: false,
  })
  const daily = taskCardViewModel({
    id: 4,
    taskId: '4',
    title: '每日登录',
    progress: 1,
    target: 1,
    rewardCoin: 100,
    done: true,
    claimed: false,
  })

  assert.equal(daily.statusTag, plant.statusTag)
  assert.equal(daily.claimAction?.type, 'claimTask')
  assert.equal(plant.claimAction?.type, 'claimTask')
  assert.deepEqual(daily.claimAction, { type: 'claimTask', taskId: 4 })
  assert.equal(daily.claimed, false)
  assert.equal(daily.done, true)
})

test('Task 4 已领取与普通任务同为 claimed，无可领动作', () => {
  const daily = taskCardViewModel({
    id: 4,
    title: '每日登录',
    progress: 1,
    target: 1,
    rewardCoin: 100,
    done: true,
    claimed: true,
  })
  const harvest = taskCardViewModel({
    id: 2,
    title: '完成一次收获',
    progress: 1,
    target: 1,
    rewardCoin: 30,
    done: true,
    claimed: true,
  })
  assert.equal(daily.statusTag, 'claimed')
  assert.equal(harvest.statusTag, 'claimed')
  assert.equal(daily.claimAction, null)
  assert.equal(harvest.claimAction, null)
})

test('未完成任务不暴露领取动作', () => {
  const vm = taskCardViewModel({
    id: 3,
    title: '拜访一次好友农场',
    progress: 0,
    target: 1,
    rewardCoin: 40,
    done: false,
    claimed: false,
  })
  assert.equal(vm.statusTag, 'in_progress')
  assert.equal(vm.claimAction, null)
  assert.equal(vm.reward, 40)
})
