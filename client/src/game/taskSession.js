/**
 * 任务请求/会话协调：隔离登录/重连/dispose，latest-wins，
 * 并避免晚到 TaskList 覆盖已应用的 TaskNotify。
 */
import { applyTaskNotify, mapServerTask, mapServerTasks } from './taskSync.js'

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
    if (
      cur &&
      Number(cur.dayKey) === Number(inc.dayKey) &&
      Number(cur.rev || 0) > Number(epochAtStart)
    ) return cur
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
  let currentDayKey = 0
  /** @type {{ client: object, session: number, promise: Promise<TaskListApplyOutcome> }|null} */
  let inFlight = null

  function invalidate() {
    sessionGen += 1
    listReqId = 0
    pushEpoch = 0
    currentDayKey = 0
    inFlight = null
    return sessionGen
  }

  function dispose() {
    disposed = true
    sessionGen += 1
    listReqId = 0
    currentDayKey = 0
    inFlight = null
  }

  /**
   * 同一会话、同一 client 的普通刷新复用正在进行的 TaskList 请求。force=true
   * 保留 latest-wins 能力，供确实需要越过在途请求的调用方使用。
   * @param {{
   *   client: object|null|undefined,
   *   getCurrentClient: () => object|null|undefined,
   *   fetch: (client: object) => Promise<TaskListFetchResult>,
   *   getTasks: () => Array<object>,
   *   setTasks: (tasks: Array<object>) => void,
   *   setResetAt: (resetAt: number) => void,
   *   afterApply?: () => void,
   *   force?: boolean,
   * }} opts
   * @returns {Promise<TaskListApplyOutcome>}
   */
  function refreshTaskList(opts) {
    if (disposed) {
      return Promise.resolve({ applied: false, ok: false, resetAt: 0, contextValid: false, reason: 'disposed' })
    }
    if (!opts.client) {
      return Promise.resolve({ applied: false, ok: false, resetAt: 0, contextValid: false, reason: 'no_client' })
    }
    if (!opts.force && inFlight?.client === opts.client && inFlight.session === sessionGen) {
      return inFlight.promise
    }

    const request = {
      client: opts.client,
      session: sessionGen,
      promise: executeTaskListRefresh(opts),
    }
    inFlight = request
    const clear = () => {
      if (inFlight === request) inFlight = null
    }
    void request.promise.then(clear, clear)
    return request.promise
  }

  /**
   * 拉取 TaskList：先只取未应用数据，校验会话/client/latest/push epoch 后再写 state。
   * @param {Parameters<typeof refreshTaskList>[0]} opts
   * @returns {Promise<TaskListApplyOutcome>}
   */
  async function executeTaskListRefresh(opts) {
    const {
      client,
      getCurrentClient,
      fetch,
      getTasks,
      setTasks,
      setResetAt,
      afterApply,
    } = opts

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
    const dayKey = Number(mapped[0]?.dayKey) || 0
    if (!dayKey || mapped.some((task) => Number(task.dayKey) !== dayKey)) {
      return {
        applied: false,
        ok: false,
        resetAt: 0,
        contextValid: true,
        reason: 'invalid_task_day',
      }
    }
    const resetAt = Number(raw.resetAt) || 0
    const pushMerged = pushEpoch !== epochAtStart
    const nextTasks = pushMerged
      ? mergeListRespectingPush(getTasks(), mapped, epochAtStart)
      : mapped

    setTasks(nextTasks)
    currentDayKey = dayKey
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
    const incoming = mapServerTask(payload)
    // 尚未取得 TaskList 或推送不属于当前任务板时直接丢弃。TaskList 是权威
    // 恢复路径，不能让跨午夜延迟到达的旧任务重新进入新一天的列表。
    if (!incoming || currentDayKey <= 0 || incoming.dayKey !== currentDayKey) return false
    const next = applyTaskNotify(sinks.getTasks(), payload)
    // TaskNotify 必须携带服务端完整定义；不完整包既不写 UI，也不改变
    // push epoch，后续完整 TaskList 仍可正常落地。
    if (!next) return false
    pushEpoch += 1
    const rev = pushEpoch
    const id = incoming.id
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
    get currentDayKey() {
      return currentDayKey
    },
  }
}
