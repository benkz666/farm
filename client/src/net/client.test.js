import test from 'node:test'
import assert from 'node:assert/strict'

import { CMD_SESSION_KICK, CMD_TASK_NOTIFY, CMD_PUSH_BATCH, MAX_PUSH_BATCH_ENVELOPES, NetClient } from './client.js'

test('AcceptInvite 使用 404 命令发送邀请凭证', async () => {
  const client = new NetClient()
  client.request = async (cmd, payload) => ({ cmd, payload })

  const result = await client.acceptInvite('invite-token')

  assert.deepEqual(result, {
    cmd: 404,
    payload: { token: 'invite-token' },
  })
})

test('好友与房间 API 使用约定命令和载荷', async () => {
  const client = new NetClient()
  client.request = async (cmd, payload) => ({ cmd, payload })

  assert.deepEqual(await client.friendList(), { cmd: 400, payload: {} })
  assert.deepEqual(await client.genShareLink(), { cmd: 402, payload: {} })
  assert.deepEqual(await client.removeFriend(7), { cmd: 406, payload: { peer_uid: 7 } })
  assert.deepEqual(await client.addFriendByUID(8), { cmd: 408, payload: { peer_uid: 8 } })
  assert.deepEqual(await client.leaveFarm(), { cmd: 202, payload: {} })
  assert.deepEqual(await client.syncFarm(9, 10), {
    cmd: 204,
    payload: { owner_uid: 9, from_seq: 10 },
  })
})

test('SearchUser 使用 410 命令按用户名精确查询', async () => {
  const client = new NetClient()
  client.request = async (cmd, payload) => ({ cmd, payload })

  assert.equal(typeof client.searchUser, 'function')
  assert.deepEqual(await client.searchUser('alice'), {
    cmd: 410,
    payload: { username: 'alice' },
  })
  assert.deepEqual(await client.requestFriend(9), { cmd: 412, payload: { peer_uid: 9 } })
  assert.deepEqual(await client.listFriendRequests(), { cmd: 414, payload: {} })
  assert.deepEqual(await client.acceptFriendRequest(9), { cmd: 416, payload: { from_uid: 9 } })
  assert.deepEqual(await client.rejectFriendRequest(9), { cmd: 418, payload: { from_uid: 9 } })
})

test('Steal / Pet / Task / Mail / DailyLogin 使用约定命令和载荷', async () => {
  const client = new NetClient()
  client.request = async (cmd, payload) => ({ cmd, payload })

  assert.deepEqual(await client.steal(9, 3, 1), {
    cmd: 222,
    payload: { owner_uid: 9, plot_index: 3, crop_id: 1 },
  })
  assert.deepEqual(await client.petStatus(), { cmd: 500, payload: {} })
  assert.deepEqual(await client.petActivate(1), { cmd: 502, payload: { dog_type: 1 } })
  assert.deepEqual(await client.petFeed(50), { cmd: 504, payload: { grams: 50 } })
  assert.deepEqual(await client.taskList(), { cmd: 600, payload: {} })
  assert.deepEqual(await client.taskClaim(2), { cmd: 602, payload: { task_id: 2 } })
  assert.deepEqual(await client.mailList(), { cmd: 604, payload: {} })
  assert.deepEqual(await client.mailReadAll(), { cmd: 606, payload: { all: true } })
  assert.deepEqual(await client.mailClaim(8), { cmd: 608, payload: { mail_id: 8 } })
  assert.deepEqual(await client.mailDeleteAll(), { cmd: 610, payload: { all: true } })
  assert.deepEqual(await client.codexList(), { cmd: 612, payload: {} })
  assert.deepEqual(await client.claimDailyLogin(), { cmd: 614, payload: {} })
  assert.deepEqual(await client.setTimeProfile('fast'), {
    cmd: 616,
    payload: { time_profile: 'fast' },
  })
})

test('邮件 ID 与 farm_seq 超过 2^53 后按十进制字符串原样回传', async () => {
  const client = new NetClient()
  client.request = async (cmd, payload) => ({ cmd, payload })

  assert.deepEqual(await client.mailClaim('9007199254740993'), {
    cmd: 608,
    payload: { mail_id: '9007199254740993' },
  })
  assert.deepEqual(await client.syncFarm('1785402171458126005', '9007199254740993'), {
    cmd: 204,
    payload: {
      owner_uid: '1785402171458126005',
      from_seq: '9007199254740993',
    },
  })
})

