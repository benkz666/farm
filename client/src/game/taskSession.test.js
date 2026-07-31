import test from 'node:test'
import assert from 'node:assert/strict'

import { createTaskSession, mergeListRespectingPush } from './taskSession.js'

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

function makeSink() {
  const writes = []
  let tasks = []
  let resetAt = 0
  return {
    writes,
    getTasks: () => tasks,
    getResetAt: () => resetAt,
    setTasks: (next) => {
      tasks = next
      writes.push({ type: 'tasks', tasks: next.map((t) => ({ ...t })) })
    },
    setResetAt: (v) => {
      resetAt = v
      writes.push({ type: 'resetAt', resetAt: v })
    },
    afterApply: () => {
      writes.push({ type: 'hud' })
    },
  }
}

test('旧登录响应不污染新登录会话', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const clientA = { id: 'A' }
  const clientB = { id: 'B' }
  let current = clientA
  const d = deferred()

  session.invalidate()
  const p1 = session.refreshTaskList({
    client: clientA,
    getCurrentClient: () => current,
    fetch: async () => d.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })

  session.invalidate()
  current = clientB
  const p2 = session.refreshTaskList({
    client: clientB,
    getCurrentClient: () => current,
    fetch: async () => ({
      ok: true,
      tasks: [{ id: 4, day_key: 20260731, title: '每日登录', progress: 1, target: 1, reward_coin: 100, claimed: false }],
      resetAt: 2000,
    }),
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })

  const r2 = await p2
  assert.equal(r2.applied, true)
  assert.equal(sink.getResetAt(), 2000)
  assert.equal(sink.getTasks()[0].id, 4)

  d.resolve({
    ok: true,
    tasks: [{ id: 1, day_key: 20260730, title: '旧播种', progress: 0, target: 1, reward_coin: 20, claimed: false }],
    resetAt: 999,
  })
  const r1 = await p1
  assert.equal(r1.applied, false)
  assert.equal(r1.reason, 'session')
  assert.equal(sink.getResetAt(), 2000)
  assert.equal(sink.getTasks()[0].id, 4)
  assert.equal(sink.getTasks()[0].title, '每日登录')
})

test('重连前响应不污染重连后（即使复用同一 client）', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'same' }
  const d = deferred()

  session.invalidate()
  const before = session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => d.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })

  // 重连恢复：即使复用 client 也必须使旧请求失效
  session.invalidate()
  const after = await session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => ({
      ok: true,
      tasks: [{ id: 2, day_key: 20260731, title: '完成一次收获', progress: 1, target: 1, reward_coin: 30, claimed: false }],
      resetAt: 5000,
    }),
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })
  assert.equal(after.applied, true)

  d.resolve({
    ok: true,
    tasks: [{ id: 1, day_key: 20260730, title: '重连前旧列表', progress: 0, target: 1, reward_coin: 20, claimed: false }],
    resetAt: 1,
  })
  const stale = await before
  assert.equal(stale.applied, false)
  assert.equal(stale.reason, 'session')
  assert.equal(sink.getTasks()[0].title, '完成一次收获')
  assert.equal(sink.getResetAt(), 5000)
})

test('晚到旧 TaskList 不覆盖已到的 TaskNotify', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'c' }
  const d = deferred()

  session.invalidate()
  await session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => ({
      ok: true,
      tasks: [{ id: 1, day_key: 20260731, title: '完成一次播种', progress: 0, target: 1, reward_coin: 20, claimed: false }],
      resetAt: 8000,
    }),
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })
  sink.writes.length = 0

  const listP = session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => d.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })

  const notified = session.applyTaskNotify(
    { id: 1, day_key: 20260731, title: '完成一次播种', progress: 1, target: 1, reward_coin: 20, claimed: false },
    { getTasks: sink.getTasks, setTasks: sink.setTasks, afterApply: sink.afterApply },
  )
  assert.equal(notified, true)
  assert.equal(sink.getTasks()[0].progress, 1)
  assert.equal(sink.getTasks()[0].done, true)

  d.resolve({
    ok: true,
    tasks: [
      { id: 1, day_key: 20260731, title: '完成一次播种', progress: 0, target: 1, reward_coin: 20, claimed: false },
      { id: 2, day_key: 20260731, title: '完成一次收获', progress: 0, target: 1, reward_coin: 30, claimed: false },
    ],
    resetAt: 8000,
  })
  const listR = await listP
  assert.equal(listR.applied, true)
  assert.equal(listR.pushMerged, true)
  assert.equal(sink.getTasks().find((t) => t.id === 1).progress, 1, 'notify 进度应保留')
  assert.equal(sink.getTasks().find((t) => t.id === 2).progress, 0, '列表其它任务仍应用')
  assert.equal(sink.getResetAt(), 8000)
})

test('跨日延迟 TaskNotify 不进入新一天任务板', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'c' }

  session.invalidate()
  const result = await session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => ({
      ok: true,
      tasks: [{ id: 5, day_key: 20260801, title: '浇水 10 次', progress: 0, target: 10, reward_coin: 200, claimed: false }],
      resetAt: 9000,
    }),
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })
  assert.equal(result.applied, true)
  assert.equal(session.currentDayKey, 20260801)

  const applied = session.applyTaskNotify(
    { id: 1, day_key: 20260731, title: '播种 6 次', progress: 5, target: 6, reward_coin: 200, claimed: false },
    { getTasks: sink.getTasks, setTasks: sink.setTasks, afterApply: sink.afterApply },
  )
  assert.equal(applied, false)
  assert.deepEqual(sink.getTasks().map((task) => task.id), [5])
})

