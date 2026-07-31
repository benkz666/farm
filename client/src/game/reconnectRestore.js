/**
 * 断线恢复接线（可单测）：权威 EnterFarm 覆盖 + restore/fail 订阅。
 * 不依赖 Three.js / 页面启动副作用；依赖由调用方注入。
 */
import { wireUid, wireUint64 } from '../net/jsonSafe.js'

/** @type {WeakMap<object, () => void>} */
const boundDisposeByClient = new WeakMap()

/**
 * @typedef {{
 *   state: object,
 *   applyPatch: (state: object, payload: object, opts?: { farmViewOnly?: boolean }) => void,
 *   setFarmView: (view: { ownerUid: string|number, farmSeq: string|number, relation: 'SELF'|'FRIEND', ownerName?: string|null }) => void,
 *   setOnlineBusy?: (busy: boolean) => void,
 *   getSelfUid?: () => number|null|undefined,
 *   refreshUI?: (info: { relation: 'SELF'|'FRIEND', visiting: boolean }) => void,
 *   toast?: (msg: string, type?: string) => void,
 * }} ApplyDeps
 *
 * @typedef {ApplyDeps & {
 *   client: {
 *     getResumeContext?: () => { resume_farm_uid: string|number, resume_farm_seq: string|number },
 *     onFarmRestored: (h: Function) => () => void,
 *     onFarmRestoreFailed: (h: Function) => () => void,
 *   },
 *   session: { viewingOwnerUid?: string|number|null, relation?: 'SELF'|'FRIEND'|null, lastFarmSeq?: string|number },
 *   leaveOnline: () => void,
 *   fail: (msg: string) => void,
 *   errText: (err: number) => string,
 *   onOfflineCleanup?: (reason: Error|object) => void,
 *   onRestored?: () => void,
 * }} BindDeps
 */

/**
 * 丢弃未确认本地态，以 EnterFarm 权威快照强制覆盖。
 * @param {ApplyDeps} deps
 * @param {{ err?: number, payload?: object }} enterEnv
 * @param {{ toast?: string, fallbackOwnerUid?: number, ownerName?: string }} [opts]
 */
export function applyAuthoritativeFarmEnter(deps, enterEnv, opts = {}) {
  if (!enterEnv || enterEnv.err !== 0) {
    throw new Error(`applyAuthoritativeFarmEnter: enterFarm err=${enterEnv?.err}`)
  }
  deps.setOnlineBusy?.(false)
  const payload = enterEnv.payload || {}
  const snapshot = payload.snapshot || {}
  const relation = payload.relation === 'FRIEND' ? 'FRIEND' : 'SELF'
  const selfUid = wireUid(deps.getSelfUid?.()) ?? 0
  const snapshotOwnerUid = wireUid(snapshot.owner_uid) ?? 0
  const ownerUid = relation === 'SELF'
    ? (selfUid || snapshotOwnerUid || wireUid(opts.fallbackOwnerUid) || 0)
    : (snapshotOwnerUid || wireUid(opts.fallbackOwnerUid) || 0)
  const ownerName = relation === 'FRIEND'
    ? (opts.ownerName || snapshot.nickname || null)
    : null
  const farmSeq = wireUint64(payload.farm_seq)
  if (farmSeq == null) {
    throw new Error('applyAuthoritativeFarmEnter: invalid farm_seq')
  }
  deps.applyPatch(deps.state, payload, { farmViewOnly: relation === 'FRIEND' })
  deps.setFarmView({
    ownerUid,
    farmSeq,
    relation,
    ownerName,
    serverTime: Number(payload.server_time) || 0,
  })
  deps.refreshUI?.({ relation, visiting: relation === 'FRIEND' })
  if (opts.toast) deps.toast?.(opts.toast, 'ok')
}

/**
 * 绑定 NetClient 断线恢复回调。重复 bind 会先 dispose 旧订阅。
 * 恢复失败时 leaveOnline、dispose 自身订阅，并调用 onOfflineCleanup。
 *
 * @param {BindDeps} deps
 * @returns {{ dispose: () => void }}
 */
export function bindFarmReconnectRestore(deps) {
  const { client, session } = deps
  boundDisposeByClient.get(client)?.()

  /** @type {null|(() => void)} */
  let unsubRestored = null
  /** @type {null|(() => void)} */
  let unsubFailed = null

  function dispose() {
    unsubRestored?.()
    unsubFailed?.()
    unsubRestored = null
    unsubFailed = null
    if (boundDisposeByClient.get(client) === dispose) {
      boundDisposeByClient.delete(client)
    }
  }

  client.getResumeContext = () => ({
    // SELF 一律用协议保留值 0；不要把可能残留的好友 UID 带进重连恢复。
    resume_farm_uid: session.relation === 'FRIEND'
      ? (wireUid(session.viewingOwnerUid) ?? 0)
      : 0,
    resume_farm_seq: wireUint64(session.lastFarmSeq) ?? 0,
  })

  unsubRestored = client.onFarmRestored((enterEnv) => {
    try {
      applyAuthoritativeFarmEnter(deps, enterEnv, {
        toast: '连接已恢复，已同步农场状态',
      })
      deps.onRestored?.()
    } catch (error) {
      deps.fail(error instanceof Error ? error.message : String(error))
    }
  })

  unsubFailed = client.onFarmRestoreFailed((reason) => {
    deps.setOnlineBusy?.(false)
    deps.leaveOnline()
    dispose()
    deps.onOfflineCleanup?.(reason)
    const msg =
      reason && typeof reason === 'object' && 'err' in reason
        ? deps.errText(reason.err) || `恢复失败（${reason.err}）`
        : reason instanceof Error
          ? reason.message
          : String(reason)
    deps.fail(`连接中断且无法恢复：${msg}`)
  })

  boundDisposeByClient.set(client, dispose)
  return { dispose }
}
