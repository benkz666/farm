import test from 'node:test'
import assert from 'node:assert/strict'

import { CROPS } from './config.js'
import { CROP_ICON_IDS, cropIconHTML } from './cropIcons.js'
import { badgeHTML } from './ui.js'

test('每一种作物都有独立的 SVG 插画', () => {
  assert.deepEqual(new Set(CROP_ICON_IDS), new Set(CROPS.map((crop) => crop.id)))
  for (const crop of CROPS) {
    const icon = cropIconHTML(crop)
    assert.match(icon, /<svg class="crop-art"/)
    assert.match(icon, new RegExp(`data-crop="${crop.id}"`))
  }
})

test('作物徽章使用 SVG，不再把名称首字作为可见图案', () => {
  for (const crop of CROPS) {
    const badge = badgeHTML(crop)
    assert.match(badge, /<svg class="crop-art"/)
    assert.doesNotMatch(badge, new RegExp(`>${crop.name[0]}<`))
  }
})
