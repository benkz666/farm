import test from 'node:test'
import assert from 'node:assert/strict'

import {
  TASK_RESET_MIN_DELAY_MS,
  TASK_RESET_RETRY_DELAY_MS,
  applyTaskListSchedule,
  createTaskResetScheduler,
  taskRefreshResultFromOutcome,
  taskResetDelayMs,
} from './taskResetTimer.js'

function deferred() {
  /** @type {(v: any) => void} */
  let resolve
  /** @type {(e?: any) => void} */
  let reject
  const promise = new Promise((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

test('taskResetDelayMs：未来 reset_at 用剩余毫秒', () => {
  assert.equal(taskResetDelayMs(10_000, 3_000), 7_000)
})

test('taskResetDelayMs：已过期或时钟偏差至少用最小延迟，避免 0ms', () => {
  assert.equal(taskResetDelayMs(1_000, 5_000), TASK_RESET_MIN_DELAY_MS)
  assert.equal(taskResetDelayMs(5_000, 5_000), TASK_RESET_MIN_DELAY_MS)
  assert.equal(taskResetDelayMs(5_100, 5_000), TASK_RESET_MIN_DELAY_MS)
})

test('taskResetDelayMs：非法 reset_at 不调度', () => {
  assert.equal(taskResetDelayMs(0, 1), null)
  assert.equal(taskResetDelayMs(NaN, 1), null)
  assert.equal(taskResetDelayMs(undefined, 1), null)
})

test('createTaskResetScheduler：一次只挂一个 timer，重复 schedule 会替换', () => {
  const timers = new Map()
  let nextId = 1
  const scheduled = []
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: (fn, ms) => {
      const id = nextId++
      timers.set(id, { fn, ms })
      scheduled.push(ms)
      return id
    },
    clearTimeout: (id) => { timers.delete(id) },
    refresh: async () => ({ status: 'success', resetAt: 90_000 }),
  })

  scheduler.scheduleFromResetAt(5_000)
  scheduler.scheduleFromResetAt(8_000)

  assert.equal(timers.size, 1)
  assert.deepEqual(scheduled, [4_000, 7_000])
  scheduler.dispose()
  assert.equal(timers.size, 0)
})

test('createTaskResetScheduler：回调成功后按新 reset_at 再排一次', async () => {
  const timers = new Map()
  let nextId = 1
  let now = 1_000
  const refreshCalls = []
  const scheduler = createTaskResetScheduler({
    now: () => now,
    setTimeout: (fn, ms) => {
      const id = nextId++
      timers.set(id, {
        ms,
        fn: async () => {
          timers.delete(id)
          await fn()
        },
      })
      return id
    },
    clearTimeout: (id) => { timers.delete(id) },
    refresh: async () => {
      refreshCalls.push(now)
      return { status: 'success', resetAt: 50_000 }
    },
  })

  scheduler.scheduleFromResetAt(2_000)
  assert.equal(timers.size, 1)
  const first = [...timers.values()][0]
  assert.equal(first.ms, 1_000)

  now = 2_000
  await first.fn()
  assert.deepEqual(refreshCalls, [2_000])
  assert.equal(timers.size, 1)
  const second = [...timers.values()][0]
  assert.equal(second.ms, 48_000)

  scheduler.dispose()
})

function makeTimerEnv() {
  const timers = new Map()
  let nextId = 1
  return {
    timers,
    setTimeout: (fn, ms) => {
      const id = nextId++
      timers.set(id, {
        ms,
        fn: async () => {
          timers.delete(id)
          await fn()
        },
      })
      return id
    },
    clearTimeout: (id) => { timers.delete(id) },
  }
}

test('createTaskResetScheduler：真实失败用重试延迟，且 dispose 后不再排程', async () => {
  const env = makeTimerEnv()
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: async () => ({ status: 'failure' }),
  })

  scheduler.scheduleFromResetAt(1_500)
  const first = [...env.timers.values()][0]
  await first.fn()
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, TASK_RESET_RETRY_DELAY_MS)

  scheduler.dispose()
  assert.equal(env.timers.size, 0)
  scheduler.scheduleFromResetAt(9_000)
  assert.equal(env.timers.size, 0)
})

test('scheduleRetry：初次失败安排有界 5s one-shot，不产生第二个 timer', () => {
  const env = makeTimerEnv()
  const scheduled = []
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: (fn, ms) => {
      scheduled.push(ms)
      return env.setTimeout(fn, ms)
    },
    clearTimeout: env.clearTimeout,
    refresh: async () => ({ status: 'success', resetAt: 90_000 }),
  })

  scheduler.scheduleRetry()
  scheduler.scheduleRetry()
  assert.equal(env.timers.size, 1)
  assert.deepEqual(scheduled, [TASK_RESET_RETRY_DELAY_MS, TASK_RESET_RETRY_DELAY_MS])
  assert.equal([...env.timers.values()][0].ms, TASK_RESET_RETRY_DELAY_MS)
  scheduler.dispose()
})

