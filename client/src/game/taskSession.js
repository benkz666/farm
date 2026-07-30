/**
 * 任务请求/会话协调：隔离登录/重连/dispose，latest-wins，
 * 并避免晚到 TaskList 覆盖已应用的 TaskNotify。
 */
import { applyTaskNotify, mapServerTasks } from './taskSync.js'

/**
 * 列表到达时若期间有 push：保留 rev > epochAtStart 的本地任务，其余用列表。
 * @param {Array<object>} current
 * @param {Array<object>} incoming
 * @param {number} epochAtStart
 */
export function mergeListRespectingPush(current, incoming, epochAtStart) {
  const curById = new Map((Array.isArray(current) ? current : []).map((t) => [Number(t.id), t]))
  return (Array.isArray(incoming) ? incoming : []).map((inc) => {
    const cur = curById.get(Number(inc.id))
    if (cur && Number(cur.rev || 0) > Number(epochAtStart)) return cur
    return { ...inc, rev: 0 }
  })
}

/**
 * @typedef {{
 *   ok: boolean,
 *   tasks?: unknown,
 *   resetAt?: number,
 *   err?: number,
 *   error?: unknown,
 * }} TaskListFetchResult
 *
 * @typedef {{
 *   applied: boolean,
 *   ok: boolean,
 *   resetAt: number,
 *   contextValid: boolean,
 *   reason?: string,
 *   pushMerged?: boolean,
 *   err?: number,
 *   error?: unknown,
 * }} TaskListApplyOutcome
 */

export function createTaskSession() {
  let disposed = false
  let sessionGen = 0
  let listReqId = 0
  let pushEpoch = 0

  function invalidate() {
    sessionGen += 1
    listReqId = 0
    pushEpoch = 0
    return sessionGen
  }

  function dispose() {
    disposed = true
    sessionGen += 1
    listReqId = 0
  }

  /**
   * 拉取 TaskList：先只取未应用数据，校验会话/client/latest/push epoch 后再写 state。
   * @param {{
   *   client: object|null|undefined,
   *   getCurrentClient: () => object|null|undefined,
   *   fetch: (client: object) => Promise<TaskListFetchResult>,
   *   getTasks: () => Array<object>,
   *   setTasks: (tasks: Array<object>) => void,
   *   setResetAt: (resetAt: number) => void,
   *   afterApply?: () => void,
   * }} opts
   * @returns {Promise<TaskListApplyOutcome>}
   */
  async function refreshTaskList(opts) {
    const {
      client,
      getCurrentClient,
      fetch,
      getTasks,
      setTasks,
      setResetAt,
      afterApply,
    } = opts

    if (disposed) {
      return { applied: false, ok: false, resetAt: 0, contextValid: false, reason: 'disposed' }
    }
    if (!client) {
      return { applied: false, ok: false, resetAt: 0, contextValid: false, reason: 'no_client' }
    }

    const session = sessionGen
    const reqId = ++listReqId
    const epochAtStart = pushEpoch
    const capturedClient = client

    /** @type {TaskListFetchResult} */
    let raw
    try {
      raw = await fetch(capturedClient)
    } catch (error) {
      raw = { ok: false, error }
    }

    if (disposed) {
      return { applied: false, ok: !!raw?.ok, resetAt: 0, contextValid: false, reason: 'disposed' }
    }
    if (session !== sessionGen) {
      return { applied: false, ok: !!raw?.ok, resetAt: 0, contextValid: false, reason: 'session' }
    }
    if (getCurrentClient() !== capturedClient) {
      return { applied: false, ok: !!raw?.ok, resetAt: 0, contextValid: false, reason: 'client' }
    }
    if (reqId !== listReqId) {
      return { applied: false, ok: !!raw?.ok, resetAt: 0, contextValid: false, reason: 'stale_request' }
    }

    if (!raw?.ok) {
      return {
        applied: false,
        ok: false,
        resetAt: 0,
        contextValid: true,
        reason: 'fetch_failed',
        err: raw?.err,
        error: raw?.error,
      }
    }

    const mapped = mapServerTasks(raw.tasks).map((t) => ({ ...t, rev: 0 }))
    const resetAt = Number(raw.resetAt) || 0
    const pushMerged = pushEpoch !== epochAtStart
    const nextTasks = pushMerged
      ? mergeListRespectingPush(getTasks(), mapped, epochAtStart)
      : mapped

    setTasks(nextTasks)
    setResetAt(resetAt)
    afterApply?.()

    return {
      applied: true,
      ok: true,
      resetAt,
      contextValid: true,
      pushMerged,
    }
  }

  /**
   * 应用 TaskNotify：提升 pushEpoch，并给任务打 rev，供晚到 list 合并。
   * @param {object} payload
   * @param {{
   *   getTasks: () => Array<object>,
   *   setTasks: (tasks: Array<object>) => void,
   *   afterApply?: () => void,
   * }} sinks
   */
  function applyNotify(payload, sinks) {
    if (disposed) return false
    pushEpoch += 1
    const rev = pushEpoch
    const next = applyTaskNotify(sinks.getTasks(), payload)
    const id = Number(payload?.id)
    const hit = next.find((t) => Number(t.id) === id)
    if (hit) hit.rev = rev
    sinks.setTasks(next)
    sinks.afterApply?.()
    return true
  }

  return {
    invalidate,
    dispose,
    refreshTaskList,
    applyTaskNotify: applyNotify,
    get disposed() {
      return disposed
    },
    get sessionGen() {
      return sessionGen
    },
    get pushEpoch() {
      return pushEpoch
    },
    get listReqId() {
      return listReqId
    },
  }
}
