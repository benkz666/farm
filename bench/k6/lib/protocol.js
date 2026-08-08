/**
 * Farm k6 协议工具。
 *
 * k6/ws 是回调式 API，不适合在 VU 中直接用 async/await 做
 * request/response。本文件用 `${cmd}:${client_seq}` 待处理映射、
 * 超时回调和最小发送间隔封装一层可组合的会话 API。
 */
import http from 'k6/http'
import { check } from 'k6'
import ws from 'k6/ws'

export const DEFAULT_BASE_URL = 'http://127.0.0.1:9002'
export const WS_SUBPROTOCOL = 'farm.v3.pb'
export const CLIENT_CONFIG_VER = 1
export const MAX_UINT32 = 0xffffffff
export const DEFAULT_REQUEST_TIMEOUT_MS = 10_000
export const MAX_COMMANDS_PER_SECOND = 8
export const DEFAULT_COMMAND_INTERVAL_MS = 1000 / MAX_COMMANDS_PER_SECOND

function appendVarint(target, value) {
  do {
    let byte = value % 128
    value = Math.floor(value / 128)
    if (value > 0) byte |= 0x80
    target.push(byte)
  } while (value > 0)
}

function appendFieldVarint(target, field, value) {
  appendVarint(target, field * 8)
  appendVarint(target, value)
}

function appendFieldBytes(target, field, value) {
  appendVarint(target, field * 8 + 2)
  appendVarint(target, value.length)
  target.push(...value)
}

function readVarint(bytes, cursor) {
  let value = 0
  let scale = 1
  for (let i = 0; i < 10; i++) {
    if (cursor.offset >= bytes.length) throw new Error('truncated varint')
    const byte = bytes[cursor.offset++]
    value += (byte & 0x7f) * scale
    if ((byte & 0x80) === 0) return value
    scale *= 128
  }
  throw new Error('invalid varint')
}

function readVarintBig(bytes, cursor) {
  let value = 0n
  let shift = 0n
  for (let i = 0; i < 10; i++) {
    if (cursor.offset >= bytes.length) throw new Error('truncated varint')
    const byte = bytes[cursor.offset++]
    value |= BigInt(byte & 0x7f) << shift
    if ((byte & 0x80) === 0) return value
    shift += 7n
  }
  throw new Error('invalid varint')
}

function readBytes(bytes, cursor) {
  const length = readVarint(bytes, cursor)
  const end = cursor.offset + length
  if (end > bytes.length) throw new Error('invalid protobuf length')
  const value = bytes.subarray(cursor.offset, end)
  cursor.offset = end
  return value
}

function skipField(bytes, cursor, wireType) {
  if (wireType === 0) readVarintBig(bytes, cursor)
  else if (wireType === 2) readBytes(bytes, cursor)
  else throw new Error(`unsupported protobuf wire type ${wireType}`)
}

function utf8Encode(value) {
  const result = []
  for (let i = 0; i < value.length; i++) {
    let point = value.codePointAt(i)
    if (point > 0xffff) i++
    if (point <= 0x7f) result.push(point)
    else if (point <= 0x7ff) result.push(0xc0 | point >> 6, 0x80 | point & 0x3f)
    else if (point <= 0xffff) result.push(0xe0 | point >> 12, 0x80 | point >> 6 & 0x3f, 0x80 | point & 0x3f)
    else result.push(0xf0 | point >> 18, 0x80 | point >> 12 & 0x3f, 0x80 | point >> 6 & 0x3f, 0x80 | point & 0x3f)
  }
  return result
}

function utf8Decode(bytes) {
  let result = ''
  for (let i = 0; i < bytes.length;) {
    const first = bytes[i++]
    let point
    if (first < 0x80) point = first
    else if ((first & 0xe0) === 0xc0) point = (first & 0x1f) << 6 | bytes[i++] & 0x3f
    else if ((first & 0xf0) === 0xe0) point = (first & 0x0f) << 12 | (bytes[i++] & 0x3f) << 6 | bytes[i++] & 0x3f
    else point = (first & 0x07) << 18 | (bytes[i++] & 0x3f) << 12 | (bytes[i++] & 0x3f) << 6 | bytes[i++] & 0x3f
    result += String.fromCodePoint(point)
  }
  return result
}

