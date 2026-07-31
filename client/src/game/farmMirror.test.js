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

test('同步期间到达的 Delta 在同步完成后依序应用', async () => {
  const state = { plots: [{ state: 1 }] }
  const session = { viewingOwnerUid: 42, lastFarmSeq: 3 }
  let resolveSync
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: () => new Promise((resolve) => {
      resolveSync = resolve
    }),
  })

  const syncing = mirror.onDelta({ owner_uid: 42, farm_seq: 5, plots: [] })
  const receivedDuringSync = mirror.onDelta({
    owner_uid: 42,
    farm_seq: 4,
    plots: [{ index: 0, state: 2 }],
  })
  resolveSync({ payload: { farm_seq: 3, deltas: [] } })

  await Promise.all([syncing, receivedDuringSync])

  assert.equal(session.lastFarmSeq, 5)
  assert.equal(state.plots[0].state, 'growing')
})

test('主动同步没有新 Delta 也是成功结果，不制造同步失败', async () => {
  const state = { plots: [] }
  const session = { viewingOwnerUid: 42, lastFarmSeq: 3, farmViewGeneration: 1 }
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async () => ({ payload: { farm_seq: 3, server_time: 9_999 } }),
  })

  assert.equal(await mirror.syncNow(), true)
  assert.equal(session.lastFarmSeq, 3)
})

test('SyncFarm 即使没有 Delta 也会应用服务端时间档设置', async () => {
  const state = { plots: [], timeScale: 'demo', timeScaleMutable: false }
  const session = { viewingOwnerUid: 42, lastFarmSeq: 3, farmViewGeneration: 1 }
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async () => ({
      payload: {
        farm_seq: 3,
        time_profile: 'fast',
        time_profile_mutable: true,
      },
    }),
  })

  assert.equal(await mirror.syncNow(), true)
  assert.equal(state.timeScale, 'fast')
  assert.equal(state.timeScaleMutable, true)
})

test('自己农场主动同步固定发送 owner_uid=0，不沿用残留好友 UID', async () => {
  const state = { plots: [] }
  const session = {
    uid: 42,
    viewingOwnerUid: 99,
    relation: 'SELF',
    lastFarmSeq: 3,
    farmViewGeneration: 1,
  }
  const calls = []
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async (ownerUid, fromSeq) => {
      calls.push({ ownerUid, fromSeq })
      return {
        payload: {
          farm_seq: 3,
          snapshot: { owner_uid: 42, plots: [] },
        },
      }
    },
  })

  assert.equal(await mirror.syncNow(), true)
  assert.deepEqual(calls, [{ ownerUid: 0, fromSeq: 3 }])
})

test('好友农场同步保留 19 位 UID 字符串精度', async () => {
  const ownerUid = '1785402171458126005'
  const state = { plots: [] }
  const session = {
    uid: '1785402171458126999',
    viewingOwnerUid: ownerUid,
    relation: 'FRIEND',
    lastFarmSeq: 3,
    farmViewGeneration: 1,
  }
  const calls = []
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async (targetUid, fromSeq) => {
      calls.push({ targetUid, fromSeq })
      return {
        payload: {
          farm_seq: 3,
          snapshot: { owner_uid: ownerUid, plots: [] },
        },
      }
    },
  })

  assert.equal(await mirror.syncNow(), true)
  assert.deepEqual(calls, [{ targetUid: ownerUid, fromSeq: 3 }])
})

test('FarmDelta 序列跨越 2^53 后仍按相邻 uint64 精确推进', async () => {
  const state = { plots: [{ state: 1 }] }
  const session = {
    viewingOwnerUid: '1785402171458126005',
    relation: 'FRIEND',
    lastFarmSeq: '9007199254740992',
  }
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async () => {
      throw new Error('连续序列不应触发 SyncFarm')
    },
  })

  const applied = await mirror.onDelta({
    owner_uid: '1785402171458126005',
    farm_seq: '9007199254740993',
    plots: [{ index: 0, state: 2 }],
  })

  assert.equal(applied, true)
  assert.equal(session.lastFarmSeq, '9007199254740993')
  assert.equal(state.plots[0].state, 'growing')
})

test('FarmDelta 不会把 2^53 后的两个相邻序列误判成同一个值', async () => {
  const state = { plots: [] }
  const session = {
    viewingOwnerUid: 42,
    lastFarmSeq: '9007199254740991',
  }
  let syncCalls = 0
  const mirror = createFarmMirror({
    state,
    session,
    syncFarm: async () => {
      syncCalls++
      return { payload: { farm_seq: '9007199254740991' } }
    },
  })

  const applied = await mirror.onDelta({
    owner_uid: 42,
    farm_seq: '9007199254740993',
    plots: [],
  })

  assert.equal(applied, false)
  assert.equal(syncCalls, 2)
  assert.equal(session.lastFarmSeq, '9007199254740991')
})
