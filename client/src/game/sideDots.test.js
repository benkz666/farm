import test from 'node:test'
import assert from 'node:assert/strict'

import { mailDotVisible, taskDotVisible } from './sideDots.js'

test('邮箱空列表不显示红点', () => {
  assert.equal(mailDotVisible([]), false)
  assert.equal(mailDotVisible(null), false)
})

test('无可领附件的已读邮件不显示红点', () => {
  assert.equal(mailDotVisible([
    { read: true, claimed: true, gold: 100, attachmentCoin: 100 },
    { read: true, claimed: false, gold: 0, attachmentCoin: 0 },
  ]), false)
})

test('有未领取金币附件时显示红点', () => {
  assert.equal(mailDotVisible([
    { read: true, claimed: false, gold: 50, attachmentCoin: 50 },
  ]), true)
})

test('纯提示未读且无附件不显示红点（避免本地假邮件误报）', () => {
  assert.equal(mailDotVisible([
    { read: false, claimed: false, gold: 0, attachmentCoin: 0, title: '新的一天' },
  ]), false)
})

test('同意/拒绝类通知邮件无附件不点亮侧栏红点', () => {
  assert.equal(mailDotVisible([
    { read: true, claimed: false, gold: 0, title: 'lxy 同意了你的邻里申请' },
    { read: true, claimed: false, gold: 0, title: 'lxy 拒绝了你的邻里申请' },
  ], []), false)
})

test('有待处理邻里申请时显示红点', () => {
  assert.equal(mailDotVisible([], [{ from_uid: 1, nickname: 'a' }]), true)
  assert.equal(mailDotVisible(
    [{ read: true, claimed: true, gold: 0 }],
    [{ from_uid: 2 }],
  ), true)
})

test('任务未完成不显示红点', () => {
  assert.equal(taskDotVisible([
    { progress: 0, target: 1, done: false, claimed: false },
  ]), false)
})

test('任务已完成未领取显示红点', () => {
  assert.equal(taskDotVisible([
    { progress: 1, target: 1, done: true, claimed: false },
  ]), true)
})

test('任务已领取不显示红点', () => {
  assert.equal(taskDotVisible([
    { progress: 1, target: 1, done: true, claimed: true },
  ]), false)
})

test('每日登录 task_id=4 完成未领取时显示红点，领取后熄灭', () => {
  assert.equal(taskDotVisible([
    { id: 4, progress: 1, target: 1, done: true, claimed: false },
  ]), true)
  assert.equal(taskDotVisible([
    { id: 4, progress: 1, target: 1, done: true, claimed: true },
  ]), false)
})