test('FarmDelta 主动推送交给 delta 订阅者', () => {
  const client = new NetClient()
  let received
  client.onDelta((envelope) => {
    received = envelope
  })

  client._onMessage({
    data: JSON.stringify({
      cmd: 9000,
      client_seq: 0,
      err: 0,
      payload: { owner_uid: 9, farm_seq: 3, plots: [] },
    }),
  })

  assert.deepEqual(received.payload, { owner_uid: 9, farm_seq: 3, plots: [] })
})

test('PlayerDelta 主动推送交给个人状态订阅者', () => {
  const client = new NetClient()
  let received
  client.onPlayerDelta((envelope) => {
    received = envelope
  })

  client._onMessage({
    data: JSON.stringify({
      cmd: 9002,
      client_seq: 0,
      err: 0,
      payload: { coin: 1170, exp: 2, level: 0, bag: {}, warehouse: { 'fruit:1': 3 } },
    }),
  })

  assert.deepEqual(received.payload, {
    coin: 1170,
    exp: 2,
    level: 0,
    bag: {},
    warehouse: { 'fruit:1': 3 },
  })
})

test('CMD_TASK_NOTIFY 为协议 9008', () => {
  assert.equal(CMD_TASK_NOTIFY, 9008)
})

test('SessionKick 推送终止重连并通知恢复失败', () => {
  let closed = 0
  const client = new NetClient({
    WebSocket: { CONNECTING: 0, OPEN: 1 },
  })
  client._autoReconnect = true
  client._hadOpenConnection = true
  client._ws = {
    readyState: 1,
    close() {
      closed++
    },
  }
  let failed
  client.onFarmRestoreFailed((reason) => {
    failed = reason
  })

  client._onMessage({
    data: JSON.stringify({
      cmd: CMD_SESSION_KICK,
      client_seq: 0,
      err: 0,
      payload: { reason: 1105 },
    }),
  })

  assert.equal(client._fatalStopped, true)
  assert.equal(client._autoReconnect, false)
  assert.equal(closed, 1)
  assert.equal(failed.err, 1105)
})

test('TaskNotify 主动推送交给 onTaskNotify 订阅者，取消后不再投递', () => {
  const client = new NetClient()
  const hits = []
  const stop = client.onTaskNotify((envelope) => {
    hits.push(envelope.payload)
  })

  const push = {
    cmd: 9008,
    client_seq: 0,
    err: 0,
    payload: {
      id: 1,
      title: '完成一次播种',
      progress: 1,
      target: 1,
      reward_coin: 20,
      claimed: false,
    },
  }
  client._onMessage({ data: JSON.stringify(push) })
  assert.equal(hits.length, 1)
  assert.deepEqual(hits[0], push.payload)

  stop()
  client._onMessage({ data: JSON.stringify(push) })
  assert.equal(hits.length, 1)
})

test('重复 onTaskNotify 后取消旧订阅不会留下重复 listener', () => {
  const client = new NetClient()
  const hits = []
  const stop1 = client.onTaskNotify(() => hits.push('a'))
  const stop2 = client.onTaskNotify(() => hits.push('b'))
  stop1()

  client._onMessage({
    data: JSON.stringify({
      cmd: 9008,
      client_seq: 0,
      err: 0,
      payload: { id: 2, progress: 1, target: 1, reward_coin: 30, claimed: false },
    }),
  })

  assert.deepEqual(hits, ['b'])
  stop2()
})

