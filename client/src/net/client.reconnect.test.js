import test from 'node:test'
import assert from 'node:assert/strict'

import {
  NetClient,
  CMD_HANDSHAKE,
  CMD_ENTER_FARM,
  CMD_PING,
  CLIENT_CONFIG_VER,
} from './client.js'

class FakeWebSocket {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSING = 2
  static CLOSED = 3
  /** @type {FakeWebSocket[]} */
  static instances = []

  /**
   * @param {string} url
   * @param {string} [protocol]
   */
  constructor(url, protocol) {
    this.url = url
    this.protocol = protocol
    this.readyState = FakeWebSocket.CONNECTING
    /** @type {object[]} */
    this.sent = []
    this.onopen = null
    this.onclose = null
    this.onerror = null
    this.onmessage = null
    FakeWebSocket.instances.push(this)
  }

  open() {
    this.readyState = FakeWebSocket.OPEN
    this.onopen?.({ type: 'open' })
  }

  /**
   * @param {string} data
   */
  send(data) {
    if (this.readyState !== FakeWebSocket.OPEN) throw new Error('FakeWebSocket: not open')
    this.sent.push(JSON.parse(data))
  }

  close() {
    if (this.readyState === FakeWebSocket.CLOSED) return
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.({ type: 'close', code: 1000, reason: '' })
  }

  /**
   * @param {object} envelope
   */
  respond(envelope) {
    this.onmessage?.({ data: JSON.stringify(envelope) })
  }

  static reset() {
    FakeWebSocket.instances = []
  }

  static latest() {
    return FakeWebSocket.instances[FakeWebSocket.instances.length - 1]
  }
}

class VirtualClock {
  constructor() {
    this.now = 0
    this._nextId = 1
    /** @type {Map<number, { due: number, fn: Function }>} */
    this._timers = new Map()
  }

  setTimeout = (fn, ms) => {
    const id = this._nextId++
    this._timers.set(id, { due: this.now + Number(ms), fn })
    return id
  }

  clearTimeout = (id) => {
    this._timers.delete(id)
  }

  /**
   * @param {number} ms
   */
  advance(ms) {
    const target = this.now + ms
    for (;;) {
      let next = null
      for (const [id, timer] of this._timers) {
        if (timer.due <= target && (!next || timer.due < next.due || (timer.due === next.due && id < next.id))) {
          next = { id, due: timer.due, fn: timer.fn }
        }
      }
      if (!next) {
        this.now = target
        return
      }
      this.now = next.due
      this._timers.delete(next.id)
      next.fn()
    }
  }

  pendingCount() {
    return this._timers.size
  }
}

/**
 * @param {Partial<{
 *   requestTimeoutMs: number,
 *   reconnectBaseMs: number,
 *   reconnectMaxMs: number,
 *   random: () => number,
 *   getResumeContext: () => { resume_farm_uid: number, resume_farm_seq: number },
 * }>} [opts]
 */
function createClient(opts = {}) {
  const clock = new VirtualClock()
  FakeWebSocket.reset()
  const client = new NetClient({
    WebSocket: FakeWebSocket,
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
    random: opts.random ?? (() => 0),
    requestTimeoutMs: opts.requestTimeoutMs ?? 5_000,
    reconnectBaseMs: opts.reconnectBaseMs ?? 1_000,
    reconnectMaxMs: opts.reconnectMaxMs ?? 8_000,
    getResumeContext: opts.getResumeContext,
  })
  client.token = 'tok'
  client.uid = 42
  client.wsUrl = 'ws://test/ws'
  return { client, clock }
}

async function openClient(opts = {}) {
  const ctx = createClient(opts)
  const connecting = ctx.client.connect()
  FakeWebSocket.latest().open()
  await connecting
  return { ...ctx, ws: FakeWebSocket.latest() }
}

test('request 超时后从 pending 删除并 reject 明确错误', async () => {
  const { client, clock, ws } = await openClient({ requestTimeoutMs: 1_000 })
  const pending = client.request(CMD_PING, {})
  assert.equal(client._pending.size, 1)

  let rejected
  pending.catch((err) => {
    rejected = err
  })
  clock.advance(1_000)
  await Promise.resolve()

  assert.ok(rejected instanceof Error)
  assert.match(rejected.message, /timeout/i)
  assert.equal(client._pending.size, 0)
  assert.equal(clock.pendingCount(), 0)
  assert.equal(ws.sent.length, 1)
})

