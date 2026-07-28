import test from 'node:test'
import assert from 'node:assert/strict'

import {
  applyAuthoritativeFarmEnter,
  bindFarmReconnectRestore,
} from './reconnectRestore.js'

function makeSession(overrides = {}) {
  return {
    isOnline: true,
    uid: 42,
    token: 'tok',
    viewingOwnerUid: 99,
    lastFarmSeq: 7,
    farmViewGeneration: 1,
    relation: 'FRIEND',
    ...overrides,
  }
}

function makeHarness(sessionOverrides = {}) {
  const session = makeSession(sessionOverrides)
  const state = { plots: [], gold: 0, stamped: null }
  const uiCalls = []
  const fails = []
  const leaveCalls = []
  const cleanupCalls = []
  let onlineBusy = true

  const restoredHandlers = new Set()
  const failedHandlers = new Set()
  const client = {
    getResumeContext: null,
    onFarmRestored(handler) {
      restoredHandlers.add(handler)
      return () => restoredHandlers.delete(handler)
    },
    onFarmRestoreFailed(handler) {
      failedHandlers.add(handler)
      return () => failedHandlers.delete(handler)
    },
    emitRestored(env) {
      for (const h of [...restoredHandlers]) h(env)
    },
    emitFailed(reason) {
      for (const h of [...failedHandlers]) h(reason)
    },
  }

  const deps = {
    client,
    session,
    state,
    applyPatch: (st, payload) => {
      st.stamped = payload
      st.gold = payload.snapshot?.coin ?? st.gold
    },
    setFarmView: ({ ownerUid, farmSeq, relation }) => {
      session.farmViewGeneration++
      session.viewingOwnerUid = ownerUid
      session.lastFarmSeq = farmSeq
      session.relation = relation
    },
    leaveOnline: () => {
      leaveCalls.push(true)
      session.isOnline = false
      session.viewingOwnerUid = null
      session.lastFarmSeq = 0
      session.relation = null
      session.farmViewGeneration++
    },
    setOnlineBusy: (v) => {
      onlineBusy = v
    },
    getOnlineBusy: () => onlineBusy,
    getSelfUid: () => 42,
    refreshUI: (info) => {
      uiCalls.push(info)
    },
    toast: (msg, type) => {
      uiCalls.push({ toast: msg, type })
    },
    fail: (msg) => {
      fails.push(msg)
    },
    errText: (err) => (err === 1102 ? '登录已过期，请重新登录' : `err=${err}`),
    onOfflineCleanup: () => {
      cleanupCalls.push(true)
    },
  }

  return {
    deps,
    client,
    session,
    state,
    uiCalls,
    fails,
    leaveCalls,
    cleanupCalls,
    restoredHandlers,
    failedHandlers,
    get onlineBusy() {
      return onlineBusy
    },
  }
}

test('onFarmRestored 好友 EnterFarm 全量响应：权威 apply、更新 view、清 busy、刷新 UI', () => {
  const h = makeHarness()
  bindFarmReconnectRestore(h.deps)

  h.client.emitRestored({
    cmd: 200,
    client_seq: 3,
    err: 0,
    payload: {
      farm_seq: 12,
      relation: 'FRIEND',
      snapshot: { owner_uid: 99, coin: 500, plots: [{ index: 0 }] },
    },
  })

  assert.equal(h.onlineBusy, false)
  assert.deepEqual(h.state.stamped.snapshot.owner_uid, 99)
  assert.equal(h.state.gold, 500)
  assert.equal(h.session.viewingOwnerUid, 99)
  assert.equal(h.session.lastFarmSeq, 12)
  assert.equal(h.session.relation, 'FRIEND')
  assert.ok(h.uiCalls.some((c) => c.relation === 'FRIEND'))
  assert.ok(h.uiCalls.some((c) => c.toast))
})

