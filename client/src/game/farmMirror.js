import { applyPatch } from './applyPatch.js'

/**
 * 将当前房间的 FarmDelta 依序投影到本地状态；发现序列缺口时请求服务端补齐。
 * @param {{
 *   state: object,
 *   session: { viewingOwnerUid?: number|null, lastFarmSeq: number, farmViewGeneration?: number },
 *   syncFarm: (ownerUid: number, fromSeq: number) => Promise<object|undefined>,
 *   onApplied?: () => void,
 * }} options
 */
export function createFarmMirror({ state, session, syncFarm, onApplied = () => {} }) {
  let syncing = null
  let disposed = false

  function currentView() {
    return {
      ownerUid: Number(session.viewingOwnerUid),
      generation: Number.isSafeInteger(session.farmViewGeneration)
        ? session.farmViewGeneration
        : null,
    }
  }

  function isCurrentView(view) {
    const current = currentView()
    return current.ownerUid === view.ownerUid && current.generation === view.generation
  }

  async function applySync(result, requestedView = currentView()) {
    if (disposed) return false
    const payload = result?.payload ?? result
    if (!payload || typeof payload !== 'object') return false
    if (!isCurrentView(requestedView)) return false

    if (payload.snapshot && typeof payload.snapshot === 'object') {
      if (Number(payload.snapshot.owner_uid) !== requestedView.ownerUid) return false
      applyPatch(state, { snapshot: payload.snapshot }, {
        farmViewOnly: session.relation === 'FRIEND',
      })
      session.lastFarmSeq = Number(payload.farm_seq) || 0
      onApplied()
      return true
    }

    if (!Array.isArray(payload.deltas)) return false
    for (const delta of payload.deltas) {
      if (!isCurrentView(requestedView)) return false
      if (!applyDelta(delta)) return false
    }
    return true
  }

  function applyDelta(delta) {
    if (disposed) return false
    if (!delta || typeof delta !== 'object') return false
    if (Number(delta.owner_uid) !== Number(session.viewingOwnerUid)) return false
    const seq = Number(delta.farm_seq)
    if (!Number.isSafeInteger(seq) || seq !== session.lastFarmSeq + 1) return false
    applyPatch(state, delta)
    session.lastFarmSeq = seq
    onApplied()
    return true
  }

  function bufferDelta(sync, delta) {
    if (!sync.buffer.some((item) => Number(item.farm_seq) === Number(delta.farm_seq))) {
      sync.buffer.push(delta)
    }
  }

  function applyBufferedDeltas(sync, view) {
    sync.buffer.sort((left, right) => Number(left.farm_seq) - Number(right.farm_seq))
    while (sync.buffer.length > 0) {
      sync.buffer = sync.buffer.filter((delta) => Number(delta.farm_seq) > session.lastFarmSeq)
      const next = sync.buffer.find((delta) => Number(delta.farm_seq) === session.lastFarmSeq + 1)
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
        const result = await syncFarm(view.ownerUid, session.lastFarmSeq)
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
      if (Number(delta?.owner_uid) !== Number(session.viewingOwnerUid)) return false
      if (syncing && syncing.view.ownerUid === Number(session.viewingOwnerUid)) {
        return requestSync(delta)
      }
      if (applyDelta(delta)) return true
      return requestSync(delta)
    },
    applySync,
    dispose() {
      disposed = true
      syncing = null
    },
  }
}
