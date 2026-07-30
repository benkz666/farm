import test from 'node:test'
import assert from 'node:assert/strict'

import { CMD_TASK_NOTIFY, NetClient } from './client.js'

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
  assert.deepEqual(await client.mailClaim(8), { cmd: 608, payload: { mail_id: 8 } })
  assert.deepEqual(await client.claimDailyLogin(), { cmd: 614, payload: {} })
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