test('正常响应清除 request timer，无泄漏', async () => {
  const { client, clock, ws } = await openClient({ requestTimeoutMs: 5_000 })
  const pending = client.request(CMD_PING, {})
  const seq = ws.sent[0].client_seq
  ws.respond({ cmd: CMD_PING, client_seq: seq, err: 0, payload: {} })
  const env = await pending
  assert.equal(env.cmd, CMD_PING)
  assert.equal(client._pending.size, 0)
  assert.equal(clock.pendingCount(), 0)
  clock.advance(5_000)
  assert.equal(client._pending.size, 0)
})

test('send 抛错清除 timer 且不二次 reject', async () => {
  const { client, clock, ws } = await openClient({ requestTimeoutMs: 5_000 })
  ws.send = () => {
    throw new Error('send boom')
  }
  await assert.rejects(() => client.request(CMD_PING, {}), /send boom/)
  assert.equal(client._pending.size, 0)
  assert.equal(clock.pendingCount(), 0)
  clock.advance(5_000)
  assert.equal(client._pending.size, 0)
})

test('socket close 拒绝在途请求并清 request timer；意外断线会挂一个重连 timer', async () => {
  const { client, clock, ws } = await openClient({ requestTimeoutMs: 5_000 })
  const pending = client.request(CMD_PING, {})
  let rejected
  pending.catch((err) => {
    rejected = err
  })
  ws.close()
  await Promise.resolve()
  assert.ok(rejected instanceof Error)
  assert.match(rejected.message, /closed/i)
  assert.equal(client._pending.size, 0)
  // request timer 已清；仅剩一个 reconnect timer
  assert.equal(clock.pendingCount(), 1)
})

test('主动 close 拒绝在途请求并清 timer，不二次 reject', async () => {
  const { client, clock } = await openClient({ requestTimeoutMs: 5_000 })
  const pending = client.request(CMD_PING, {})
  let rejects = 0
  pending.catch(() => {
    rejects += 1
  })
  client.close()
  await Promise.resolve()
  assert.equal(rejects, 1)
  assert.equal(client._pending.size, 0)
  assert.equal(clock.pendingCount(), 0)
  clock.advance(5_000)
  assert.equal(rejects, 1)
})

test('意外断开后指数退避重连，同一时刻最多一个 reconnect timer', async () => {
  const delays = []
  const { client, clock, ws } = await openClient({
    reconnectBaseMs: 1_000,
    reconnectMaxMs: 8_000,
    random: () => 0.5, // mid jitter
  })
  const originalSetTimeout = client._setTimeout
  client._setTimeout = (fn, ms) => {
    delays.push(ms)
    return originalSetTimeout(fn, ms)
  }

  ws.close()
  await Promise.resolve()
  assert.equal(clock.pendingCount(), 1)
  assert.equal(delays.length, 1)

  // 不应叠第二个 reconnect timer
  assert.equal(FakeWebSocket.instances.length, 1)
  clock.advance(delays[0])
  assert.equal(FakeWebSocket.instances.length, 2)
  assert.equal(clock.pendingCount(), 0)

  // 第二次失败 → 更大退避
  FakeWebSocket.latest().close()
  await Promise.resolve()
  assert.equal(clock.pendingCount(), 1)
  assert.equal(delays.length, 2)
  assert.ok(delays[1] > delays[0], `expected backoff ${delays[1]} > ${delays[0]}`)
  assert.ok(delays[1] <= 8_000)
})

