/**
 * Online 会话标志（期 3）。
 * 登录页完成 Handshake + EnterFarm 后 enterOnline；玩法写路径仅 online。
 */
import { wireUid, wireUint64 } from './jsonSafe.js'

/** @type {{
 *   isOnline: boolean,
 *   uid: string|number|null,
 *   token: string|null,
 *   viewingOwnerUid: string|number|null,
 *   viewingOwnerName: string|null,
 *   lastFarmSeq: number,
 *   farmViewGeneration: number,
 *   serverTimeOffsetMs: number,
 *   relation: 'SELF'|'FRIEND'|null,
 * }} */
export const session = {
  isOnline: false,
  uid: null,
  token: null,
  viewingOwnerUid: null,
  viewingOwnerName: null,
  lastFarmSeq: 0,
  farmViewGeneration: 0,
  serverTimeOffsetMs: 0,
  relation: null,
}

/** 用服务端响应时间校准农场倒计时；receivedAt 默认取收到响应的本地时刻。 */
export function setServerTime(serverTime, receivedAt = Date.now()) {
  const authoritative = Number(serverTime)
  const local = Number(receivedAt)
  if (
    !Number.isSafeInteger(authoritative) ||
    authoritative <= 0 ||
    !Number.isSafeInteger(local) ||
    local <= 0
  ) return false
  session.serverTimeOffsetMs = authoritative - local
  return true
}

export function farmNow(localNow = Date.now()) {
  return Number(localNow) + (Number(session.serverTimeOffsetMs) || 0)
}

/**
 * 进入 online 模式。仅记录会话；WS 连接与 applyPatch 由调用方完成。
 * @param {{ uid: string|number, token: string }} creds
 */
export function enterOnline({ uid, token }) {
  const safeUID = wireUid(uid)
  if (safeUID == null || !token) {
    throw new Error('session: enterOnline requires uid and token')
  }
  session.uid = safeUID
  session.token = token
  session.isOnline = true
}

/** 退出 online；不清空 token，便于重连。 */
export function leaveOnline() {
  session.isOnline = false
  session.viewingOwnerUid = null
  session.viewingOwnerName = null
  session.lastFarmSeq = 0
  session.farmViewGeneration++
  session.relation = null
}

/** 退出登录：除在线农场视图外，一并丢弃内存中的认证凭证。 */
export function logout() {
  leaveOnline()
  session.uid = null
  session.token = null
  session.serverTimeOffsetMs = 0
}

/**
 * 记录 EnterFarm 快照确定的当前房间与 FarmDelta 基准序列。
 * @param {{ ownerUid: string|number, farmSeq: string|number, relation: 'SELF'|'FRIEND', ownerName?: string|null }} view
 */
export function setFarmView({ ownerUid, farmSeq, relation, ownerName = null, serverTime = 0 }) {
  const safeOwnerUID = wireUid(ownerUid)
  const safeFarmSeq = wireUint64(farmSeq)
  if (safeOwnerUID == null || safeFarmSeq == null) {
    throw new Error('session: setFarmView requires exact ownerUid and farmSeq')
  }
  setServerTime(serverTime)
  session.farmViewGeneration++
  session.viewingOwnerUid = safeOwnerUID
  session.lastFarmSeq = safeFarmSeq
  session.relation = relation
  session.viewingOwnerName = relation === 'FRIEND'
    ? (typeof ownerName === 'string' && ownerName.trim() ? ownerName.trim() : null)
    : null
}

/** 是否处于 online 意图路径。 */
export function isOnline() {
  return session.isOnline === true
}
