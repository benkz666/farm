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