test('旧 socket 事件不能污染新连接', async () => {
  const { client, clock, ws: oldWs } = await openClient({ reconnectBaseMs: 100, random: () => 0 })
  oldWs.close()
  await Promise.resolve()
  clock.advance(100)
  const newWs = FakeWebSocket.latest()
  newWs.open()
  await Promise.resolve()
  await Promise.resolve()

  // 完成自动恢复，避免 Handshake 占用 pending
  const hs = newWs.sent.find((e) => e.cmd === CMD_HANDSHAKE)
  assert.ok(hs)
  newWs.respond({ cmd: CMD_HANDSHAKE, client_seq: hs.client_seq, err: 0, payload: {} })
  await Promise.resolve()
  await Promise.resolve()
  const enter = newWs.sent.find((e) => e.cmd === CMD_ENTER_FARM)
  if (enter) {
    newWs.respond({
      cmd: CMD_ENTER_FARM,
      client_seq: enter.client_seq,
      err: 0,
      payload: { farm_seq: 1, relation: 'SELF', snapshot: { owner_uid: 42 } },
    })
    await Promise.resolve()
    await Promise.resolve()
  }

  const beforeSent = newWs.sent.length
  const req = client.request(CMD_PING, {})
  const ping = newWs.sent[beforeSent]
  assert.equal(ping.cmd, CMD_PING)
  const seq = ping.client_seq

  oldWs.respond({ cmd: CMD_PING, client_seq: seq, err: 0, payload: { stale: true } })
  await Promise.resolve()
  assert.equal(client._pending.size, 1)

  newWs.respond({ cmd: CMD_PING, client_seq: seq, err: 0, payload: { ok: true } })
  const env = await req
  assert.deepEqual(env.payload, { ok: true })
})

test('主动 close 取消重连；后续显式 connect 可重新启用', async () => {
  const { client, clock, ws } = await openClient({ reconnectBaseMs: 1_000, random: () => 0 })
  ws.close()
  await Promise.resolve()
  assert.equal(clock.pendingCount(), 1)

  client.close()
  assert.equal(clock.pendingCount(), 0)

  const before = FakeWebSocket.instances.length
  clock.advance(10_000)
  assert.equal(FakeWebSocket.instances.length, before)

  const reconnecting = client.connect()
  FakeWebSocket.latest().open()
  await reconnecting
  FakeWebSocket.latest().close()
  await Promise.resolve()
  assert.equal(clock.pendingCount(), 1)
})

test('初次 connect 失败不自动重试（一致语义）', async () => {
  const { client, clock } = createClient({ reconnectBaseMs: 500, random: () => 0 })
  const connecting = client.connect()
  FakeWebSocket.latest().close()
  await assert.rejects(() => connecting, /closed before open/)
  assert.equal(clock.pendingCount(), 0)
  clock.advance(5_000)
  assert.equal(FakeWebSocket.instances.length, 1)
})

test('重连打开后 Handshake 携带 resume_farm_uid/seq，再 EnterFarm 并回调恢复', async () => {
  const restored = []
  const { client, clock, ws } = await openClient({
    reconnectBaseMs: 100,
    random: () => 0,
    getResumeContext: () => ({ resume_farm_uid: 99, resume_farm_seq: 7 }),
  })
  client.onFarmRestored((enterEnv) => {
    restored.push(enterEnv)
  })

  ws.close()
  await Promise.resolve()
  clock.advance(100)
  const newWs = FakeWebSocket.latest()
  newWs.open()
  await Promise.resolve()
  await Promise.resolve()

  assert.ok(newWs.sent.length >= 1)
  const hs = newWs.sent[0]
  assert.equal(hs.cmd, CMD_HANDSHAKE)
  assert.equal(hs.payload.token, 'tok')
  assert.equal(hs.payload.client_config_ver, CLIENT_CONFIG_VER)
  assert.equal(hs.payload.resume_farm_uid, 99)
  assert.equal(hs.payload.resume_farm_seq, 7)

  newWs.respond({ cmd: CMD_HANDSHAKE, client_seq: hs.client_seq, err: 0, payload: {} })
  await Promise.resolve()
  await Promise.resolve()

  const enter = newWs.sent.find((e) => e.cmd === CMD_ENTER_FARM)
  assert.ok(enter)
  assert.equal(enter.payload.owner_uid, 99)

  const enterRsp = {
    cmd: CMD_ENTER_FARM,
    client_seq: enter.client_seq,
    err: 0,
    payload: {
      farm_seq: 8,
      relation: 'FRIEND',
      snapshot: { owner_uid: 99, plots: [] },
    },
  }
  newWs.respond(enterRsp)
  await Promise.resolve()
  await Promise.resolve()

  assert.equal(restored.length, 1)
  assert.equal(restored[0].payload.farm_seq, 8)
  assert.equal(restored[0].payload.snapshot.owner_uid, 99)
})

