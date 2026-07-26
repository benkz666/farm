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
    const payload = result?.payload ?? result
    if (!payload || typeof payload !== 'object') return false
    if (!isCurrentView(requestedView)) return false

    if (payload.snapshot && typeof payload.snapshot === 'object') {
      if (Number(payload.snapshot.owner_uid) !== requestedView.ownerUid) return false
      applyPatch(state, { snapshot: payload.snapshot })
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
    if (!delta || typeof delta !== 'object') return false
    if (Number(delta.owner_uid) !== Number(session.viewingOwnerUid)) return false
    const seq = Number(delta.farm_seq)
    if (!Number.isSafeInteger(seq) || seq !== session.lastFarmSeq + 1) return false
    applyPatch(state, delta)
    session.lastFarmSeq = seq
    onApplied()
    return true
  }

  async function requestSync() {
    const view = currentView()
    if (syncing && syncing.view.ownerUid === view.ownerUid && syncing.view.generation === view.generation) {
      return syncing.promise
    }
    const fromSeq = session.lastFarmSeq
    const promise = Promise.resolve(syncFarm(view.ownerUid, fromSeq))
      .then((result) => applySync(result, view))
      .finally(() => {
        if (syncing?.promise === promise) syncing = null
      })
    syncing = { view, promise }
    return promise
  }

  return {
    async onDelta(delta) {
      if (Number(delta?.owner_uid) !== Number(session.viewingOwnerUid)) return false
      if (applyDelta(delta)) return true
      await requestSync()
      return false
    },
    applySync,
  }
}
