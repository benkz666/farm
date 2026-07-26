import test from 'node:test'
import assert from 'node:assert/strict'

import { shouldApplyPatchFromError } from './onlineResponse.js'

test('仅对带补丁的误铲错误应用服务端变更', () => {
  assert.equal(shouldApplyPatchFromError(1204, { patch: { coin: 1000 } }), true)
})

test('其他错误或缺失补丁不应用服务端变更', () => {
  assert.equal(shouldApplyPatchFromError(1204, {}), false)
  assert.equal(shouldApplyPatchFromError(1203, { patch: { coin: 1000 } }), false)
})
