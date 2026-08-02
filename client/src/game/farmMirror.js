import { applyPatch } from './applyPatch.js'
import {
  compareUint64,
  isNextUint64,
  sameUid,
  sameUint64,
  wireUid,
  wireUint64,
} from '../net/jsonSafe.js'

/**
 * 将当前房间的 FarmDelta 依序投影到本地状态；发现序列缺口时请求服务端补齐。
 * @param {{
 *   state: object,
 *   session: { uid?: string|number|null, viewingOwnerUid?: string|number|null, relation?: 'SELF'|'FRIEND'|null, lastFarmSeq: string|number, farmViewGeneration?: number },
 *   syncFarm: (ownerUid: string|number, fromSeq: string|number) => Promise<object|undefined>,
 *   onApplied?: () => void,
 * }} options
 */
export function createFarmMirror({ state, session, syncFarm, onApplied = () => {} }) {
  let syncing = null
  let disposed = false

  function currentView() {
    const selfView = session.relation === 'SELF'
    const ownerUid = selfView
      ? (wireUid(session.uid) ?? wireUid(session.viewingOwnerUid) ?? 0)
      : (wireUid(session.viewingOwnerUid) ?? 0)
    return {
      ownerUid,
      // 协议约定 owner_uid=0 永远表示本人农场。SELF 视图不能复用内存里
      // 可能残留的好友 UID，否则成熟边界同步会误入好友关系校验。
      requestOwnerUid: selfView ? 0 : ownerUid,
      relation: session.relation || null,
      generation: Number.isSafeInteger(session.farmViewGeneration)
        ? session.farmViewGeneration
        : null,
    }
  }

  function isCurrentView(view) {
    const current = currentView()
    return sameUid(current.ownerUid, view.ownerUid) &&
      current.relation === view.relation &&
      current.generation === view.generation
  }

  async function applySync(result, requestedView = currentView()) {
    if (disposed) return false
    const payload = result?.payload ?? result
    if (!payload || typeof payload !== 'object') return false
    if (!isCurrentView(requestedView)) return false

    if (payload.snapshot && typeof payload.snapshot === 'object') {
      if (!sameUid(payload.snapshot.owner_uid, requestedView.ownerUid)) return false
      applyPatch(state, payload, {
        farmViewOnly: session.relation === 'FRIEND',
      })
      const farmSeq = wireUint64(payload.farm_seq)
      if (farmSeq == null) return false
      session.lastFarmSeq = farmSeq
      onApplied()
      return true
    }

    // 没有新 Delta 是一次合法同步结果（例如客户端时钟略早于服务端边界）。
    if (!Array.isArray(payload.deltas)) {
      if (!sameUint64(payload.farm_seq, session.lastFarmSeq)) return false
      applyPatch(state, payload)
      return true
    }
    applyPatch(state, payload)
    for (const delta of payload.deltas) {
      if (!isCurrentView(requestedView)) return false
      if (!applyDelta(delta)) return false
    }
    return true
  }

  function applyDelta(delta) {
    if (disposed) return false
    if (!delta || typeof delta !== 'object') return false
    if (!sameUid(delta.owner_uid, session.viewingOwnerUid)) return false
    const seq = wireUint64(delta.farm_seq)
    if (seq == null || !isNextUint64(seq, session.lastFarmSeq)) return false
    applyPatch(state, delta)
    session.lastFarmSeq = seq
    onApplied()
    return true
  }

  function bufferDelta(sync, delta) {
    if (!sync.buffer.some((item) => sameUint64(item.farm_seq, delta.farm_seq))) {
      sync.buffer.push(delta)
    }
  }

  function applyBufferedDeltas(sync, view) {
    sync.buffer.sort((left, right) => compareUint64(left.farm_seq, right.farm_seq) ?? 0)
    while (sync.buffer.length > 0) {
      sync.buffer = sync.buffer.filter(
        (delta) => (compareUint64(delta.farm_seq, session.lastFarmSeq) ?? -1) > 0,
      )
      const next = sync.buffer.find((delta) => isNextUint64(delta.farm_seq, session.lastFarmSeq))
      if (!next) return sync.buffer.length === 0
      sync.buffer = sync.buffer.filter((delta) => delta !== next)
      if (!isCurrentView(view) || !applyDelta(next)) return false
    }
    return true
  }

  async function requestSync(delta) {
    if (disposed) return false
    const view = currentView()
    if (syncing && syncing.view.ownerUid === view.ownerUid && syncing.view.generation === view.generation) {
      if (delta) bufferDelta(syncing, delta)
      return syncing.promise
    }
    const sync = { view, buffer: [] }
    if (delta) bufferDelta(sync, delta)
    const promise = (async () => {
      let retried = false
      while (isCurrentView(view)) {
        const result = await syncFarm(view.requestOwnerUid, session.lastFarmSeq)
        if (!await applySync(result, view)) return false
        if (applyBufferedDeltas(sync, view)) return true
        if (retried) return false
        retried = true
      }
      return false
    })()
      .finally(() => {
        if (syncing?.promise === promise) syncing = null
      })
    sync.promise = promise
    syncing = sync
    return promise
  }

  return {
    async onDelta(delta) {
      if (disposed) return false
      if (!sameUid(delta?.owner_uid, session.viewingOwnerUid)) return false
      const seq = wireUint64(delta?.farm_seq)
      // Gateway 对局部失败批次会有限重试。相同或更旧的 Delta 已经体现在
      // 当前镜像中，直接幂等忽略，避免一次重试额外触发 SyncFarm。
      if (seq != null && (compareUint64(seq, session.lastFarmSeq) ?? 1) <= 0) return true
      if (syncing && sameUid(syncing.view.ownerUid, session.viewingOwnerUid)) {
        return requestSync(delta)
      }
      if (applyDelta(delta)) return true
      return requestSync(delta)
    },
    async syncNow() {
      return requestSync(null)
    },
    applySync,
    dispose() {
      disposed = true
      syncing = null
    },
  }
}