test('dispose 后无 state/HUD 写入', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'c' }
  const d = deferred()

  session.invalidate()
  const p = session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => d.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })

  session.dispose()
  d.resolve({
    ok: true,
    tasks: [{ id: 1, day_key: 20260731, title: 'x', progress: 1, target: 1, reward_coin: 20, claimed: false }],
    resetAt: 9,
  })
  const r = await p
  assert.equal(r.applied, false)
  assert.equal(r.reason, 'disposed')
  assert.equal(sink.writes.length, 0)
  assert.equal(session.applyTaskNotify({ id: 1, day_key: 20260731, progress: 1, target: 1 }, {
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    afterApply: sink.afterApply,
  }), false)
})

test('同会话乱序 latest-wins：旧响应不覆盖新请求', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'c' }
  const d1 = deferred()
  const d2 = deferred()

  session.invalidate()
  const p1 = session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => d1.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })
  const p2 = session.refreshTaskList({
    client,
    force: true,
    getCurrentClient: () => client,
    fetch: async () => d2.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })

  d2.resolve({
    ok: true,
    tasks: [{ id: 3, day_key: 20260731, title: '拜访', progress: 1, target: 1, reward_coin: 40, claimed: false }],
    resetAt: 300,
  })
  assert.equal((await p2).applied, true)

  d1.resolve({
    ok: true,
    tasks: [{ id: 1, day_key: 20260731, title: '旧请求', progress: 0, target: 1, reward_coin: 20, claimed: false }],
    resetAt: 100,
  })
  const r1 = await p1
  assert.equal(r1.applied, false)
  assert.equal(r1.reason, 'stale_request')
  assert.equal(sink.getTasks()[0].id, 3)
  assert.equal(sink.getResetAt(), 300)
})

test('同会话同时刷新复用一条在途 TaskList 请求', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'c' }
  const d = deferred()
  let fetchCalls = 0
  const options = {
    client,
    getCurrentClient: () => client,
    fetch: async () => {
      fetchCalls += 1
      return d.promise
    },
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  }

  session.invalidate()
  const loginRefresh = session.refreshTaskList(options)
  const panelRefresh = session.refreshTaskList(options)
  assert.equal(fetchCalls, 1)

  d.resolve({
    ok: true,
    tasks: [{ id: 4, day_key: 20260731, title: '每日登录', progress: 1, target: 1, reward_coin: 100, claimed: false }],
    resetAt: 9000,
  })
  const [loginResult, panelResult] = await Promise.all([loginRefresh, panelRefresh])
  assert.equal(loginResult.applied, true)
  assert.equal(panelResult.applied, true)
  assert.equal(fetchCalls, 1)
  assert.equal(sink.getResetAt(), 9000)
  assert.equal(sink.writes.filter((item) => item.type === 'tasks').length, 1)
})

test('client 已切换时旧响应不应用', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const clientA = { id: 'A' }
  const clientB = { id: 'B' }
  let current = clientA
  const d = deferred()

  session.invalidate()
  const p = session.refreshTaskList({
    client: clientA,
    getCurrentClient: () => current,
    fetch: async () => d.promise,
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })
  current = clientB
  d.resolve({
    ok: true,
    tasks: [{ id: 1, day_key: 20260731, title: 'x', progress: 0, target: 1, reward_coin: 20, claimed: false }],
    resetAt: 1,
  })
  const r = await p
  assert.equal(r.applied, false)
  assert.equal(r.reason, 'client')
  assert.equal(sink.writes.length, 0)
})

test('mergeListRespectingPush：仅保留请求开始后被 push 更新的任务', () => {
  const merged = mergeListRespectingPush(
    [
      { id: 1, dayKey: 20260731, progress: 1, rev: 3 },
      { id: 2, dayKey: 20260731, progress: 0, rev: 0 },
    ],
    [
      { id: 1, dayKey: 20260731, progress: 0, rev: 0 },
      { id: 2, dayKey: 20260731, progress: 1, rev: 0 },
      { id: 3, dayKey: 20260731, progress: 0, rev: 0 },
    ],
    2,
  )
  assert.equal(merged.find((t) => t.id === 1).progress, 1)
  assert.equal(merged.find((t) => t.id === 2).progress, 1)
  assert.equal(merged.find((t) => t.id === 3).progress, 0)
})

test('mergeListRespectingPush：跨日旧 push 不覆盖新列表', () => {
  const merged = mergeListRespectingPush(
    [{ id: 1, dayKey: 20260731, progress: 6, rev: 3 }],
    [{ id: 1, dayKey: 20260801, progress: 0, rev: 0 }],
    2,
  )
  assert.equal(merged[0].dayKey, 20260801)
  assert.equal(merged[0].progress, 0)
})

test('contextValid 失败时可供上层安排重试，但不写 tasks', async () => {
  const session = createTaskSession()
  const sink = makeSink()
  const client = { id: 'c' }
  session.invalidate()
  const r = await session.refreshTaskList({
    client,
    getCurrentClient: () => client,
    fetch: async () => ({ ok: false, err: 500 }),
    getTasks: sink.getTasks,
    setTasks: sink.setTasks,
    setResetAt: sink.setResetAt,
    afterApply: sink.afterApply,
  })
  assert.equal(r.applied, false)
  assert.equal(r.ok, false)
  assert.equal(r.contextValid, true)
  assert.equal(sink.writes.length, 0)
})