test('好友权限撤销时 EnterFarm 回退自己农场，不无限重连', async () => {
  const restored = []
  const { client, clock, ws } = await openClient({
    reconnectBaseMs: 100,
    random: () => 0,
    getResumeContext: () => ({ resume_farm_uid: 99, resume_farm_seq: 3 }),
  })
  client.onFarmRestored((enterEnv) => {
    restored.push(enterEnv)
  })

  ws.close()
  await Promise.resolve()
  clock.advance(100)
  const newWs = FakeWebSocket.latest()
  newWs.open()
  await Promise.resolve()
  await Promise.resolve()

  const hs = newWs.sent[0]
  newWs.respond({ cmd: CMD_HANDSHAKE, client_seq: hs.client_seq, err: 0, payload: {} })
  await Promise.resolve()
  await Promise.resolve()

  const friendEnter = newWs.sent.find((e) => e.cmd === CMD_ENTER_FARM)
  newWs.respond({
    cmd: CMD_ENTER_FARM,
    client_seq: friendEnter.client_seq,
    err: 1401,
    payload: {},
  })
  await Promise.resolve()
  await Promise.resolve()

  const selfEnter = newWs.sent.filter((e) => e.cmd === CMD_ENTER_FARM).at(-1)
  assert.ok(selfEnter)
  assert.equal(selfEnter.payload.owner_uid, 0)
  assert.notEqual(selfEnter.client_seq, friendEnter.client_seq)

  newWs.respond({
    cmd: CMD_ENTER_FARM,
    client_seq: selfEnter.client_seq,
    err: 0,
    payload: {
      farm_seq: 1,
      relation: 'SELF',
      snapshot: { owner_uid: 42, plots: [] },
    },
  })
  await Promise.resolve()
  await Promise.resolve()

  assert.equal(restored.length, 1)
  assert.equal(restored[0].payload.relation, 'SELF')
  assert.equal(clock.pendingCount(), 0)
})

test('重连失败继续退避，且任一时刻最多一个连接尝试', async () => {
  const { client, clock, ws } = await openClient({
    reconnectBaseMs: 200,
    reconnectMaxMs: 1_000,
    random: () => 0,
  })
  ws.close()
  await Promise.resolve()

  clock.advance(200)
  assert.equal(FakeWebSocket.instances.length, 2)
  // 连接仍在 CONNECTING，不应再开第三个
  assert.equal(clock.pendingCount(), 0)
  FakeWebSocket.latest().close()
  await Promise.resolve()
  assert.equal(clock.pendingCount(), 1)

  const before = FakeWebSocket.instances.length
  clock.advance(200)
  // 可能已到 max；至少不应并行两个新连接
  assert.ok(FakeWebSocket.instances.length <= before + 1)
  assert.ok(FakeWebSocket.instances.filter((s) => s.readyState === FakeWebSocket.CONNECTING).length <= 1)
})

test('_clientSeq 同一登录会话跨重连单调递增', async () => {
  const { client, clock, ws } = await openClient({ reconnectBaseMs: 50, random: () => 0 })
  const p1 = client.request(CMD_PING, {})
  const seq1 = ws.sent[0].client_seq
  ws.respond({ cmd: CMD_PING, client_seq: seq1, err: 0, payload: {} })
  await p1

  ws.close()
  await Promise.resolve()
  clock.advance(50)
  const newWs = FakeWebSocket.latest()
  newWs.open()
  await Promise.resolve()
  await Promise.resolve()

  // auto handshake uses next seq
  const hsSeq = newWs.sent[0].client_seq
  assert.ok(hsSeq > seq1)

  newWs.respond({ cmd: CMD_HANDSHAKE, client_seq: hsSeq, err: 0, payload: {} })
  await Promise.resolve()
  await Promise.resolve()
  const enterSeq = newWs.sent.find((e) => e.cmd === CMD_ENTER_FARM).client_seq
  assert.ok(enterSeq > hsSeq)
})

