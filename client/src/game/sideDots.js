/**
 * 右侧菜单红点：只在「有可操作事项」时亮，避免空邮箱/假本地提示误报。
 */

/**
 * @param {Array<{ read?: boolean, claimed?: boolean, gold?: number, attachmentCoin?: number }>|null|undefined} mails
 * @param {Array|null|undefined} [friendRequests] 待处理邻里申请（展示在邮箱）
 * @returns {boolean}
 */
export function mailDotVisible(mails, friendRequests) {
  if (Array.isArray(friendRequests) && friendRequests.length > 0) return true
  if (!Array.isArray(mails) || mails.length === 0) return false
  return mails.some((m) => {
    const gold = Number(m.gold || m.attachmentCoin) || 0
    // 仅「有未领附件」才亮；纯文案未读不亮（本地「新的一天」等不应劫持红点）
    return !m.claimed && gold > 0
  })
}

/**
 * @param {Array<{ done?: boolean, progress?: number, target?: number, claimed?: boolean }>|null|undefined} tasks
 * @returns {boolean}
 */
export function taskDotVisible(tasks) {
  if (!Array.isArray(tasks) || tasks.length === 0) return false
  return tasks.some((t) => {
    const target = Number(t.target) || 1
    const progress = Number(t.progress) || 0
    const done = t.done === true || progress >= target
    return done && !t.claimed
  })
}
