/** 期 1/2 联调：HTTP auth + WS Envelope。不写入本地 game/state（由 applyPatch 负责）。 */

export const CMD_HANDSHAKE = 100
export const CMD_PING = 102
export const CMD_ENTER_FARM = 200
export const CMD_LEAVE_FARM = 202
export const CMD_SYNC_FARM = 204
export const CMD_FRIEND_LIST = 400
export const CMD_GEN_SHARE_LINK = 402
export const CMD_ACCEPT_INVITE = 404
export const CMD_REMOVE_FRIEND = 406
export const CMD_ADD_FRIEND_BY_UID = 408
export const CMD_SEARCH_USER = 410
export const CMD_FARM_DELTA = 9000

/** 地块动作（protocol 5.3）。 */
export const CMD_TILL = 206
export const CMD_CLEAR = 208
export const CMD_PLANT = 210
export const CMD_WATER = 212
export const CMD_REMOVE_WEED = 214
export const CMD_REMOVE_PEST = 216
export const CMD_FERTILIZE = 218
export const CMD_HARVEST = 220

/** 商店 */
export const CMD_BUY = 302
export const CMD_SELL = 304

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
    /** @type {Map<number, Set<(envelope: Envelope) => void>>} */
    this._pushHandlers = new Map()
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

  /** 离开当前农场房间（cmd 202）。 */
  leaveFarm() {
    return this.request(CMD_LEAVE_FARM, {})
  }

  /**
   * 从指定序列补齐农场镜像（cmd 204）。
   * @param {number} ownerUid
   * @param {number} fromSeq
   * @returns {Promise<Envelope>}
   */
  syncFarm(ownerUid, fromSeq) {
    return this.request(CMD_SYNC_FARM, { owner_uid: ownerUid, from_seq: fromSeq })
  }

  /** 获取好友列表（cmd 400）。 */
  friendList() {
    return this.request(CMD_FRIEND_LIST, {})
  }

  /** 创建分享链接（cmd 402）。 */
  genShareLink() {
    return this.request(CMD_GEN_SHARE_LINK, {})
  }

  /**
   * AcceptInvite（cmd 404）。
   * @param {string} token
   * @returns {Promise<Envelope>}
   */
  acceptInvite(token) {
    return this.request(CMD_ACCEPT_INVITE, { token })
  }

  /**
   * 删除好友（cmd 406）。
   * @param {number} peerUid
   * @returns {Promise<Envelope>}
   */
  removeFriend(peerUid) {
    return this.request(CMD_REMOVE_FRIEND, { peer_uid: peerUid })
  }

  /**
   * 按 UID 添加好友（cmd 408）。
   * @param {number} peerUid
   * @returns {Promise<Envelope>}
   */
  addFriendByUID(peerUid) {
    return this.request(CMD_ADD_FRIEND_BY_UID, { peer_uid: peerUid })
  }

  /**
   * 按用户名精确查询玩家（cmd 410）。
   * @param {string} username
   * @returns {Promise<Envelope>}
   */
  searchUser(username) {
    return this.request(CMD_SEARCH_USER, { username })
  }

  /**
   * 订阅服务端主动推送，返回取消订阅函数。
   * @param {number} cmd
   * @param {(envelope: Envelope) => void} handler
   * @returns {() => void}
   */
  onPush(cmd, handler) {
    if (!this._pushHandlers.has(cmd)) this._pushHandlers.set(cmd, new Set())
    const handlers = this._pushHandlers.get(cmd)
    handlers.add(handler)
    return () => handlers.delete(handler)
  }

  /**
   * 订阅 FarmDelta（cmd 9000）。
   * @param {(envelope: Envelope) => void} handler
   * @returns {() => void}
   */
  onDelta(handler) {
    return this.onPush(CMD_FARM_DELTA, handler)
  }

  /**
   * 地块动作（Till/Clear/Plant/…）。返回完整 Envelope；err≠0 由调用方处理。
   * @param {number} cmd CMD_TILL 等
   * @param {number} plotIndex
   * @param {number} [arg=0] 播种时为 crop_id，施肥时为 fertilizer_id
   * @param {number} [ownerUid=0]
   * @returns {Promise<Envelope>}
   */
  plotAction(cmd, plotIndex, arg = 0, ownerUid = 0) {
    return this.request(cmd, {
      owner_uid: ownerUid,
      plot_index: plotIndex,
      arg,
    })
  }

  /**
   * Buy（cmd 302）。item_id 为作物数字 ID；err≠0 由调用方处理。
   * @param {number} itemId
   * @param {number} [quantity=1]
   * @returns {Promise<Envelope>}
   */
  buy(itemId, quantity = 1) {
    return this.request(CMD_BUY, { item_id: itemId, quantity })
  }

  /**
   * Sell（cmd 304）。卖仓库果实；err≠0 由调用方处理。
   * @param {number} itemId
   * @param {number} [quantity=1]
   * @returns {Promise<Envelope>}
   */
  sell(itemId, quantity = 1) {
    return this.request(CMD_SELL, { item_id: itemId, quantity })
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
      const error = new Error(`net: ${path} failed err=${err}`)
      error.code = err
      throw error
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
    if (envelope.client_seq === 0) {
      for (const handler of this._pushHandlers.get(envelope.cmd) || []) {
        handler(envelope)
      }
      return
    }
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
