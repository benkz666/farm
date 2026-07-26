import test from 'node:test'
import assert from 'node:assert/strict'

import { errText } from './errors.js'

test('社交与邀请错误码提供可操作中文文案', () => {
  const expected = new Map([
    [1402, '你们已经是好友了'],
    [1403, '不能添加自己为好友'],
    [1404, '你的好友数量已达上限'],
    [1405, '对方的好友数量已达上限'],
    [1406, '邀请链接无效'],
    [1407, '邀请链接已过期'],
  ])

  for (const [code, text] of expected) {
    assert.equal(errText(code), text)
  }
})
