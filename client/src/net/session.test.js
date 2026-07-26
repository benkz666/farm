import test from 'node:test'
import assert from 'node:assert/strict'

import { session, setFarmView } from './session.js'

test('进入农场快照记录 owner、序列与关系', () => {
  const previousGeneration = session.farmViewGeneration || 0
  setFarmView({ ownerUid: 42, farmSeq: 8, relation: 'FRIEND' })

  assert.equal(session.viewingOwnerUid, 42)
  assert.equal(session.lastFarmSeq, 8)
  assert.equal(session.relation, 'FRIEND')
  assert.equal(session.farmViewGeneration, previousGeneration + 1)
})