export function encodeBinaryBatch(envelopes) {
  if (!Array.isArray(envelopes) || envelopes.length < 1 || envelopes.length > 64) {
    throw new Error('farm protocol: invalid binary batch size')
  }
  const bytes = []
  for (const envelope of envelopes) {
    const payload = utf8Encode(JSON.stringify(envelope.payload || {}))
    const message = []
    appendFieldVarint(message, 1, envelope.cmd)
    appendFieldVarint(message, 2, envelope.client_seq)
    if (envelope.err) appendFieldVarint(message, 3, envelope.err)
    appendFieldBytes(message, 10, payload)
    appendFieldBytes(bytes, 1, message)
  }
  return new Uint8Array(bytes).buffer
}

export function decodeBinaryBatch(data) {
  const bytes = new Uint8Array(data)
  const cursor = { offset: 0 }
  const envelopes = []
  while (cursor.offset < bytes.length) {
    const tag = readVarint(bytes, cursor)
    if (tag !== 10) throw new Error('invalid WireBatch field')
    envelopes.push(decodeWireEnvelope(readBytes(bytes, cursor)))
  }
  if (envelopes.length < 1 || envelopes.length > 64) throw new Error('invalid protobuf batch count')
  return envelopes
}

function decodeWireEnvelope(bytes) {
  const cursor = { offset: 0 }
  let cmd = 0
  let client_seq = 0
  let err = 0
  let payload = null
  while (cursor.offset < bytes.length) {
    const tag = readVarint(bytes, cursor)
    const field = Math.floor(tag / 8)
    const wireType = tag % 8
    if (field === 1 && wireType === 0) cmd = readVarint(bytes, cursor)
    else if (field === 2 && wireType === 0) client_seq = readVarint(bytes, cursor)
    else if (field === 3 && wireType === 0) err = readVarint(bytes, cursor)
    else if (field === 10 && wireType === 2) payload = JSON.parse(utf8Decode(readBytes(bytes, cursor)))
    else if (field === 13 && wireType === 2) payload = decodeFarmReadPayload(readBytes(bytes, cursor), 2, 6)
    else if (field === 14 && wireType === 2) payload = decodeFarmReadPayload(readBytes(bytes, cursor), 3, 0)
    else if (field === 15 && wireType === 2) payload = decodeFarmDeltaPayload(readBytes(bytes, cursor))
    else skipField(bytes, cursor, wireType)
  }
  if (payload === null) throw new Error('missing protobuf payload')
  return { cmd, client_seq, err, payload }
}

function decodeFarmReadPayload(bytes, farmSeqField, relationField) {
  const cursor = { offset: 0 }
  const payload = {}
  while (cursor.offset < bytes.length) {
    const tag = readVarint(bytes, cursor)
    const field = Math.floor(tag / 8)
    const wireType = tag % 8
    if (field === farmSeqField && wireType === 0) payload.farm_seq = readVarintBig(bytes, cursor).toString()
    else if (field === relationField && relationField !== 0 && wireType === 2) payload.relation = utf8Decode(readBytes(bytes, cursor))
    else skipField(bytes, cursor, wireType)
  }
  return payload
}

function decodeFarmDeltaPayload(bytes) {
  const cursor = { offset: 0 }
  const payload = { plots: [] }
  while (cursor.offset < bytes.length) {
    const tag = readVarint(bytes, cursor)
    const field = Math.floor(tag / 8)
    const wireType = tag % 8
    if (field === 1 && wireType === 0) payload.owner_uid = readVarintBig(bytes, cursor).toString()
    else if (field === 2 && wireType === 0) payload.farm_seq = readVarintBig(bytes, cursor).toString()
    else if (field === 5 && wireType === 0) payload.actor_uid = readVarintBig(bytes, cursor).toString()
    else if (field === 6 && wireType === 0) payload.action = readVarint(bytes, cursor)
    else skipField(bytes, cursor, wireType)
  }
  return payload
}