test('HTTP 鉴权失败保留协议错误码', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = async () => ({
    ok: false,
    status: 400,
    text: async () => JSON.stringify({ err: 1104 }),
    json: async () => ({ err: 1104 }),
  })

  try {
    const client = new NetClient()
    await assert.rejects(
      client.login('alice', 'wrong-password'),
      (error) => error instanceof Error && error.code === 1104,
    )
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('CMD_PUSH_BATCH 为协议 9010', () => {
  assert.equal(CMD_PUSH_BATCH, 9010)
  assert.equal(MAX_PUSH_BATCH_ENVELOPES, 64)
})

test('PushBatch 按数组顺序依次触发 delta/player/task/mail', () => {
  const client = new NetClient()
  const order = []
  client.onDelta((env) => order.push(['delta', env.payload.farm_seq]))
  client.onPlayerDelta((env) => order.push(['player', env.payload.coin]))
  client.onTaskNotify((env) => order.push(['task', env.payload.id]))
  client.onMailNotify((env) => order.push(['mail', env.payload.kind]))

  client._onMessage({
    data: JSON.stringify({
      cmd: CMD_PUSH_BATCH,
      client_seq: 0,
      err: 0,
      payload: {
        envelopes: [
          {
            cmd: 9000,
            client_seq: 0,
            err: 0,
            payload: { owner_uid: '9007199254740993', farm_seq: '9007199254740994', plots: [] },
          },
          { cmd: 9002, client_seq: 0, err: 0, payload: { coin: 9 } },
          { cmd: 9008, client_seq: 0, err: 0, payload: { id: 1, progress: 1, target: 1 } },
          { cmd: 9004, client_seq: 0, err: 0, payload: { kind: 'new_mail' } },
        ],
      },
    }),
  })

  assert.deepEqual(order, [
    ['delta', '9007199254740994'],
    ['player', 9],
    ['task', 1],
    ['mail', 'new_mail'],
  ])
})

test('PushBatch 不破坏请求 pending 匹配', async () => {
  const FakeWS = {
    CONNECTING: 0,
    OPEN: 1,
  }
  const client = new NetClient({ WebSocket: FakeWS, requestTimeoutMs: 5_000 })
  let sent
  client._ws = {
    readyState: FakeWS.OPEN,
    send(text) {
      sent = JSON.parse(text)
    },
  }
  const pending = client.request(102, { client_time: 1 })
  assert.equal(sent.cmd, 102)
  const seq = sent.client_seq

  client._onMessage({
    data: JSON.stringify({
      cmd: CMD_PUSH_BATCH,
      client_seq: 0,
      err: 0,
      payload: {
        envelopes: [{ cmd: 9002, client_seq: 0, err: 0, payload: { coin: 1 } }],
      },
    }),
  })

  client._onMessage({
    data: JSON.stringify({
      cmd: 102,
      client_seq: seq,
      err: 0,
      payload: { client_time: 1, server_time: 2 },
    }),
  })
  const env = await pending
  assert.equal(env.payload.server_time, 2)
})

test('PushBatch 拒绝嵌套 batch', () => {
  const client = new NetClient({ WebSocket: { CONNECTING: 0, OPEN: 1 } })
  let closed = 0
  client._ws = {
    readyState: 1,
    close() {
      closed++
    },
  }
  let rejected
  client._pending.set(1, {
    cmd: 102,
    timer: null,
    settled: false,
    resolve() {},
    reject(err) {
      rejected = err
    },
  })

  client._onMessage({
    data: JSON.stringify({
      cmd: CMD_PUSH_BATCH,
      client_seq: 0,
      err: 0,
      payload: {
        envelopes: [
          {
            cmd: CMD_PUSH_BATCH,
            client_seq: 0,
            err: 0,
            payload: { envelopes: [] },
          },
        ],
      },
    }),
  })

  assert.match(String(rejected), /nested push batch/)
  assert.equal(closed, 1)
  assert.equal(client._ws, null)
})

test('PushBatch 拒绝超限内层数量', () => {
  const client = new NetClient({ WebSocket: { CONNECTING: 0, OPEN: 1 } })
  let closed = 0
  client._ws = {
    readyState: 1,
    close() {
      closed++
    },
  }
  const envelopes = Array.from({ length: MAX_PUSH_BATCH_ENVELOPES + 1 }, () => ({
    cmd: 9004,
    client_seq: 0,
    err: 0,
    payload: { kind: 'x' },
  }))
  let rejected
  client._pending.set(1, {
    cmd: 102,
    timer: null,
    settled: false,
    resolve() {},
    reject(err) {
      rejected = err
    },
  })

  client._onMessage({
    data: JSON.stringify({
      cmd: CMD_PUSH_BATCH,
      client_seq: 0,
      err: 0,
      payload: { envelopes },
    }),
  })

  assert.match(String(rejected), /invalid push batch/)
  assert.equal(closed, 1)
})
