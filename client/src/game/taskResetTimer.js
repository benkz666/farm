/**
 * 任务自然日跨日：一次性 timer，不用 setInterval。
 * 处理过期 reset_at / 时钟偏差 / 失败重试，避免 0ms 死循环与重复 timer。
 *
 * refresh outcome：
 * - success：按 reset_at 排下一枪
 * - failure：真实网络/协议失败，排 5s retry
 * - stale：被取代/会话失效/dispose，不改动当前 timer
 */

/** 已过期或偏差过小时的最小等待，避免紧循环。 */
export const TASK_RESET_MIN_DELAY_MS = 1_000
/** TaskList 失败后的重试间隔。 */
export const TASK_RESET_RETRY_DELAY_MS = 5_000

/**
 * @typedef {'success'|'failure'|'stale'} TaskRefreshStatus
 * @typedef {{
 *   status: TaskRefreshStatus,
 *   resetAt?: number,
 *   reason?: string,
 *   err?: number,
 *   error?: unknown,
 * }} TaskRefreshResult
 */

/**
 * @param {unknown} resetAt
 * @param {number} nowMs
 * @param {{ minDelayMs?: number }} [opts]
 * @returns {number|null} 延迟毫秒；非法 reset_at 返回 null（不调度）
 */
export function taskResetDelayMs(resetAt, nowMs, opts = {}) {
  const minDelayMs = opts.minDelayMs ?? TASK_RESET_MIN_DELAY_MS
  const at = Number(resetAt)
  if (!Number.isFinite(at) || at <= 0) return null
  const delay = at - Number(nowMs)
  return Math.max(delay, minDelayMs)
}

/**
 * 将 taskSession outcome 规范为 scheduler 可消费的三态结果。
 * @param {{
 *   applied?: boolean,
 *   ok?: boolean,
 *   resetAt?: number,
 *   contextValid?: boolean,
 *   reason?: string,
 *   err?: number,
 *   error?: unknown,
 *   status?: TaskRefreshStatus,
 * }|null|undefined} outcome
 * @returns {TaskRefreshResult}
 */
export function taskRefreshResultFromOutcome(outcome) {
  if (!outcome) return { status: 'stale', reason: 'empty' }
  if (outcome.status === 'success' || outcome.status === 'failure' || outcome.status === 'stale') {
    return {
      status: outcome.status,
      resetAt: outcome.resetAt,
      reason: outcome.reason,
      err: outcome.err,
      error: outcome.error,
    }
  }
  // 显式 contextValid:false → stale；未提供时不要误判为 stale
  if (outcome.contextValid === false) {
    return { status: 'stale', reason: outcome.reason || 'context_invalid' }
  }
  if (outcome.ok && (outcome.applied === true || outcome.applied == null)) {
    return { status: 'success', resetAt: Number(outcome.resetAt) || 0 }
  }
  if (outcome.ok === false || outcome.reason === 'fetch_failed') {
    return {
      status: 'failure',
      reason: outcome.reason || 'fetch_failed',
      err: outcome.err,
      error: outcome.error,
    }
  }
  return { status: 'stale', reason: outcome.reason || 'context_invalid' }
}

/**
 * @typedef {{
 *   setTimeout: typeof setTimeout,
 *   clearTimeout: typeof clearTimeout,
 *   now?: () => number,
 *   refresh: () => Promise<TaskRefreshResult>,
 *   minDelayMs?: number,
 *   retryDelayMs?: number,
 * }} TaskResetSchedulerOptions
 */

/**
 * @param {TaskResetSchedulerOptions} options
 */
export function createTaskResetScheduler(options) {
  const setTimeoutFn = options.setTimeout
  const clearTimeoutFn = options.clearTimeout
  const now = options.now ?? (() => Date.now())
  const refresh = options.refresh
  const minDelayMs = options.minDelayMs ?? TASK_RESET_MIN_DELAY_MS
  const retryDelayMs = options.retryDelayMs ?? TASK_RESET_RETRY_DELAY_MS

  /** @type {ReturnType<typeof setTimeout>|null} */
  let timerId = null
  let disposed = false

  function clear() {
    if (timerId != null) {
      clearTimeoutFn(timerId)
      timerId = null
    }
  }

  function dispose() {
    disposed = true
    clear()
  }

  /**
   * @param {unknown} resetAt
   * @param {{ failed?: boolean }} [opts]
   */
  function schedule(resetAt, opts = {}) {
    if (disposed) return
    clear()
    let delay
    if (opts.failed) {
      delay = retryDelayMs
    } else {
      delay = taskResetDelayMs(resetAt, now(), { minDelayMs })
      if (delay == null) return
    }
    timerId = setTimeoutFn(() => {
      timerId = null
      void runRefresh()
    }, delay)
  }

  async function runRefresh() {
    if (disposed) return
    try {
      const result = taskRefreshResultFromOutcome(await refresh())
      if (disposed) return
      if (result.status === 'success') {
        schedule(result.resetAt)
      } else if (result.status === 'failure') {
        schedule(0, { failed: true })
      }
      // stale / cancelled：不改动当前 timer（通常已被更新请求重排）
    } catch {
      if (!disposed) schedule(0, { failed: true })
    }
  }

  return {
    scheduleFromResetAt(resetAt) {
      schedule(resetAt)
    },
    /** 初次拉取或跨日刷新失败后的有界 5s one-shot 重试。 */
    scheduleRetry() {
      schedule(0, { failed: true })
    },
    clear,
    dispose,
    get disposed() {
      return disposed
    },
  }
}

/**
 * 根据三态结果排程：success→reset_at；failure→5s retry；stale→不动 timer。
 * @param {{ disposed?: boolean, scheduleFromResetAt: Function, scheduleRetry: Function }|null|undefined} scheduler
 * @param {TaskRefreshResult|{ ok?: boolean, resetAt?: number, status?: TaskRefreshStatus }|null|undefined} result
 * @returns {boolean} 是否实际改动了 timer
 */
export function applyTaskListSchedule(scheduler, result) {
  if (!scheduler || scheduler.disposed) return false
  const normalized = taskRefreshResultFromOutcome(result)
  if (normalized.status === 'success') {
    scheduler.scheduleFromResetAt(normalized.resetAt)
    return true
  }
  if (normalized.status === 'failure') {
    scheduler.scheduleRetry()
    return true
  }
  return false
}