export const CMD_HANDSHAKE = 100
export const CMD_PING = 102
export const CMD_ENTER_FARM = 200
export const CMD_LEAVE_FARM = 202
export const CMD_SYNC_FARM = 204
export const CMD_FARM_DELTA = 9000
export const CMD_PLAYER_DELTA = 9002
export const CMD_MAIL_NOTIFY = 9004
export const CMD_SESSION_KICK = 9006
export const CMD_TASK_NOTIFY = 9008

export const COMMANDS = Object.freeze({
  HANDSHAKE: CMD_HANDSHAKE,
  PING: CMD_PING,
  ENTER_FARM: CMD_ENTER_FARM,
  LEAVE_FARM: CMD_LEAVE_FARM,
  SYNC_FARM: CMD_SYNC_FARM,
  FARM_DELTA: CMD_FARM_DELTA,
  PLAYER_DELTA: CMD_PLAYER_DELTA,
  MAIL_NOTIFY: CMD_MAIL_NOTIFY,
  SESSION_KICK: CMD_SESSION_KICK,
  TASK_NOTIFY: CMD_TASK_NOTIFY,
})

/**
 * 去掉尾部斜杠，避免 BASE_URL + API 路径出现双斜杠。
 *
 * @param {string} baseUrl
 * @returns {string}
 */
export function normalizeBaseUrl(baseUrl = DEFAULT_BASE_URL) {
  const value = String(baseUrl || DEFAULT_BASE_URL).trim()
  return value.replace(/\/+$/, '')
}

/**
 * 根据 HTTP 地址推导 WS 地址。登录接口返回的 ws_url 优先级更高。
 *
 * @param {string} baseUrl
 * @returns {string}
 */
export function deriveWsUrl(baseUrl = DEFAULT_BASE_URL) {
  const value = normalizeBaseUrl(baseUrl)
  const match = /^(https?|wss?):\/\/([^/]+)/i.exec(value)
  if (!match) {
    return `${value.replace(/\/+$/, '')}/ws`
  }
  const scheme = match[1].toLowerCase() === 'https' || match[1].toLowerCase() === 'wss'
    ? 'wss'
    : 'ws'
  return `${scheme}://${match[2]}/ws`
}

/**
 * @param {{ws_url?: string}|null|undefined} auth
 * @param {string} baseUrl
 * @returns {string}
 */
export function resolveWsUrl(auth, baseUrl = DEFAULT_BASE_URL) {
  return auth && auth.ws_url ? String(auth.ws_url) : deriveWsUrl(baseUrl)
}

/**
 * POST /api/register。
 *
 * @param {string} baseUrl
 * @param {string} username
 * @param {string} password
 * @returns {{uid: string|number, token: string, ws_url: string}|null}
 */
export function register(baseUrl, username, password) {
  return authenticate(baseUrl, '/api/register', username, password, 'register')
}

/**
 * POST /api/login。
 *
 * @param {string} baseUrl
 * @param {string} username
 * @param {string} password
 * @returns {{uid: string|number, token: string, ws_url: string}|null}
 */
export function login(baseUrl, username, password) {
  return authenticate(baseUrl, '/api/login', username, password, 'login')
}

function authenticate(baseUrl, path, username, password, operation) {
  const response = http.post(
    `${normalizeBaseUrl(baseUrl)}${path}`,
    JSON.stringify({ username, password }),
    {
      headers: { 'Content-Type': 'application/json' },
      tags: { operation },
    },
  )

  const statusOK = check(response, {
    [`${operation} HTTP status is 200`]: (res) => res.status === 200,
  })
  if (!statusOK) return null

  let body
  try {
    body = JSON.parse(response.body || '{}')
  } catch {
    body = null
  }

  const credentialsOK = check(response, {
    [`${operation} response has credentials`]: () =>
      body !== null &&
      body !== undefined &&
      body.uid !== undefined &&
      body.uid !== null &&
      typeof body.token === 'string' &&
      body.token.length > 0 &&
      typeof body.ws_url === 'string' &&
      body.ws_url.length > 0,
  })
  if (!credentialsOK) return null

  // uid/farm_seq 保留服务端的字符串形式，避免 JS Number 丢失精度。
  return {
    uid: body.uid,
    token: body.token,
    ws_url: body.ws_url,
  }
}

