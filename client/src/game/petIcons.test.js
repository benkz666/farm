import test from 'node:test'
import assert from 'node:assert/strict'

import { DOGS } from './config.js'
import { PET_ICON_IDS, petBadgeHTML, petIconHTML } from './petIcons.js'

test('每一种看家狗都有独立的 SVG 插画', () => {
  assert.deepEqual(new Set(PET_ICON_IDS), new Set(DOGS.map((dog) => dog.id)))
  for (const dog of DOGS) {
    const icon = petIconHTML(dog)
    assert.match(icon, /<svg class="pet-art"/)
    assert.match(icon, new RegExp(`data-pet="${dog.id}"`))
  }
})

test('宠物徽章使用 SVG 并支持顶部小尺寸状态入口', () => {
  const badge = petBadgeHTML(DOGS[0])
  const chip = petBadgeHTML(DOGS[0], 'chip')
  assert.match(badge, /class="pet-badge pet-badge--md"/)
  assert.match(chip, /class="pet-badge pet-badge--chip"/)
  assert.match(chip, /<svg class="pet-art"/)
})
