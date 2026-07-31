import test from 'node:test'
import assert from 'node:assert/strict'

import {
  compareUint64,
  isNextUint64,
  parseJSONSafe,
  sameUid,
  sameUint64,
  wireUid,
  wireUint64,
} from './jsonSafe.js'

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

test('parseJSONSafe 把碰巧可表示但不安全的 2^53 也保留为字符串', () => {
  const got = parseJSONSafe(
    '{"a":9007199254740992,"b":9007199254740993,"negative":-9007199254740992,"items":[9007199254740992]}',
  )
  assert.equal(got.a, '9007199254740992')
  assert.equal(got.b, '9007199254740993')
  assert.equal(got.negative, '-9007199254740992')
  assert.equal(got.items[0], '9007199254740992')
})

test('parseJSONSafe 不改写字符串内容、浮点数或指数形式', () => {
  const got = parseJSONSafe(
    '{"text":"inside: 9007199254740993","decimal":9007199254740992.5,"exponent":1e20}',
  )
  assert.equal(got.text, 'inside: 9007199254740993')
  assert.equal(got.decimal, 9007199254740992)
  assert.equal(got.exponent, 1e20)
})

test('parseJSONSafe 不会把带前导零的非法 JSON 修复成合法字符串', () => {
  assert.throws(() => parseJSONSafe('{"id":09007199254740993}'), SyntaxError)
})

test('wireUid 保留数字字符串', () => {
  assert.equal(wireUid('1785142595526523238'), '1785142595526523238')
  assert.equal(wireUid(42), 42)
  assert.equal(wireUid(null), null)
})

test('sameUid 精确比较 19 位 UID，不经过 Number 舍入', () => {
  assert.equal(sameUid('1785402171458126005', '1785402171458126005'), true)
  assert.equal(sameUid('1785402171458126005', '1785402171458126006'), false)
})

test('wireUid 拒绝已经丢失精度的 Number', () => {
  assert.equal(wireUid(1785402171458126005), null)
})

test('uint64 工具跨越 2^53 后仍能精确比较相邻序列', () => {
  const current = '9007199254740992'
  const next = '9007199254740993'
  assert.equal(wireUint64(next), next)
  assert.equal(compareUint64(current, next), -1)
  assert.equal(sameUint64(current, next), false)
  assert.equal(isNextUint64(next, current), true)
  assert.equal(wireUint64('18446744073709551616'), null)
})
