import test from 'node:test'
import assert from 'node:assert/strict'

import { logicDayStart } from './config.js'
import { applyMailClaimReceipt, defaultState } from './state.js'

test('defaultState 不含 NPC 假好友', () => {
  const state = defaultState()
  assert.deepEqual(state.friends, [])
})

test('defaultState.dayStart 对齐全局逻辑日起点', () => {
  const state = defaultState()
  assert.equal(state.daily.dayStart, logicDayStart(state.createdAt, state.timeScale))
})

test('邮件领取回执不乐观累加金币', () => {
  const state = defaultState()
  state.gold = 900
  state.mails = [{ id: 7, gold: 100, attachmentCoin: 100, claimed: false }]

  const reward = applyMailClaimReceipt(state, 7, { attachment_coin: 100 })

  assert.equal(reward, 100)
  assert.equal(state.gold, 900)
  assert.deepEqual(state.mails[0], {
    id: 7,
    gold: 0,
    attachmentCoin: 0,
    claimed: true,
    read: true,
  })
})
