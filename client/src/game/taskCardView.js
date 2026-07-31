/**
 * 任务卡片纯视图模型：Task 4（每日登录）与其它任务同一决策路径。
 */
/**
 * @typedef {{
 *   id: number|null,
 *   name: string,
 *   progress: number,
 *   target: number,
 *   reward: number,
 *   done: boolean,
 *   claimed: boolean,
 *   pct: number,
 *   statusTag: 'claimed'|'claimable'|'in_progress',
 *   claimAction: null|{ type: 'claimTask', taskId: number },
 * }} TaskCardViewModel
 */

/**
 * @param {object} task
 * @returns {TaskCardViewModel}
 */
export function taskCardViewModel(task) {
  const id = task?.id != null && Number.isFinite(Number(task.id)) ? Number(task.id) : null
  const name = typeof task?.title === 'string' ? task.title : ''
  const target = Number.isFinite(Number(task?.target)) ? Number(task.target) : 0
  const progress = Number.isFinite(Number(task?.progress)) ? Number(task.progress) : 0
  const reward = Number.isFinite(Number(task?.rewardCoin)) ? Number(task.rewardCoin) : 0
  const done = target > 0 && (task?.done === true || progress >= target)
  const claimed = !!task?.claimed
  const pct = target > 0 ? Math.min(1, progress / target) : 0

  /** @type {TaskCardViewModel['statusTag']} */
  let statusTag = 'in_progress'
  if (claimed) statusTag = 'claimed'
  else if (done) statusTag = 'claimable'

  // 每日登录（id=4）与其它任务相同：可领时走 claimTask，无 614 分支
  const claimAction =
    done && !claimed && id != null
      ? { type: 'claimTask', taskId: id }
      : null

  return {
    id,
    name,
    progress: target > 0 ? Math.min(progress, target) : 0,
    target,
    reward,
    done,
    claimed,
    pct,
    statusTag,
    claimAction,
  }
}
