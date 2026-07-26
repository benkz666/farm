import test from 'node:test'
import assert from 'node:assert/strict'

import { routeRedirect } from './routeAccess.js'

test('未登录访问农场时转到登录页', () => {
  assert.deepEqual(
    routeRedirect({ name: 'farm', meta: { requiresAuth: true } }, false),
    { name: 'login' },
  )
})

test('未登录邀请落地时把 token 带到登录页', () => {
  assert.deepEqual(
    routeRedirect({ name: 'invite', params: { token: 'abc.def' }, meta: {} }, false),
    { name: 'login', query: { invite: 'abc.def' } },
  )
})

test('已登录打开带邀请参数的登录页时继续邀请落地', () => {
  assert.deepEqual(
    routeRedirect({ name: 'login', query: { invite: 'abc.def' }, meta: {} }, true),
    { name: 'invite', params: { token: 'abc.def' } },
  )
})

test('已登录访问普通登录页时返回农场', () => {
  assert.deepEqual(
    routeRedirect({ name: 'login', query: {}, meta: {} }, true),
    { name: 'farm' },
  )
})

test('已登录访问农场时不重定向', () => {
  assert.equal(
    routeRedirect({ name: 'farm', meta: { requiresAuth: true } }, true),
    null,
  )
})
