import test from 'node:test'
import assert from 'node:assert/strict'

import {
  GOOD_NETWORK_MAX_LATENCY_MS,
  NET_PROBE_INTERVAL_MS,
  NET_PROBE_TIMEOUT_MS,
  NET_STATE_REFRESH_INTERVAL_MS,
  medianLatency,
  networkQuality,
  networkStatusLabel,
} from './netStatus.js'

test('网络状态高频刷新且每秒执行一次真实 Ping', () => {
  assert.equal(NET_STATE_REFRESH_INTERVAL_MS, 250)
  assert.equal(NET_PROBE_INTERVAL_MS, 1_000)
  assert.equal(NET_PROBE_TIMEOUT_MS, 1_500)
})

test('网络质量以 200ms 为良好与弱网分界', () => {
  assert.equal(networkQuality(GOOD_NETWORK_MAX_LATENCY_MS), 'good')
  assert.equal(networkQuality(GOOD_NETWORK_MAX_LATENCY_MS + 1), 'weak')
  assert.equal(networkQuality(null), 'checking')
})

test('在线状态只显示时延，连接生命周期显示中文状态', () => {
  assert.equal(networkStatusLabel('good', 38.4), '38ms')
  assert.equal(networkStatusLabel('weak', 615), '615ms')
  assert.equal(networkStatusLabel('reconnecting', null), '重连中')
  assert.equal(networkStatusLabel('restoring', null), '同步中')
  assert.equal(networkStatusLabel('unreachable', null), '断网')
})

test('短 RTT 样本取中位数抑制单次尖峰', () => {
  assert.equal(medianLatency([42, 610, 45]), 45)
  assert.equal(medianLatency([42, 44]), 44)
  assert.equal(medianLatency([]), null)
})
