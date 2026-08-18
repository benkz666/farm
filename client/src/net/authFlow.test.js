import test from 'node:test'
import assert from 'node:assert/strict'

import {
  ProtocolError,
  acceptInviteForSession,
  authenticateAndEnter,
} from './authFlow.js'

function createClient(events) {
  return {
    uid: 42,
    token: 'session-token',
    async login(username, password) {
      events.push(['login', username, password])
    },
    async register(username, password) {
      events.push(['register', username, password])
    },
    async connect() {
      events.push(['connect'])
    },
    async handshake() {
      events.push(['handshake'])
      return { err: 0, payload: {} }
    },
    async acceptInvite(token) {
      events.push(['acceptInvite', token])
      return { err: 0, payload: {} }
    },
    async enterFarm(ownerUid) {
      events.push(['enterFarm', ownerUid])
      return { err: 0, payload: { coin: 800 } }
    },
  }
}

test('登录后按协议顺序接受邀请并进入自己的农场', async () => {
  const events = []
  const client = createClient(events)
  const farm = {
    enterOnlineFromNet(receivedClient, envelope) {
      events.push(['enterOnlineFromNet', receivedClient, envelope])
    },
  }

  await authenticateAndEnter({
    client,
    mode: 'login',
    username: 'alice',
    password: 'secret12',
    inviteToken: 'invite-token',
    getFarmBridge: async () => farm,
  })

  assert.deepEqual(events.slice(0, 5), [
    ['login', 'alice', 'secret12'],
    ['connect'],
    ['handshake'],
    ['acceptInvite', 'invite-token'],
    ['enterFarm', 0],
  ])
  assert.equal(events[5][0], 'enterOnlineFromNet')
  assert.equal(events[5][1], client)
  assert.deepEqual(events[5][2], { err: 0, payload: { coin: 800 } })
})

test('协议错误保留错误码且不进入农场', async () => {
  const events = []
  const client = createClient(events)
  client.handshake = async () => ({ err: 1102, payload: {} })

  await assert.rejects(
    authenticateAndEnter({
      client,
      mode: 'register',
      username: 'alice',
      password: 'secret12',
      getFarmBridge: async () => {
        throw new Error('不应加载农场')
      },
    }),
    (error) => error instanceof ProtocolError && error.code === 1102,
  )
})

test('同账号已有在线连接时保留 1105 且不进入农场', async () => {
  const events = []
  const client = createClient(events)
  client.handshake = async () => {
    events.push(['handshake'])
    return { err: 1105, payload: {} }
  }

  await assert.rejects(
    authenticateAndEnter({
      client,
      mode: 'login',
      username: 'alice',
      password: 'secret12',
      getFarmBridge: async () => {
        throw new Error('不应加载农场')
      },
    }),
    (error) => error instanceof ProtocolError && error.code === 1105,
  )
  assert.deepEqual(events, [
    ['login', 'alice', 'secret12'],
    ['connect'],
    ['handshake'],
    ['enterFarm', 0],
  ])
})

test('已登录邀请落地复用 404 并返回响应', async () => {
  const events = []
  const client = createClient(events)

  const response = await acceptInviteForSession(client, 'invite-token')

  assert.deepEqual(response, { err: 0, payload: {} })
  assert.deepEqual(events, [['acceptInvite', 'invite-token']])
})

test('已是好友的邀请不阻断登录进入农场', async () => {
  const events = []
  const client = createClient(events)
  client.acceptInvite = async (token) => {
    events.push(['acceptInvite', token])
    return { err: 1402, payload: {} }
  }
  const farm = {
    enterOnlineFromNet() {
      events.push(['enterOnlineFromNet'])
    },
  }

  await authenticateAndEnter({
    client,
    mode: 'login',
    username: 'alice',
    password: 'secret12',
    inviteToken: 'invite-token',
    getFarmBridge: async () => farm,
  })

  assert.deepEqual(events.slice(0, 6), [
    ['login', 'alice', 'secret12'],
    ['connect'],
    ['handshake'],
    ['acceptInvite', 'invite-token'],
    ['enterFarm', 0],
    ['enterOnlineFromNet'],
  ])
})

test('游戏桥接准备与登录请求并行启动', async () => {
  const events = []
  let resolveLogin
  const client = createClient(events)
  client.login = async () => {
    events.push(['login-start'])
    await new Promise((resolve) => { resolveLogin = resolve })
    events.push(['login-done'])
  }
  const farm = {
    enterOnlineFromNet() {
      events.push(['enterOnlineFromNet'])
    },
  }

  const pending = authenticateAndEnter({
    client,
    mode: 'login',
    username: 'alice',
    password: 'secret12',
    getFarmBridge: async () => {
      events.push(['farm-bridge-start'])
      return farm
    },
  })
  await Promise.resolve()

  assert.deepEqual(events, [['login-start'], ['farm-bridge-start']])
  resolveLogin()
  await pending
})

test('握手、邀请和进入农场在等待响应前一起发出', async () => {
  const events = []
  let resolveHandshake
  const client = createClient(events)
  client.handshake = async () => {
    events.push(['handshake-start'])
    await new Promise((resolve) => { resolveHandshake = resolve })
    events.push(['handshake-done'])
    return { err: 0, payload: {} }
  }
  const farm = { enterOnlineFromNet() {} }

  const pending = authenticateAndEnter({
    client,
    mode: 'login',
    username: 'alice',
    password: 'secret12',
    inviteToken: 'invite-token',
    getFarmBridge: async () => farm,
  })
  await Promise.resolve()
  await Promise.resolve()

  assert.deepEqual(events, [
    ['login', 'alice', 'secret12'],
    ['connect'],
    ['handshake-start'],
    ['acceptInvite', 'invite-token'],
    ['enterFarm', 0],
  ])
  resolveHandshake()
  await pending
})
