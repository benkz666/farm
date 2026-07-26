import test from 'node:test'
import assert from 'node:assert/strict'

import { NetClient } from './client.js'

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

test('HTTP 鉴权失败保留协议错误码', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = async () => ({
    ok: false,
    status: 400,
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
