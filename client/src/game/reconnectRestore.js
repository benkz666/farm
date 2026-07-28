/**
 * 断线恢复接线（可单测）：权威 EnterFarm 覆盖 + restore/fail 订阅。
 * 不依赖 Three.js / 页面启动副作用；依赖由调用方注入。
 */

/** @type {WeakMap<object, () => void>} */
const boundDisposeByClient = new WeakMap()

/**
 * @typedef {{
 *   state: object,
 *   applyPatch: (state: object, payload: object) => void,
 *   setFarmView: (view: { ownerUid: number, farmSeq: number, relation: 'SELF'|'FRIEND' }) => void,
 *   setOnlineBusy?: (busy: boolean) => void,
 *   getSelfUid?: () => number|null|undefined,
 *   refreshUI?: (info: { relation: 'SELF'|'FRIEND', visiting: boolean }) => void,
 *   toast?: (msg: string, type?: string) => void,
 * }} ApplyDeps
 *
 * @typedef {ApplyDeps & {
 *   client: {
 *     getResumeContext?: () => { resume_farm_uid: number, resume_farm_seq: number },
 *     onFarmRestored: (h: Function) => () => void,
 *     onFarmRestoreFailed: (h: Function) => () => void,
 *   },
 *   session: { viewingOwnerUid?: number|null, lastFarmSeq?: number },
 *   leaveOnline: () => void,
 *   fail: (msg: string) => void,
 *   errText: (err: number) => string,
 *   onOfflineCleanup?: () => void,
 * }} BindDeps
 */

/**
 * 丢弃未确认本地态，以 EnterFarm 权威快照强制覆盖。
 * @param {ApplyDeps} deps
 * @param {{ err?: number, payload?: object }} enterEnv
 * @param {{ toast?: string, fallbackOwnerUid?: number }} [opts]
 */
export function applyAuthoritativeFarmEnter(deps, enterEnv, opts = {}) {
  if (!enterEnv || enterEnv.err !== 0) {
    throw new Error(`applyAuthoritativeFarmEnter: enterFarm err=${enterEnv?.err}`)
  }
  deps.setOnlineBusy?.(false)
  const payload = enterEnv.payload || {}
  const snapshot = payload.snapshot || {}
  const ownerUid =
    Number(snapshot.owner_uid) ||
    Number(opts.fallbackOwnerUid) ||
    Number(deps.getSelfUid?.()) ||
    0
  const relation = payload.relation === 'FRIEND' ? 'FRIEND' : 'SELF'
  deps.applyPatch(deps.state, payload)
  deps.setFarmView({
    ownerUid,
    farmSeq: Number(payload.farm_seq) || 0,
    relation,
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
    resume_farm_uid: Number(session.viewingOwnerUid) || 0,
    resume_farm_seq: Number(session.lastFarmSeq) || 0,
  })

  unsubRestored = client.onFarmRestored((enterEnv) => {
    try {
      applyAuthoritativeFarmEnter(deps, enterEnv, {
        toast: '连接已恢复，已同步农场状态',
      })
    } catch (error) {
      deps.fail(error instanceof Error ? error.message : String(error))
    }
  })

  unsubFailed = client.onFarmRestoreFailed((reason) => {
    deps.setOnlineBusy?.(false)
    deps.leaveOnline()
    dispose()
    deps.onOfflineCleanup?.()
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