test('onFarmRestored 自己农场：更新为 SELF view', () => {
  const h = makeHarness({ viewingOwnerUid: 42, relation: 'SELF', lastFarmSeq: 1 })
  bindFarmReconnectRestore(h.deps)

  h.client.emitRestored({
    cmd: 200,
    err: 0,
    payload: {
      farm_seq: 4,
      relation: 'SELF',
      snapshot: { owner_uid: 42, coin: 10 },
    },
  })

  assert.equal(h.session.viewingOwnerUid, 42)
  assert.equal(h.session.lastFarmSeq, 4)
  assert.equal(h.session.relation, 'SELF')
  assert.equal(h.onlineBusy, false)
})

test('权限撤销回退自己的响应不会保留旧好友 view', () => {
  const h = makeHarness({ viewingOwnerUid: 99, relation: 'FRIEND', lastFarmSeq: 7 })
  bindFarmReconnectRestore(h.deps)

  h.client.emitRestored({
    cmd: 200,
    err: 0,
    payload: {
      farm_seq: 2,
      relation: 'SELF',
      snapshot: { owner_uid: 42 },
    },
  })

  assert.equal(h.session.viewingOwnerUid, 42)
  assert.equal(h.session.relation, 'SELF')
  assert.equal(h.session.lastFarmSeq, 2)
  assert.notEqual(h.session.viewingOwnerUid, 99)
})

test('onFarmRestoreFailed 进入离线并清理可交互状态', () => {
  const h = makeHarness()
  bindFarmReconnectRestore(h.deps)

  h.client.emitFailed({ cmd: 100, err: 1102, payload: {} })

  assert.equal(h.session.isOnline, false)
  assert.equal(h.session.viewingOwnerUid, null)
  assert.equal(h.session.relation, null)
  assert.equal(h.onlineBusy, false)
  assert.equal(h.leaveCalls.length, 1)
  assert.equal(h.cleanupCalls.length, 1)
  assert.ok(h.fails.some((m) => /无法恢复|登录已过期/.test(m)))
})

test('重新绑定不会重复注册 handler', () => {
  const h = makeHarness()
  const first = bindFarmReconnectRestore(h.deps)
  assert.equal(h.restoredHandlers.size, 1)
  assert.equal(h.failedHandlers.size, 1)

  const second = bindFarmReconnectRestore(h.deps)
  assert.equal(h.restoredHandlers.size, 1)
  assert.equal(h.failedHandlers.size, 1)

  let restores = 0
  // 通过副作用计数：再 emit 一次应只 apply 一次
  const goldBefore = h.state.gold
  h.client.emitRestored({
    cmd: 200,
    err: 0,
    payload: { farm_seq: 9, relation: 'SELF', snapshot: { owner_uid: 42, coin: 77 } },
  })
  assert.equal(h.state.gold, 77)
  assert.notEqual(h.state.gold, goldBefore)

  first.dispose()
  second.dispose()
  h.client.emitRestored({
    cmd: 200,
    err: 0,
    payload: { farm_seq: 10, relation: 'SELF', snapshot: { owner_uid: 42, coin: 1 } },
  })
  // dispose 后不应再改 state
  assert.equal(h.state.gold, 77)
  assert.equal(restores, 0)
})

test('applyAuthoritativeFarmEnter 可独立强制覆盖 snapshot', () => {
  const h = makeHarness()
  applyAuthoritativeFarmEnter(h.deps, {
    err: 0,
    payload: {
      farm_seq: 3,
      relation: 'FRIEND',
      snapshot: { owner_uid: 88, coin: 9 },
    },
  })
  assert.equal(h.session.viewingOwnerUid, 88)
  assert.equal(h.session.lastFarmSeq, 3)
  assert.equal(h.onlineBusy, false)
  assert.equal(h.state.gold, 9)
})

test('恢复失败会解除 restore 订阅，避免重复回调', () => {
  const h = makeHarness()
  bindFarmReconnectRestore(h.deps)
  h.client.emitFailed({ err: 1102 })
  assert.equal(h.failedHandlers.size, 0)
  assert.equal(h.restoredHandlers.size, 0)
  h.client.emitFailed({ err: 1102 })
  assert.equal(h.fails.length, 1)
  assert.equal(h.cleanupCalls.length, 1)
})
