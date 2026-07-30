/** 期 1/2 联调：HTTP auth + WS Envelope。不写入本地 game/state（由 applyPatch 负责）。 */

import { parseJSONSafe, wireUid } from './jsonSafe.js'

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
export const CMD_REQUEST_FRIEND = 412
export const CMD_LIST_FRIEND_REQUESTS = 414
export const CMD_ACCEPT_FRIEND_REQUEST = 416
export const CMD_REJECT_FRIEND_REQUEST = 418
export const CMD_FARM_DELTA = 9000
export const CMD_PLAYER_DELTA = 9002
export const CMD_MAIL_NOTIFY = 9004
export const CMD_TASK_NOTIFY = 9008

/** 地块动作（protocol 5.3）。 */
export const CMD_TILL = 206
export const CMD_CLEAR = 208
export const CMD_PLANT = 210
export const CMD_WATER = 212
export const CMD_REMOVE_WEED = 214
export const CMD_REMOVE_PEST = 216
export const CMD_FERTILIZE = 218
export const CMD_HARVEST = 220
export const CMD_STEAL = 222

/** 商店 */
export const CMD_BUY = 302
export const CMD_SELL = 304

/** 宠物（protocol 5.6） */
export const CMD_PET_STATUS = 500
export const CMD_PET_ACTIVATE = 502
export const CMD_PET_FEED = 504

/** 任务 / 邮件 / 每日登录（protocol 5.7） */
export const CMD_TASK_LIST = 600
export const CMD_TASK_CLAIM = 602
export const CMD_MAIL_LIST = 604
export const CMD_MAIL_CLAIM = 608
export const CMD_CLAIM_DAILY_LOGIN = 614

export const WS_SUBPROTOCOL = 'farm.v1.json'
export const CLIENT_CONFIG_VER = 1

/** 默认请求超时（毫秒）。 */
export const DEFAULT_REQUEST_TIMEOUT_MS = 10_000
/** 重连初始退避（毫秒）。 */
export const DEFAULT_RECONNECT_BASE_MS = 500
/** 重连最大退避（毫秒）。 */
export const DEFAULT_RECONNECT_MAX_MS = 30_000

/** 停止自动重连的致命错误（鉴权 / 配置过期）。 */
const FATAL_RECONNECT_ERRS = new Set([1007, 1101, 1102, 1105])
/** 非好友，EnterFarm 好友农场时回退自己。 */
const ERR_NOT_FRIEND = 1401

/**
 * @typedef {{ uid: number, token: string, ws_url: string }} AuthResult
 * @typedef {{ cmd: number, client_seq: number, err: number, payload: object }} Envelope
 * @typedef {{ resume_farm_uid: number, resume_farm_seq: number }} ResumeContext
 * @typedef {{
 *   WebSocket?: typeof WebSocket,
 *   setTimeout?: typeof setTimeout,
 *   clearTimeout?: typeof clearTimeout,
 *   random?: () => number,
 *   requestTimeoutMs?: number,
 *   reconnectBaseMs?: number,
 *   reconnectMaxMs?: number,
 *   getResumeContext?: () => ResumeContext,
 * }} NetClientOptions
 */