/**
 * 构造一个合法的客户端 Envelope。payload 必须是对象，不能是数组或 null。
 *
 * @param {number} cmd
 * @param {number} clientSeq
 * @param {object} [payload]
 * @returns {{cmd: number, client_seq: number, err: number, payload: object}}
 */
export function makeEnvelope(cmd, clientSeq, payload = {}) {
  if (!Number.isSafeInteger(cmd) || cmd < 0 || cmd > MAX_UINT32) {
    throw new Error(`farm protocol: invalid cmd=${cmd}`)
  }
  if (
    !Number.isSafeInteger(clientSeq) ||
    clientSeq < 0 ||
    clientSeq > MAX_UINT32
  ) {
    throw new Error(`farm protocol: invalid client_seq=${clientSeq}`)
  }
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) {
    throw new Error('farm protocol: payload must be an object')
  }
  return {
    cmd,
    client_seq: clientSeq,
    err: 0,
    payload,
  }
}

/**
 * 解析压测时长，如 30s、5m、1h、500ms。
 *
 * @param {string|number} value
 * @returns {number}
 */
export function parseDurationMs(value) {
  if (typeof value === 'number') {
    if (Number.isFinite(value) && value >= 0) return value
    throw new Error(`farm protocol: invalid duration=${value}`)
  }
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/i.exec(String(value || '').trim())
  if (!match) {
    throw new Error(`farm protocol: invalid duration=${value}`)
  }
  const amount = Number(match[1])
  const unit = match[2].toLowerCase()
  const multiplier = unit === 'h'
    ? 60 * 60 * 1000
    : unit === 'm'
      ? 60 * 1000
      : unit === 's'
        ? 1000
        : 1
  return amount * multiplier
}

/**
 * 一条 Farm WebSocket 会话。所有回调均为 (response, error) 形式。
 */
export class FarmSession {
  constructor(socket, token, options = {}) {
    this.socket = socket
    this.token = token
    this.options = options
    this.requestTimeoutMs = options.requestTimeoutMs || DEFAULT_REQUEST_TIMEOUT_MS
    this.commandIntervalMs = Math.max(
      DEFAULT_COMMAND_INTERVAL_MS,
      Number(options.commandIntervalMs || DEFAULT_COMMAND_INTERVAL_MS),
    )
    this.clientSeq = 0
    this.pending = new Map()
    this.pushHandlers = []
    this.pushCount = 0
    this.opened = false
    this.closed = false
    this.expectedClose = false
    this.closeReason = ''
    this.resumeFarmUid = options.resumeFarmUid === undefined
      ? '0'
      : options.resumeFarmUid
    this.resumeFarmSeq = options.resumeFarmSeq === undefined
      ? '0'
      : options.resumeFarmSeq
    this.nextSendAt = 0
  }

  /**
   * 发送请求，并在收到同 cmd + client_seq 响应时回调。
   *
   * @param {number} cmd
   * @param {object} payload
   * @param {(response: object|null, error: Error|null) => void} callback
   * @param {number} [timeoutMs]
   * @returns {{cmd: number, client_seq: number, key: string}|null}
   */
  request(cmd, payload = {}, callback = () => {}, timeoutMs = this.requestTimeoutMs) {
    if (this.closed) {
      callback(null, new Error('farm protocol: websocket is closed'))
      return null
    }

    const clientSeq = this.nextClientSeq()
    const key = `${cmd}:${clientSeq}`
    const entry = {
      cmd,
      clientSeq,
      key,
      payload,
      callback,
      timeoutMs,
      sent: false,
      settled: false,
    }
    this.pending.set(key, entry)

    const now = Date.now()
    const sendAt = Math.max(now, this.nextSendAt)
    this.nextSendAt = sendAt + this.commandIntervalMs
    // k6/ws 的 setTimeout 不允许 delay=0，到期则直接发送。
    const delayMs = Math.max(0, sendAt - now)
    if (delayMs <= 0) {
      this.sendPending(entry)
    } else {
      this.socket.setTimeout(() => this.sendPending(entry), delayMs)
    }
    return { cmd, client_seq: clientSeq, key }
  }

