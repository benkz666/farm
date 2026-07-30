import test from 'node:test'
import assert from 'node:assert/strict'

import { parseJSONSafe, wireUid } from './jsonSafe.js'

test('parseJSONSafe 保留 19 位雪花为字符串', () => {
  const raw = '{"from_uid":1785142595526523238,"nickname":"zbk"}'
  const got = parseJSONSafe(raw)
  assert.equal(got.from_uid, '1785142595526523238')
  assert.equal(got.nickname, 'zbk')
})

test('parseJSONSafe 不改写安全整数', () => {
  const got = parseJSONSafe('{"uid":42,"n":9007199254740991}')
  assert.equal(got.uid, 42)
  assert.equal(got.n, 9007199254740991)
})

test('wireUid 保留数字字符串', () => {
  assert.equal(wireUid('1785142595526523238'), '1785142595526523238')
  assert.equal(wireUid(42), 42)
  assert.equal(wireUid(null), null)
})