export class NetClient {
  /**
   * @param {NetClientOptions} [options]
   */
  constructor(options = {}) {
    /** @type {string|null} */
    this.token = null
    /** @type {number|null} */
    this.uid = null
    /** @type {string|null} */
    this.wsUrl = null
    /** @type {WebSocket|null} */
    this._ws = null
    this._clientSeq = 0
    /** @type {Map<number, { resolve: (e: Envelope) => void, reject: (e: Error) => void, cmd: number, timer: ReturnType<typeof setTimeout>|null, settled: boolean }>} */
    this._pending = new Map()
    /** @type {Map<number, Set<(envelope: Envelope) => void>>} */
    this._pushHandlers = new Map()
    /** @type {Set<(envelope: Envelope) => void>} */
    this._farmRestoredHandlers = new Set()
    /** @type {Set<(reason: Error|Envelope) => void>} */
    this._farmRestoreFailedHandlers = new Set()

    this._WebSocket = options.WebSocket ?? globalThis.WebSocket
    this._setTimeout = options.setTimeout ?? ((fn, ms) => globalThis.setTimeout(fn, ms))
    this._clearTimeout = options.clearTimeout ?? ((id) => globalThis.clearTimeout(id))
    this._random = options.random ?? Math.random.bind(Math)
    this.requestTimeoutMs = options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS
    this.reconnectBaseMs = options.reconnectBaseMs ?? DEFAULT_RECONNECT_BASE_MS
    this.reconnectMaxMs = options.reconnectMaxMs ?? DEFAULT_RECONNECT_MAX_MS
    /** @type {() => ResumeContext} */
    this.getResumeContext =
      options.getResumeContext ??
      (() => ({ resume_farm_uid: 0, resume_farm_seq: 0 }))

    /** 显式 connect 成功后启用；主动 close 关闭。 */
    this._autoReconnect = false
    /** 曾成功 open 过：仅此后意外断线才自动重连（初次 connect 失败不重试）。 */
    this._hadOpenConnection = false
    this._connecting = false
    /** @type {ReturnType<typeof setTimeout>|null} */
    this._reconnectTimer = null
    this._reconnectAttempt = 0
    /** 防止重入恢复流程。 */
    this._restoring = false
    this._restoreGeneration = 0
    /** @type {WebSocket|null} 已调用 open、尚未赋值给 _ws 的连接中 socket */
    this._openingWs = null
    /** 致命恢复失败后的终止态；显式 connect 可清除。 */
    this._fatalStopped = false
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
   * 初次连接失败不自动重试；成功 open 后意外断线才会退避重连。
   * @param {string} [wsUrl]
   * @returns {Promise<void>}
   */
  connect(wsUrl) {
    const url = wsUrl || this.wsUrl || defaultWsUrl()
    if (!this.token) {
      return Promise.reject(new Error('net: missing token; register/login first'))
    }
    this._fatalStopped = false
    this._autoReconnect = true
    this._cancelReconnectTimer()
    this._reconnectAttempt = 0
    this._restoreGeneration++
    this._restoring = false
    this._connecting = false
    this._detachSocket()
    this._failAllPending(new Error('net: connection closed'))
    return this._openSocket(url, { manual: true })
  }

  /**
   * 主动关闭：取消重连、拒绝在途请求。后续显式 connect 可重新启用自动重连。
   */
  close() {
    this._autoReconnect = false
    this._cancelReconnectTimer()
    this._restoreGeneration++
    this._restoring = false
    this._connecting = false
    this._detachSocket()
    this._failAllPending(new Error('net: connection closed'))
  }

  /**
   * Handshake（cmd 100）。自动附带 getResumeContext() 的 resume_*。
   * @returns {Promise<Envelope>}
   */
  handshake() {
    if (!this.token) {
      return Promise.reject(new Error('net: missing token'))
    }
    const resume = this._resumeContext()
    return this.request(CMD_HANDSHAKE, {
      token: this.token,
      client_config_ver: CLIENT_CONFIG_VER,
      resume_farm_uid: resume.resume_farm_uid,
      resume_farm_seq: resume.resume_farm_seq,
    })
  }

  /**
   * EnterFarm（cmd 200）。owner_uid 为 0 表示自己的农场。
   * @param {number} [ownerUid=0]
   * @returns {Promise<Envelope>}
   */
  enterFarm(ownerUid = 0) {
    const uid = wireUid(ownerUid)
    return this.request(CMD_ENTER_FARM, { owner_uid: uid == null ? 0 : uid })
  }

  /** 离开当前农场房间（cmd 202）。 */
  leaveFarm() {
    return this.request(CMD_LEAVE_FARM, {})
  }

  /**
   * 从指定序列补齐农场镜像（cmd 204）。
   * @param {string|number} ownerUid
   * @param {number} fromSeq
   * @returns {Promise<Envelope>}
   */
  syncFarm(ownerUid, fromSeq) {
    const uid = wireUid(ownerUid)
    return this.request(CMD_SYNC_FARM, {
      owner_uid: uid == null ? 0 : uid,
      from_seq: fromSeq,
    })
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
    return this.request(CMD_REMOVE_FRIEND, { peer_uid: wireUid(peerUid) })
  }

  /**
   * 按 UID 添加好友（cmd 408）。
   * @param {string|number} peerUid
   * @returns {Promise<Envelope>}
   */
  addFriendByUID(peerUid) {
    return this.request(CMD_ADD_FRIEND_BY_UID, { peer_uid: wireUid(peerUid) })
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
   * 发起好友申请（cmd 412）。搜索添加走申请；分享链接仍用 acceptInvite。
   * @param {number} peerUid
   */
  requestFriend(peerUid) {
    const uid = wireUid(peerUid)
    return this.request(CMD_REQUEST_FRIEND, { peer_uid: uid })
  }

  /** 收到的好友申请列表（cmd 414）。 */
  listFriendRequests() {
    return this.request(CMD_LIST_FRIEND_REQUESTS, {})
  }

  /**
   * 同意好友申请（cmd 416）。
   * @param {string|number} fromUid
   */
  acceptFriendRequest(fromUid) {
    const uid = wireUid(fromUid)
    return this.request(CMD_ACCEPT_FRIEND_REQUEST, { from_uid: uid })
  }

  /**
   * 拒绝好友申请（cmd 418）。
   * @param {string|number} fromUid
   */
  rejectFriendRequest(fromUid) {
    const uid = wireUid(fromUid)
    return this.request(CMD_REJECT_FRIEND_REQUEST, { from_uid: uid })
  }

  /**
   * 偷菜（cmd 222）。拜访好友农场时使用；不做数量乐观预测。
   * @param {number} ownerUid
   * @param {number} plotIndex
   * @param {number} cropId
   * @returns {Promise<Envelope>}
   */
  steal(ownerUid, plotIndex, cropId) {
    const uid = wireUid(ownerUid)
    return this.request(CMD_STEAL, {
      owner_uid: uid == null ? 0 : uid,
      plot_index: plotIndex,
      crop_id: cropId,
    })
  }

  /** 狗状态（cmd 500）。 */
  petStatus() {
    return this.request(CMD_PET_STATUS, {})
  }

  /**
   * 启用狗（cmd 502）。
   * @param {number} dogType
   * @returns {Promise<Envelope>}
   */
  petActivate(dogType) {
    return this.request(CMD_PET_ACTIVATE, { dog_type: dogType })
  }

  /**
   * 喂狗粮（cmd 504）。
   * @param {number} grams
   * @returns {Promise<Envelope>}
   */
  petFeed(grams) {
    return this.request(CMD_PET_FEED, { grams })
  }

  /** 当日任务列表（cmd 600）。 */
  taskList() {
    return this.request(CMD_TASK_LIST, {})
  }

  /**
   * 领取任务奖励（cmd 602）；奖励直接入账。
   * @param {number} taskId
   * @returns {Promise<Envelope>}
   */
  taskClaim(taskId) {
    return this.request(CMD_TASK_CLAIM, { task_id: taskId })
  }

  /** 邮件列表（cmd 604）。 */
  mailList() {
    return this.request(CMD_MAIL_LIST, {})
  }

  /**
   * 领取邮件附件（cmd 608）。
   * @param {number} mailId
   * @returns {Promise<Envelope>}
   */
  mailClaim(mailId) {
    return this.request(CMD_MAIL_CLAIM, { mail_id: mailId })
  }

  /** 领取每日登录奖励（cmd 614）。 */
  claimDailyLogin() {
    return this.request(CMD_CLAIM_DAILY_LOGIN, {})
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
   * 订阅自己的金币、经验与背包变化（cmd 9002）。
   * @param {(envelope: Envelope) => void} handler
   * @returns {() => void}
   */
  onPlayerDelta(handler) {
    return this.onPush(CMD_PLAYER_DELTA, handler)
  }

  /**
   * MailNotify（cmd 9004）个人通知：邻里申请 / 新邮件提示。
   * @param {(env: Envelope) => void} handler
   */
  onMailNotify(handler) {
    return this.onPush(CMD_MAIL_NOTIFY, handler)
  }

  /**
   * TaskNotify（cmd 9008）个人通知：单条每日任务权威状态。
   * @param {(env: Envelope) => void} handler
   * @returns {() => void}
   */
  onTaskNotify(handler) {
    return this.onPush(CMD_TASK_NOTIFY, handler)
  }

  /**
   * 重连并完成 EnterFarm 权威恢复后通知（全量快照）。返回取消订阅函数。
   * @param {(envelope: Envelope) => void} handler
   * @returns {() => void}
   */
  onFarmRestored(handler) {
    this._farmRestoredHandlers.add(handler)
    return () => this._farmRestoredHandlers.delete(handler)
  }

  /**
   * 恢复失败且停止自动重连时通知（如鉴权失效）。返回取消订阅函数。
   * @param {(reason: Error|Envelope) => void} handler
   * @returns {() => void}
   */
  onFarmRestoreFailed(handler) {
    this._farmRestoreFailedHandlers.add(handler)
    return () => this._farmRestoreFailedHandlers.delete(handler)
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
    const uid = wireUid(ownerUid)
    return this.request(cmd, {
      owner_uid: uid == null ? 0 : uid,
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
   * 发送 Envelope，按 client_seq 匹配应答。默认超时；超时/关闭时清 timer，不二次 reject。
   * @param {number} cmd
   * @param {object} payload
   * @returns {Promise<Envelope>}
   */
  request(cmd, payload = {}) {
    if (!this._ws || this._ws.readyState !== this._WebSocket.OPEN) {
      return Promise.reject(new Error('net: websocket not open'))
    }
    const client_seq = ++this._clientSeq
    /** @type {Envelope} */
    const envelope = { cmd, client_seq, err: 0, payload }
    const ws = this._ws
    return new Promise((resolve, reject) => {
      /** @type {{ resolve: (e: Envelope) => void, reject: (e: Error) => void, cmd: number, timer: ReturnType<typeof setTimeout>|null, settled: boolean }} */
      const entry = {
        cmd,
        timer: null,
        settled: false,
        resolve: (env) => {
          if (entry.settled) return
          entry.settled = true
          if (entry.timer != null) this._clearTimeout(entry.timer)
          entry.timer = null
          resolve(env)
        },
        reject: (err) => {
          if (entry.settled) return
          entry.settled = true
          if (entry.timer != null) this._clearTimeout(entry.timer)
          entry.timer = null
          reject(err)
        },
      }
      entry.timer = this._setTimeout(() => {
        if (entry.settled) return
        this._pending.delete(client_seq)
        entry.reject(new Error(`net: request timeout cmd=${cmd} seq=${client_seq}`))
      }, this.requestTimeoutMs)
      this._pending.set(client_seq, entry)
      try {
        ws.send(JSON.stringify(envelope))
      } catch (err) {
        this._pending.delete(client_seq)
        entry.reject(err instanceof Error ? err : new Error(String(err)))
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
    const raw = await response.text()
    let body = {}
    try {
      body = raw ? parseJSONSafe(raw) : {}
    } catch {
      body = {}
    }
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

  /**
   * @param {string} url
   * @param {{ manual?: boolean, afterReconnect?: boolean }} [opts]
   * @returns {Promise<void>}
   */
  _openSocket(url, opts = {}) {
    if (this._connecting) {
      return Promise.reject(new Error('net: connection already in progress'))
    }
    this._connecting = true
    return new Promise((resolve, reject) => {
      let settled = false
      let ws
      try {
        ws = new this._WebSocket(url, WS_SUBPROTOCOL)
      } catch (err) {
        this._connecting = false
        reject(err instanceof Error ? err : new Error(String(err)))
        return
      }
      this._openingWs = ws

      ws.onopen = () => {
        if (settled) return
        if (this._openingWs !== ws && this._ws !== ws) return
        settled = true
        this._connecting = false
        this._openingWs = null
        this._ws = ws
        this._hadOpenConnection = true
        resolve()
        if (opts.afterReconnect) {
          void this._restoreAfterReconnect()
        }
      }
      ws.onerror = () => {
        if (settled) return
        // 等 onclose 统一处理，避免与 close 双 settle
      }
      ws.onclose = () => {
        if (this._openingWs === ws) this._openingWs = null
        this._handleSocketClose(ws, {
          beforeOpen: !settled,
          settleConnect: (err) => {
            if (settled) return
            settled = true
            this._connecting = false
            reject(err)
          },
        })
      }
      ws.onmessage = (event) => this._onMessage(ws, event)
    })
  }

  /**
   * @param {WebSocket} ws
   * @param {{ beforeOpen: boolean, settleConnect?: (err: Error) => void }} ctx
   */
  _handleSocketClose(ws, ctx) {
    if (this._ws === ws) this._ws = null
    this._failAllPending(new Error('net: websocket closed'))

    if (ctx.beforeOpen) {
      ctx.settleConnect?.(new Error(`net: websocket closed before open`))
      // 初次 / 手动 connect 未 open 成功：不自动重试
      // 重连尝试未 open：继续退避
      if (!ctx.settleConnect && this._autoReconnect && this.token && this._hadOpenConnection) {
        this._scheduleReconnect()
      }
      return
    }

    if (this._autoReconnect && this.token && this._hadOpenConnection) {
      this._scheduleReconnect()
    }
  }

  _scheduleReconnect() {
    if (!this._autoReconnect || !this.token) return
    if (this._reconnectTimer != null) return
    if (this._connecting) return
    if (this._ws) return

    const exp = Math.min(
      this.reconnectMaxMs,
      this.reconnectBaseMs * 2 ** this._reconnectAttempt,
    )
    this._reconnectAttempt += 1
    const delay = Math.max(0, exp * (0.5 + this._random() * 0.5))
    this._reconnectTimer = this._setTimeout(() => {
      this._reconnectTimer = null
      void this._reconnectNow()
    }, delay)
  }

  async _reconnectNow() {
    if (!this._autoReconnect || !this.token) return
    if (this._connecting || this._ws) return
    const url = this.wsUrl || defaultWsUrl()
    try {
      await this._openSocket(url, { afterReconnect: true })
    } catch {
      if (this._autoReconnect && this.token && this._hadOpenConnection) {
        this._scheduleReconnect()
      }
    }
  }

  async _restoreAfterReconnect() {
    if (!this._autoReconnect || !this.token) return
    if (this._restoring) return
    this._restoring = true
    const gen = ++this._restoreGeneration
    try {
      const resume = this._resumeContext()
      const hs = await this.handshake()
      if (gen !== this._restoreGeneration) return
      if (hs.err !== 0) {
        if (FATAL_RECONNECT_ERRS.has(hs.err)) {
          this._stopReconnectWithFailure(hs)
        } else if (this._autoReconnect) {
          this._detachSocket()
          this._scheduleReconnect()
        }
        return
      }

      let enter = await this.enterFarm(resume.resume_farm_uid || 0)
      if (gen !== this._restoreGeneration) return
      if (
        enter.err === ERR_NOT_FRIEND &&
        resume.resume_farm_uid &&
        resume.resume_farm_uid !== 0
      ) {
        enter = await this.enterFarm(0)
        if (gen !== this._restoreGeneration) return
      }

      if (enter.err !== 0) {
        if (FATAL_RECONNECT_ERRS.has(enter.err)) {
          this._stopReconnectWithFailure(enter)
        } else if (this._autoReconnect) {
          this._detachSocket()
          this._scheduleReconnect()
        } else {
          this._emitFarmRestoreFailed(enter)
        }
        return
      }

      this._reconnectAttempt = 0
      for (const handler of this._farmRestoredHandlers) {
        try {
          handler(enter)
        } catch {
          // 恢复回调失败不阻断其它订阅者
        }
      }
    } catch (err) {
      if (gen !== this._restoreGeneration) return
      // 在途请求已因断线 reject；若仍连着则继续退避
      if (this._autoReconnect && this.token && this._hadOpenConnection) {
        if (!this._ws) this._scheduleReconnect()
        else {
          this._detachSocket()
          this._scheduleReconnect()
        }
      } else {
        this._emitFarmRestoreFailed(err instanceof Error ? err : new Error(String(err)))
      }
    } finally {
      if (gen === this._restoreGeneration) this._restoring = false
    }
  }

  /**
   * 致命恢复失败：进入真正终止态（停重连、清 pending/timer、关 socket），failure 只通知一次。
   * @param {Envelope|Error} reason
   */
  _stopReconnectWithFailure(reason) {
    if (this._fatalStopped) return
    this._fatalStopped = true
    this._autoReconnect = false
    this._cancelReconnectTimer()
    this._restoreGeneration++
    this._restoring = false
    this._connecting = false
    this._failAllPending(new Error('net: reconnect aborted'))
    this._detachSocket()
    this._emitFarmRestoreFailed(reason)
  }

  /** @param {Envelope|Error} reason */
  _emitFarmRestoreFailed(reason) {
    for (const handler of this._farmRestoreFailedHandlers) {
      try {
        handler(reason)
      } catch {
        // ignore
      }
    }
  }

  _cancelReconnectTimer() {
    if (this._reconnectTimer != null) {
      this._clearTimeout(this._reconnectTimer)
      this._reconnectTimer = null
    }
  }

  /** 卸下当前 socket（含连接中），旧事件不再进入本客户端。 */
  _detachSocket() {
    const sockets = [this._ws, this._openingWs]
    this._ws = null
    this._openingWs = null
    for (const ws of sockets) {
      if (!ws) continue
      ws.onopen = null
      ws.onmessage = null
      ws.onerror = null
      ws.onclose = null
      try {
        if (
          ws.readyState === this._WebSocket.CONNECTING ||
          ws.readyState === this._WebSocket.OPEN
        ) {
          ws.close()
        }
      } catch {
        // ignore
      }
    }
  }

  /** @returns {ResumeContext} */
  _resumeContext() {
    try {
      const ctx = this.getResumeContext?.() || {}
      return {
        resume_farm_uid: Number(ctx.resume_farm_uid) || 0,
        resume_farm_seq: Number(ctx.resume_farm_seq) || 0,
      }
    } catch {
      return { resume_farm_uid: 0, resume_farm_seq: 0 }
    }
  }

  /**
   * @param {WebSocket|MessageEvent} wsOrEvent
   * @param {MessageEvent} [maybeEvent]
   */
  _onMessage(wsOrEvent, maybeEvent) {
    const event = maybeEvent === undefined ? wsOrEvent : maybeEvent
    const ws = maybeEvent === undefined ? this._ws : wsOrEvent
    if (ws != null && this._ws != null && this._ws !== ws) return
    let envelope
    try {
      envelope = parseJSONSafe(typeof event.data === 'string' ? event.data : String(event.data))
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
    const entries = [...this._pending.values()]
    this._pending.clear()
    for (const pending of entries) {
      pending.reject(err)
    }
  }
}

function defaultWsUrl() {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${location.host}/ws`
}
