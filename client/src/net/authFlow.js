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
  await client[mode](username, password)
  await client.connect()
  requireOK(await client.handshake())

  if (inviteToken) {
    await acceptInviteForSession(client, inviteToken)
  }

  const enterEnvelope = requireOK(await client.enterFarm(0))
  const farm = await getFarmBridge()
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
