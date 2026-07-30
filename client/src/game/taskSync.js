/**
 * 服务端任务列表 / TaskNotify 映射与合并（纯函数，可单测）。
 */

/**
 * @param {object} t
 * @returns {{
 *   id: number,
 *   taskId: string,
 *   title: string,
 *   progress: number,
 *   target: number,
 *   rewardCoin: number,
 *   done: boolean,
 *   claimed: boolean,
 * }}
 */
export function mapServerTask(t) {
  const id = Number(t?.id)
  const progress = Number(t?.progress) || 0
  const target = Number(t?.target) || 1
  return {
    id,
    taskId: String(id),
    title: t?.title || `任务 ${id}`,
    progress,
    target,
    rewardCoin: Number(t?.reward_coin) || 0,
    done: progress >= target,
    claimed: !!t?.claimed,
  }
}

/**
 * @param {unknown} tasks
 */
export function mapServerTasks(tasks) {
  return (Array.isArray(tasks) ? tasks : []).map(mapServerTask)
}

/**
 * 按任务 id 合并/替换权威任务状态；不触碰金币。
 * @param {Array<object>|null|undefined} tasks
 * @param {object} payload TaskNotify 完整任务对象
 */
export function applyTaskNotify(tasks, payload) {
  const next = mapServerTask(payload || {})
  if (!Number.isFinite(next.id) || next.id <= 0) {
    return Array.isArray(tasks) ? [...tasks] : []
  }
  const list = Array.isArray(tasks) ? [...tasks] : []
  const idx = list.findIndex((t) => Number(t.id) === next.id)
  if (idx >= 0) list[idx] = next
  else list.push(next)
  return list
}
