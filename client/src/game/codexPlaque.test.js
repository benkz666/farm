import assert from 'node:assert/strict'
import test from 'node:test'

import { codexPlaqueViewModel } from './codexPlaque.js'

test('codex plaque follows unlock, bronze, silver and gold harvest thresholds', () => {
  const cases = [
    [0, 'locked', 1, 0],
    [1, 'wood', 10, 10],
    [9, 'wood', 10, 90],
    [10, 'bronze', 20, 50],
    [19, 'bronze', 20, 95],
    [20, 'silver', 50, 40],
    [49, 'silver', 50, 98],
    [50, 'gold', 0, 100],
  ]

  for (const [count, tier, nextTarget, progressPct] of cases) {
    const got = codexPlaqueViewModel({ harvestCount: count })
    assert.equal(got.tier, tier, `count=${count}`)
    assert.equal(got.nextTarget, nextTarget, `count=${count}`)
    assert.equal(got.progressPct, progressPct, `count=${count}`)
  }
})

test('gold plaque keeps showing the total successful harvest actions', () => {
  const got = codexPlaqueViewModel({ harvestCount: 73 })
  assert.equal(got.progressText, '累计收获 73 次')
  assert.equal(got.remainingText, '已达最高阶段')
  assert.equal(got.nextReward, undefined)
})
