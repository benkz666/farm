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

test('切换房间后丢弃旧房间在途 SyncFarm 快照', async () => {
  const state = { plots: [], gold: 99 }
  const session = { viewingOwnerUid: 42, lastFarmSeq: 3, farmViewGeneration: 1 }
  let resolveSync
  let signalSyncStarted
  const syncStarted = new Promise((resolve) => {
    signalSyncStarted = resolve
  })
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: () => {
      signalSyncStarted()
      return new Promise((resolve) => {
        resolveSync = resolve
      })
    },
  })

  const pendingDelta = mirror.onDelta({ owner_uid: 42, farm_seq: 5, plots: [] })
  await syncStarted
  session.viewingOwnerUid = 99
  session.lastFarmSeq = 6
  session.farmViewGeneration = 2
  session.viewingOwnerUid = 42
  session.lastFarmSeq = 8
  session.farmViewGeneration = 3
  resolveSync({ payload: { farm_seq: 5, snapshot: { owner_uid: 42, coin: 1 } } })

  await pendingDelta

  assert.equal(state.gold, 99)
  assert.equal(session.lastFarmSeq, 8)
})
