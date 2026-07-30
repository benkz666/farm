/**
 * Online 会话标志（期 3）。
 * 登录页完成 Handshake + EnterFarm 后 enterOnline；玩法写路径仅 online。
 */

/** @type {{
 *   isOnline: boolean,
 *   uid: number|null,
 *   token: string|null,
 *   viewingOwnerUid: number|null,
 *   viewingOwnerName: string|null,
 *   lastFarmSeq: number,
 *   farmViewGeneration: number,
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
  relation: null,
}

/**
 * 进入 online 模式。仅记录会话；WS 连接与 applyPatch 由调用方完成。
 * @param {{ uid: number, token: string }} creds
 */
export function enterOnline({ uid, token }) {
  if (uid == null || !token) {
    throw new Error('session: enterOnline requires uid and token')
  }
  session.uid = uid
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
}

/**
 * 记录 EnterFarm 快照确定的当前房间与 FarmDelta 基准序列。
 * @param {{ ownerUid: number, farmSeq: number, relation: 'SELF'|'FRIEND', ownerName?: string|null }} view
 */
export function setFarmView({ ownerUid, farmSeq, relation, ownerName = null }) {
  session.farmViewGeneration++
  session.viewingOwnerUid = ownerUid
  session.lastFarmSeq = farmSeq
  session.relation = relation
  session.viewingOwnerName = relation === 'FRIEND'
    ? (typeof ownerName === 'string' && ownerName.trim() ? ownerName.trim() : null)
    : null
}

/** 是否处于 online 意图路径。 */
export function isOnline() {
  return session.isOnline === true
}
