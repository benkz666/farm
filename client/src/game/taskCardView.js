/**
 * 任务卡片纯视图模型：Task 4（每日登录）与其它任务同一决策路径。
 */
import { TASK_POOL } from './config.js'

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
 * @param {typeof TASK_POOL} [pool]
 * @returns {TaskCardViewModel}
 */
export function taskCardViewModel(task, pool = TASK_POOL) {
  const id = task?.id != null && Number.isFinite(Number(task.id)) ? Number(task.id) : null
  const def = pool.find((d) => d.id === task?.taskId) || pool.find((d) => String(d.id) === String(id))
  const name = task?.title || def?.name || `任务 ${id ?? task?.taskId ?? ''}`
  const target = Number(task?.target) || def?.target || 1
  const progress = Number(task?.progress) || 0
  const reward = task?.rewardCoin ?? def?.gold ?? 0
  const done = task?.done === true || progress >= target
  const claimed = !!task?.claimed
  const pct = Math.min(1, progress / target)

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
    progress: Math.min(progress, target),
    target,
    reward: Number(reward) || 0,
    done,
    claimed,
    pct,
    statusTag,
    claimAction,
  }
}
