import test from 'node:test'
import assert from 'node:assert/strict'

import { createFarmMirror } from './farmMirror.js'

test('乱序 FarmDelta 请求 SyncFarm 而不修改镜像序列', async () => {
  const state = { plots: [] }
  const session = { viewingOwnerUid: 42, lastFarmSeq: 3 }
  let syncCalls = 0
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async () => {
      syncCalls++
    },
  })

  const applied = await mirror.onDelta({
    owner_uid: 42,
    farm_seq: 5,
    plots: [],
  })

  assert.equal(applied, false)
  assert.equal(syncCalls, 1)
  assert.equal(session.lastFarmSeq, 3)
})