  nextClientSeq() {
    this.clientSeq = this.clientSeq >= MAX_UINT32 ? 1 : this.clientSeq + 1
    return this.clientSeq
  }

  sendPending(entry) {
    if (entry.settled || this.closed) {
      if (!entry.settled) {
        this.settle(entry, null, new Error('farm protocol: websocket closed before send'))
      }
      return
    }
    entry.sent = true
    entry.timeoutTimer = this.socket.setTimeout(
      () => this.timeoutPending(entry),
      entry.timeoutMs,
    )
    try {
      this.socket.sendBinary(encodeBinaryBatch([
        makeEnvelope(entry.cmd, entry.clientSeq, entry.payload),
      ]))
    } catch (error) {
      this.settle(entry, null, error instanceof Error ? error : new Error(String(error)))
    }
  }

  timeoutPending(entry) {
    if (!entry.settled && this.pending.has(entry.key)) {
      this.settle(
        entry,
        null,
        new Error(`farm protocol: request timeout ${entry.key}`),
      )
    }
  }

  settle(entry, response, error) {
    if (entry.settled) return
    entry.settled = true
    this.pending.delete(entry.key)
    entry.callback(response, error)
  }

  /**
   * 处理一个服务端二进制批帧。client_seq=0 的 push 不会匹配 pending。
   *
   * @param {string} data
   */
  onMessage(data) {
    let envelopes
    try {
      envelopes = decodeBinaryBatch(data)
    } catch {
      this.protocolError('invalid binary frame')
      return
    }
    for (const envelope of envelopes) {
      if (!Number.isSafeInteger(envelope.cmd) || !Number.isSafeInteger(envelope.client_seq)) {
        this.protocolError('invalid envelope metadata')
        return
      }
      if (!envelope.payload || typeof envelope.payload !== 'object' || Array.isArray(envelope.payload)) {
        this.protocolError('envelope payload must be an object')
        return
      }
      if (envelope.client_seq === 0) {
        this.handlePush(envelope)
        continue
      }
      const key = `${envelope.cmd}:${envelope.client_seq}`
      const entry = this.pending.get(key)
      if (entry) this.settle(entry, envelope, null)
    }
  }

  handlePush(envelope) {
    this.pushCount += 1
    if (typeof this.options.onPush === 'function') {
      this.options.onPush(envelope, this)
    }
    for (const handler of this.pushHandlers) {
      handler(envelope, this)
    }
  }

  /**
   * 注册 push 处理器。二进制批帧已在进入此回调前展开。
   *
   * @param {(envelope: object, session: FarmSession) => void} handler
   * @returns {() => void}
   */
  onPush(handler) {
    this.pushHandlers.push(handler)
    return () => {
      const index = this.pushHandlers.indexOf(handler)
      if (index >= 0) this.pushHandlers.splice(index, 1)
    }
  }

  protocolError(message) {
    if (typeof this.options.onProtocolError === 'function') {
      this.options.onProtocolError(new Error(`farm protocol: ${message}`), this)
    }
  }

  handshake(callback) {
    return this.request(CMD_HANDSHAKE, {
      token: this.token,
      resume_farm_uid: this.resumeFarmUid,
      resume_farm_seq: this.resumeFarmSeq,
      client_config_ver: CLIENT_CONFIG_VER,
    }, callback)
  }

  ping(callback) {
    return this.request(CMD_PING, {
      client_time: Date.now(),
    }, callback)
  }

  enterFarm(ownerUid = 0, callback = () => {}) {
    return this.request(CMD_ENTER_FARM, {
      owner_uid: ownerUid,
    }, callback)
  }

  /**
   * 当前 Gateway 的请求字段为 from_seq；响应字段为 farm_seq。
   * farmSeq 始终直接透传，避免将 uint64 转成不安全的 Number。
   */
  syncFarm(ownerUid, farmSeq, callback = () => {}) {
    return this.request(CMD_SYNC_FARM, {
      owner_uid: ownerUid,
      from_seq: farmSeq,
    }, callback)
  }