test('不匹配 cmd/seq 的应答不清理其他 pending', async () => {
  const { client, ws } = await openClient()
  const a = client.request(CMD_PING, {})
  const b = client.request(CMD_HANDSHAKE, { token: 'tok', client_config_ver: 1 })
  const seqA = ws.sent[0].client_seq
  const seqB = ws.sent[1].client_seq

  // 错误 cmd
  ws.respond({ cmd: CMD_ENTER_FARM, client_seq: seqA, err: 0, payload: {} })
  await Promise.resolve()
  assert.equal(client._pending.size, 2)

  // 未知 seq
  ws.respond({ cmd: CMD_PING, client_seq: 99999, err: 0, payload: {} })
  await Promise.resolve()
  assert.equal(client._pending.size, 2)

  ws.respond({ cmd: CMD_PING, client_seq: seqA, err: 0, payload: { a: 1 } })
  assert.deepEqual((await a).payload, { a: 1 })
  assert.equal(client._pending.size, 1)

  ws.respond({ cmd: CMD_HANDSHAKE, client_seq: seqB, err: 0, payload: {} })
  await b
  assert.equal(client._pending.size, 0)
})

test('handshake() 显式携带 resume 上下文字段', async () => {
  const { client, ws } = await openClient({
    getResumeContext: () => ({ resume_farm_uid: 5, resume_farm_seq: 11 }),
  })
  const pending = client.handshake()
  assert.equal(ws.sent[0].cmd, CMD_HANDSHAKE)
  assert.deepEqual(ws.sent[0].payload, {
    token: 'tok',
    client_config_ver: CLIENT_CONFIG_VER,
    resume_farm_uid: 5,
    resume_farm_seq: 11,
  })
  ws.respond({ cmd: CMD_HANDSHAKE, client_seq: ws.sent[0].client_seq, err: 0, payload: {} })
  await pending
})

test('致命 Handshake 失败进入终止态：关 socket、清 pending/timer、不再建连、failure 只一次', async () => {
  const failures = []
  const { client, clock, ws } = await openClient({
    reconnectBaseMs: 100,
    random: () => 0,
    getResumeContext: () => ({ resume_farm_uid: 99, resume_farm_seq: 3 }),
  })
  client.onFarmRestoreFailed((reason) => {
    failures.push(reason)
  })

  // 挂一个会在终止时被清掉的在途请求
  const stuck = client.request(CMD_PING, {})
  let stuckRejected = false
  stuck.catch(() => {
    stuckRejected = true
  })

  ws.close()
  await Promise.resolve()
  // close 已清 pending；再走重连恢复
  assert.equal(stuckRejected, true)

  clock.advance(100)
  const newWs = FakeWebSocket.latest()
  const socketsBeforeFatal = FakeWebSocket.instances.length
  newWs.open()
  await Promise.resolve()
  await Promise.resolve()

  const hs = newWs.sent.find((e) => e.cmd === CMD_HANDSHAKE)
  assert.ok(hs)
  // 再挂一个在途请求，致命终止应清掉
  const duringRestore = client.request(CMD_PING, {})
  let duringRejected = false
  duringRestore.catch(() => {
    duringRejected = true
  })
  assert.equal(client._pending.size >= 1, true)

  newWs.respond({ cmd: CMD_HANDSHAKE, client_seq: hs.client_seq, err: 1102, payload: {} })
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()

  assert.equal(newWs.readyState, FakeWebSocket.CLOSED)
  assert.equal(client._ws, null)
  assert.equal(client._pending.size, 0)
  assert.equal(clock.pendingCount(), 0)
  assert.equal(duringRejected, true)
  assert.equal(client._autoReconnect, false)
  assert.equal(failures.length, 1)
  assert.equal(failures[0].err, 1102)

  const after = FakeWebSocket.instances.length
  clock.advance(10_000)
  assert.equal(FakeWebSocket.instances.length, after)
  assert.equal(FakeWebSocket.instances.length, socketsBeforeFatal)
  assert.equal(failures.length, 1)
})
