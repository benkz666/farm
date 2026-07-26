/**
 * Online 会话标志（期 2b）。
 * 登录 + Handshake + EnterFarm 成功后由调用方 enterOnline；未登录保持本地权威。
 */

/** @type {{ isOnline: boolean, uid: number|null, token: string|null }} */
export const session = {
  isOnline: false,
  uid: null,
  token: null,
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

/** 退出 online（保留本地模式可用）；不清空 token，便于重连。 */
export function leaveOnline() {
  session.isOnline = false
}

/** 是否处于 online 意图路径。 */
export function isOnline() {
  return session.isOnline === true
}
