/** 期 1 联调：HTTP auth + WS Envelope。不写入本地 game/state。 */

export const CMD_HANDSHAKE = 100
export const CMD_PING = 102
export const CMD_ENTER_FARM = 200

export const WS_SUBPROTOCOL = 'farm.v1.json'
export const CLIENT_CONFIG_VER = 1

/**
 * @typedef {{ uid: number, token: string, ws_url: string }} AuthResult
 * @typedef {{ cmd: number, client_seq: number, err: number, payload: object }} Envelope
 */

export class NetClient {
  constructor() {
    /** @type {string|null} */
    this.token = null
    /** @type {number|null} */
    this.uid = null
    /** @type {string|null} */
    this.wsUrl = null
    /** @type {WebSocket|null} */
    this._ws = null
    this._clientSeq = 0
    /** @type {Map<number, { resolve: (e: Envelope) => void, reject: (e: Error) => void, cmd: number }>} */
    this._pending = new Map()
  }

  /**
   * @param {string} username
   * @param {string} password
   * @returns {Promise<AuthResult>}
   */
  register(username, password) {
    return this._authenticate('/api/register', username, password)
  }

  /**
   * @param {string} username
   * @param {string} password
   * @returns {Promise<AuthResult>}
   */
  login(username, password) {
    return this._authenticate('/api/login', username, password)
  }

  /**
   * 建立 WebSocket；需已持有 token。优先用登录返回的 ws_url。
   * @param {string} [wsUrl]
   * @returns {Promise<void>}
   */
  connect(wsUrl) {
    const url = wsUrl || this.wsUrl || defaultWsUrl()
    if (!this.token) {
      return Promise.reject(new Error('net: missing token; register/login first'))
    }
    this.close()
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url, WS_SUBPROTOCOL)
      let settled = false

      ws.onopen = () => {
        settled = true
        this._ws = ws
        resolve()
      }
      ws.onerror = () => {
        if (!settled) {
          settled = true
          reject(new Error(`net: websocket error connecting to ${url}`))
        }
      }
      ws.onclose = () => {
        this._failAllPending(new Error('net: websocket closed'))
        if (this._ws === ws) this._ws = null
        if (!settled) {
          settled = true
          reject(new Error(`net: websocket closed before open (${url})`))
        }
      }
      ws.onmessage = (event) => this._onMessage(event)
    })
  }

  close() {
    if (this._ws) {
      this._ws.onopen = null
      this._ws.onmessage = null
      this._ws.onerror = null
      this._ws.onclose = null
      this._ws.close()
      this._ws = null
    }
    this._failAllPending(new Error('net: connection closed'))
  }

  /**
   * Handshake（cmd 100）。resume_* 可省略。
   * @returns {Promise<Envelope>}
   */
  handshake() {
    if (!this.token) {
      return Promise.reject(new Error('net: missing token'))
    }
    return this.request(CMD_HANDSHAKE, {
      token: this.token,
      client_config_ver: CLIENT_CONFIG_VER,
    })
  }

  /**
   * EnterFarm（cmd 200）。owner_uid 为 0 表示自己的农场。
   * @param {number} [ownerUid=0]
   * @returns {Promise<Envelope>}
   */
  enterFarm(ownerUid = 0) {
    return this.request(CMD_ENTER_FARM, { owner_uid: ownerUid })
  }

  /**
   * 发送 Envelope，按 client_seq 匹配应答。
   * @param {number} cmd
   * @param {object} payload
   * @returns {Promise<Envelope>}
   */
  request(cmd, payload = {}) {
    if (!this._ws || this._ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(new Error('net: websocket not open'))
    }
    const client_seq = ++this._clientSeq
    /** @type {Envelope} */
    const envelope = { cmd, client_seq, err: 0, payload }
    return new Promise((resolve, reject) => {
      this._pending.set(client_seq, { resolve, reject, cmd })
      try {
        this._ws.send(JSON.stringify(envelope))
      } catch (err) {
        this._pending.delete(client_seq)
        reject(err instanceof Error ? err : new Error(String(err)))
      }
    })
  }

  /**
   * @param {string} path
   * @param {string} username
   * @param {string} password
   * @returns {Promise<AuthResult>}
   */
  async _authenticate(path, username, password) {
    const response = await fetch(path, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ username, password }),
    })
    const body = await response.json().catch(() => ({}))
    if (!response.ok) {
      const err = body && typeof body.err === 'number' ? body.err : response.status
      throw new Error(`net: ${path} failed err=${err}`)
    }
    if (!body.token || !body.ws_url) {
      throw new Error(`net: ${path} missing token/ws_url`)
    }
    this.uid = body.uid
    this.token = body.token
    this.wsUrl = body.ws_url
    return { uid: body.uid, token: body.token, ws_url: body.ws_url }
  }

  /** @param {MessageEvent} event */
  _onMessage(event) {
    let envelope
    try {
      envelope = JSON.parse(typeof event.data === 'string' ? event.data : String(event.data))
    } catch {
      return
    }
    if (!envelope || typeof envelope.client_seq !== 'number') return
    const pending = this._pending.get(envelope.client_seq)
    if (!pending) return
    if (pending.cmd !== envelope.cmd) return
    this._pending.delete(envelope.client_seq)
    pending.resolve(envelope)
  }

  /** @param {Error} err */
  _failAllPending(err) {
    for (const [, pending] of this._pending) {
      pending.reject(err)
    }
    this._pending.clear()
  }
}

function defaultWsUrl() {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${location.host}/ws`
}
