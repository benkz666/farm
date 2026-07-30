/** 右侧菜单红点：邮件按未读状态，任务按可领取状态。 */

/**
 * @param {Array<{ read?: boolean }>|null|undefined} mails
 * @param {Array|null|undefined} [friendRequests] 待处理邻里申请（展示在邮箱）
 * @returns {boolean}
 */
export function mailDotVisible(mails, friendRequests) {
  if (Array.isArray(friendRequests) && friendRequests.length > 0) return true
  if (!Array.isArray(mails) || mails.length === 0) return false
  return mails.some((m) => m?.read !== true)
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
