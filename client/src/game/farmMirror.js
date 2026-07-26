import { applyPatch } from './applyPatch.js'

/**
 * 将当前房间的 FarmDelta 依序投影到本地状态；发现序列缺口时请求服务端补齐。
 * @param {{
 *   state: object,
 *   session: { viewingOwnerUid?: number|null, lastFarmSeq: number },
 *   syncFarm: (ownerUid: number, fromSeq: number) => Promise<object|undefined>,
 *   onApplied?: () => void,
 * }} options
 */
export function createFarmMirror({ state, session, syncFarm, onApplied = () => {} }) {
  let syncing = null

  async function applySync(result) {
    const payload = result?.payload ?? result
    if (!payload || typeof payload !== 'object') return false

    if (payload.snapshot && typeof payload.snapshot === 'object') {
      applyPatch(state, { snapshot: payload.snapshot })
      session.lastFarmSeq = Number(payload.farm_seq) || 0
      onApplied()
      return true
    }

    if (!Array.isArray(payload.deltas)) return false
    for (const delta of payload.deltas) {
      if (!applyDelta(delta)) return false
    }
    return true
  }

  function applyDelta(delta) {
    if (!delta || typeof delta !== 'object') return false
    const seq = Number(delta.farm_seq)
    if (!Number.isSafeInteger(seq) || seq !== session.lastFarmSeq + 1) return false
    applyPatch(state, delta)
    session.lastFarmSeq = seq
    onApplied()
    return true
  }

  async function requestSync() {
    if (syncing) return syncing
    const ownerUid = Number(session.viewingOwnerUid)
    const fromSeq = session.lastFarmSeq
    syncing = Promise.resolve(syncFarm(ownerUid, fromSeq))
      .then(applySync)
      .finally(() => {
        syncing = null
      })
    return syncing
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