  setTimeout(callback, delayMs) {
    const delay = Math.max(1, Number(delayMs) || 0)
    return this.socket.setTimeout(() => {
      if (!this.closed) callback(this)
    }, delay)
  }

  setInterval(callback, intervalMs) {
    const interval = Math.max(1, Number(intervalMs) || 0)
    return this.socket.setInterval(() => {
      if (!this.closed) callback(this)
    }, interval)
  }

  close(reason = 'client close', expected = false) {
    if (this.closed) return
    this.expectedClose = this.expectedClose || expected
    this.closeReason = reason
    this.closed = true
    if (this.expectedClose && this.options.ignorePendingOnExpectedClose) {
      // 测量窗口到期属于预期关闭；窗口边缘仍在途的请求不应被记作系统错误。
      this.pending.clear()
    } else {
      this.failPending(new Error(`farm protocol: ${reason}`))
    }
    try {
      this.socket.close()
    } catch {
      // close 可能已经由 k6 或服务端触发。
    }
  }

  markSocketClosed() {
    if (this.closed) {
      if (this.expectedClose && this.options.ignorePendingOnExpectedClose) {
        this.pending.clear()
      } else {
        this.failPending(new Error(`farm protocol: ${this.closeReason || 'websocket closed'}`))
      }
      return
    }
    this.closed = true
    this.failPending(new Error('farm protocol: websocket closed'))
  }

  failPending(error) {
    const entries = [...this.pending.values()]
    this.pending.clear()
    for (const entry of entries) {
      if (entry.settled) continue
      entry.settled = true
      entry.callback(null, error)
    }
  }
}

/**
 * 建立连接、自动握手，握手成功后调用 onReady(session, handshakeResponse)。
 *
 * options:
 * - durationMs：到时主动关闭并标记为 expected close
 * - onOpen(session)、onHandshake(response, error, session)、onClose(session, info)
 * - onError(error, session)、onPush(envelope, session)
 *
 * @param {string} url
 * @param {string} token
 * @param {(session: FarmSession, response: object) => void} onReady
 * @param {object} [options]
 * @returns {object} k6/ws 的 HTTP 升级响应
 */
export function withFarmSession(url, token, onReady, options = {}) {
  const params = {
    headers: {
      // Gateway 会拒绝没有该子协议的 Upgrade 请求。
      'Sec-WebSocket-Protocol': WS_SUBPROTOCOL,
    },
    tags: options.tags || {},
  }

  return ws.connect(String(url), params, (socket) => {
    const session = new FarmSession(socket, token, options)
    let closeNotified = false

    socket.on('open', () => {
      session.opened = true
      if (typeof options.onOpen === 'function') {
        options.onOpen(session)
      }

      session.handshake((response, error) => {
        if (typeof options.onHandshake === 'function') {
          options.onHandshake(response, error, session)
        }
        if (error || !response || response.err !== 0) {
          const handshakeError = error || new Error(
            `farm protocol: handshake err=${response ? response.err : 'unknown'}`,
          )
          if (typeof options.onError === 'function') {
            options.onError(handshakeError, session, response)
          }
          session.close('handshake failed', false)
          return
        }
        try {
          onReady(session, response)
        } catch (callbackError) {
          if (typeof options.onError === 'function') {
            options.onError(callbackError, session)
          }
          session.close('onReady callback failed', false)
        }
      })
    })

    socket.on('binaryMessage', (data) => session.onMessage(data))

    socket.on('error', (error) => {
      if (typeof options.onError === 'function') {
        options.onError(error instanceof Error ? error : new Error(String(error)), session)
      }
    })

    socket.on('close', (code, reason) => {
      session.markSocketClosed()
      if (closeNotified) return
      closeNotified = true
      if (typeof options.onClose === 'function') {
        options.onClose(session, {
          code,
          reason,
          expected: session.expectedClose,
          closeReason: session.closeReason,
        })
      }
    })

    if (options.durationMs !== undefined && options.durationMs !== null) {
      const durationMs = Math.max(1, Number(options.durationMs) || 0)
      socket.setTimeout(
        () => session.close('connection duration complete', true),
        durationMs,
      )
    }
  })
}
