import test from 'node:test'
import assert from 'node:assert/strict'

import { enterOnline, leaveOnline, logout, session, setFarmView } from './session.js'

test('进入农场快照记录 owner、序列与关系', () => {
  const previousGeneration = session.farmViewGeneration || 0
  setFarmView({ ownerUid: 42, farmSeq: 8, relation: 'FRIEND', ownerName: '小明' })

  assert.equal(session.viewingOwnerUid, 42)
  assert.equal(session.lastFarmSeq, 8)
  assert.equal(session.relation, 'FRIEND')
  assert.equal(session.viewingOwnerName, '小明')
  assert.equal(session.farmViewGeneration, previousGeneration + 1)
})

test('回到自己农场清空参观昵称', () => {
  setFarmView({ ownerUid: 42, farmSeq: 1, relation: 'SELF' })
  assert.equal(session.viewingOwnerName, null)
})

test('leaveOnline 清空参观昵称', () => {
  setFarmView({ ownerUid: 99, farmSeq: 2, relation: 'FRIEND', ownerName: '阿花' })
  leaveOnline()
  assert.equal(session.viewingOwnerName, null)
})

test('logout 清空认证凭证与农场会话', () => {
  enterOnline({ uid: 42, token: 'token' })
  setFarmView({ ownerUid: 42, farmSeq: 3, relation: 'SELF' })

  logout()

  assert.equal(session.isOnline, false)
  assert.equal(session.uid, null)
  assert.equal(session.token, null)
  assert.equal(session.viewingOwnerUid, null)
})
