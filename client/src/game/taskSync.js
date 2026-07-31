/**
 * 服务端任务列表 / TaskNotify 映射与合并（纯函数，可单测）。
 */

/**
 * @param {object} t
 * @returns {{
 *   id: number,
 *   dayKey: number,
 *   kind: 'fixed'|'random',
 *   taskId: string,
 *   title: string,
 *   progress: number,
 *   target: number,
 *   rewardCoin: number,
 *   done: boolean,
 *   claimed: boolean,
 * }|null}
 */
export function mapServerTask(t) {
  const id = Number(t?.id)
  const dayKey = Number(t?.day_key)
  const progress = Number(t?.progress)
  const target = Number(t?.target)
  const rewardCoin = Number(t?.reward_coin)
  const title = typeof t?.title === 'string' ? t.title.trim() : ''

  // 标题、目标和奖励均是服务端任务定义的一部分。客户端不为缺失字段
  // 自行推导默认值，以免旧包或异常推送被误展示为一条“本地任务”。
  if (
    !Number.isSafeInteger(id) || id <= 0 ||
    !Number.isSafeInteger(dayKey) || dayKey <= 0 ||
    !Number.isSafeInteger(progress) || progress < 0 ||
    !Number.isSafeInteger(target) || target <= 0 ||
    !Number.isSafeInteger(rewardCoin) || rewardCoin < 0 ||
    !title || typeof t?.claimed !== 'boolean'
  ) {
    return null
  }

  return {
    id,
    dayKey,
    kind: t?.kind === 'fixed' ? 'fixed' : 'random',
    taskId: String(id),
    title,
    progress,
    target,
    rewardCoin,
    done: progress >= target,
    claimed: !!t?.claimed,
  }
}

/**
 * @param {unknown} tasks
 */
export function mapServerTasks(tasks) {
  const mapped = []
  for (const task of Array.isArray(tasks) ? tasks : []) {
    const next = mapServerTask(task)
    if (next) mapped.push(next)
  }
  return mapped
}

/**
 * 按任务 id 合并/替换权威任务状态；不触碰金币。
 * @param {Array<object>|null|undefined} tasks
 * @param {object} payload TaskNotify 完整任务对象
 */
export function applyTaskNotify(tasks, payload) {
  const next = mapServerTask(payload || {})
  if (!next) return null
  const list = Array.isArray(tasks) ? [...tasks] : []
  const idx = list.findIndex((t) => Number(t.id) === next.id)
  if (idx >= 0) list[idx] = next
  else list.push(next)
  return list
}