test('dispose 发生在异步 refresh 未完成时，resolve 后不再安排', async () => {
  const env = makeTimerEnv()
  /** @type {(v: any) => void} */
  let resolveRefresh
  const refreshPromise = new Promise((resolve) => {
    resolveRefresh = resolve
  })
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: () => refreshPromise,
  })

  scheduler.scheduleFromResetAt(2_000)
  const pending = [...env.timers.values()][0].fn()
  scheduler.dispose()
  resolveRefresh({ status: 'success', resetAt: 50_000 })
  await pending

  assert.equal(env.timers.size, 0)
  scheduler.scheduleFromResetAt(9_000)
  assert.equal(env.timers.size, 0)
})

test('dispose 发生在异步 refresh 未完成时，reject 后不再安排', async () => {
  const env = makeTimerEnv()
  /** @type {(e?: Error) => void} */
  let rejectRefresh
  const refreshPromise = new Promise((_, reject) => {
    rejectRefresh = reject
  })
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: () => refreshPromise,
  })

  scheduler.scheduleFromResetAt(2_000)
  const pending = [...env.timers.values()][0].fn()
  scheduler.dispose()
  rejectRefresh(new Error('network'))
  await pending.catch(() => {})

  assert.equal(env.timers.size, 0)
})

test('applyTaskListSchedule：仅真实失败 retry；stale 不改 timer；成功按 reset_at', () => {
  const env = makeTimerEnv()
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: async () => ({ status: 'success', resetAt: 90_000 }),
  })

  assert.equal(applyTaskListSchedule(scheduler, { status: 'failure' }), true)
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, TASK_RESET_RETRY_DELAY_MS)

  assert.equal(applyTaskListSchedule(scheduler, { status: 'success', resetAt: 20_000 }), true)
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, 19_000)

  assert.equal(applyTaskListSchedule(scheduler, { status: 'stale' }), false)
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, 19_000)

  scheduler.dispose()
  assert.equal(applyTaskListSchedule(scheduler, { status: 'success', resetAt: 30_000 }), false)
  assert.equal(env.timers.size, 0)
})

test('timer 请求 A 在途时手动 B 成功排远期 timer，A 变 stale 不得覆盖', async () => {
  const env = makeTimerEnv()
  let now = 1_000
  const dA = deferred()
  const scheduler = createTaskResetScheduler({
    now: () => now,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: () => dA.promise,
  })

  scheduler.scheduleFromResetAt(2_000)
  now = 2_000
  const pendingA = [...env.timers.values()][0].fn()

  // 手动 B 先成功，按远期 reset_at 排程
  scheduler.scheduleFromResetAt(100_000)
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, 98_000)

  dA.resolve({ status: 'stale', reason: 'stale_request' })
  await pendingA

  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, 98_000, 'stale A 不得改写 B 的远期 timer')
  scheduler.dispose()
})

test('timer refresh 返回 context invalid/stale 不得 retry', async () => {
  const env = makeTimerEnv()
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: async () => ({ status: 'stale', reason: 'disposed' }),
  })

  scheduler.scheduleFromResetAt(2_000)
  await [...env.timers.values()][0].fn()
  assert.equal(env.timers.size, 0, 'stale 后不应再挂 retry timer')
  scheduler.dispose()
})

test('timer refresh 真实失败仍 5s retry', async () => {
  const env = makeTimerEnv()
  const scheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: async () => ({ status: 'failure', err: 500 }),
  })

  scheduler.scheduleFromResetAt(2_000)
  await [...env.timers.values()][0].fn()
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, TASK_RESET_RETRY_DELAY_MS)
  scheduler.dispose()
})

test('taskRefreshResultFromOutcome：区分 success / failure / stale', () => {
  assert.deepEqual(
    taskRefreshResultFromOutcome({ applied: true, ok: true, resetAt: 9, contextValid: true }),
    { status: 'success', resetAt: 9 },
  )
  assert.equal(
    taskRefreshResultFromOutcome({
      applied: false, ok: false, resetAt: 0, contextValid: true, reason: 'fetch_failed', err: 500,
    }).status,
    'failure',
  )
  assert.equal(
    taskRefreshResultFromOutcome({
      applied: false, ok: false, resetAt: 0, contextValid: false, reason: 'stale_request',
    }).status,
    'stale',
  )
  assert.equal(
    taskRefreshResultFromOutcome({
      applied: false, ok: true, resetAt: 0, contextValid: false, reason: 'disposed',
    }).status,
    'stale',
  )
})

test('dispose 后新 scheduler 可重建，旧实例 resolve 不干扰', async () => {
  const env = makeTimerEnv()
  /** @type {(v: any) => void} */
  let resolveOld
  const oldRefresh = new Promise((resolve) => {
    resolveOld = resolve
  })

  const oldScheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: () => oldRefresh,
  })
  oldScheduler.scheduleFromResetAt(2_000)
  const oldPending = [...env.timers.values()][0].fn()
  oldScheduler.dispose()
  assert.equal(env.timers.size, 0)

  const newScheduler = createTaskResetScheduler({
    now: () => 1_000,
    setTimeout: env.setTimeout,
    clearTimeout: env.clearTimeout,
    refresh: async () => ({ status: 'success', resetAt: 80_000 }),
  })
  newScheduler.scheduleFromResetAt(5_000)
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, 4_000)

  resolveOld({ status: 'success', resetAt: 99_999 })
  await oldPending
  assert.equal(env.timers.size, 1)
  assert.equal([...env.timers.values()][0].ms, 4_000)

  newScheduler.dispose()
  assert.equal(env.timers.size, 0)
})
