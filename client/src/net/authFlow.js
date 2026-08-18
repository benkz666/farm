export class ProtocolError extends Error {
  constructor(code) {
    super(`net: protocol err=${code}`)
    this.name = 'ProtocolError'
    this.code = code
  }
}

function requireOK(envelope) {
  if (!envelope || envelope.err !== 0) {
    throw new ProtocolError(envelope?.err ?? 1001)
  }
  return envelope
}

/**
 * 完成鉴权、握手、可选邀请与自己的农场快照，并把服务端快照交给游戏层。
 */
export async function authenticateAndEnter({
  client,
  mode,
  username,
  password,
  inviteToken = '',
  getFarmBridge,
}) {
  // 游戏桥接与鉴权网络链路互不依赖，提前并行准备，避免登录成功后再等待大模块。
  const farmResultPromise = Promise.resolve()
    .then(getFarmBridge)
    .then(
      (farm) => ({ farm, error: null }),
      (error) => ({ farm: null, error }),
    )

  await client[mode](username, password)
  await client.connect()

  // NetClient batches requests queued in the same microtask into one WS frame.
  // The Gateway processes that frame in order, so handshake -> invite -> enter
  // keeps the protocol semantics while removing one or two network round trips.
  const requests = [client.handshake()]
  if (inviteToken) requests.push(client.acceptInvite(inviteToken))
  requests.push(client.enterFarm(0))
  const responses = await Promise.all(requests)
  const handshakeEnvelope = responses[0]
  requireOK(handshakeEnvelope)
  if (inviteToken) {
    const inviteEnvelope = responses[1]
    if (inviteEnvelope?.err !== 1402) requireOK(inviteEnvelope)
  }
  const enterEnvelope = requireOK(responses.at(-1))
  const farmResult = await farmResultPromise
  if (farmResult.error) throw farmResult.error
  const farm = farmResult.farm
  if (!farm?.enterOnlineFromNet) {
    throw new Error('game: farm bridge is not ready')
  }
  farm.enterOnlineFromNet(client, enterEnvelope)
  return enterEnvelope
}

export async function acceptInviteForSession(client, token) {
  const response = await client.acceptInvite(token)
  if (response?.err === 1402) return response
  return requireOK(response)
}
