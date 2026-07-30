import { CODEX_PLAQUE_STAGES } from './config.js'

const TIER_LABEL = Object.freeze({
  locked: '未解锁',
  wood: '收藏木牌',
  bronze: '铜牌',
  silver: '银牌',
  gold: '金牌',
})

/**
 * 将权威收获次数投影为牌子、下一阶段进度和奖励提示。
 * @param {{ harvestCount?: number, tier?: string, nextTarget?: number }|null|undefined} entry
 */
export function codexPlaqueViewModel(entry) {
  const count = Math.max(0, Math.floor(Number(entry?.harvestCount) || 0))
  if (count === 0) {
    return {
      unlocked: false,
      tier: 'locked',
      tierLabel: TIER_LABEL.locked,
      count: 0,
      nextTarget: 1,
      progressPct: 0,
      progressText: '首次收获后解锁',
      remainingText: '亲手收获一次即可点亮',
    }
  }
  let tier = 'wood'
  let next = CODEX_PLAQUE_STAGES[0]
  for (const stage of CODEX_PLAQUE_STAGES) {
    if (count < stage.target) {
      next = stage
      break
    }
    tier = stage.tier
    next = null
  }
  if (!next) {
    return {
      unlocked: true,
      tier: 'gold',
      tierLabel: TIER_LABEL.gold,
      count,
      nextTarget: 0,
      progressPct: 100,
      progressText: `累计收获 ${count} 次`,
      remainingText: '已达最高阶段',
    }
  }
  return {
    unlocked: true,
    tier,
    tierLabel: TIER_LABEL[tier],
    count,
    nextTarget: next.target,
    nextTierLabel: next.name,
    nextReward: next.reward,
    progressPct: Math.min(100, (count / next.target) * 100),
    progressText: `${next.name}进度 ${count} / ${next.target}`,
    remainingText: `距${next.name}还差 ${Math.max(0, next.target - count)} 次`,
  }
}
