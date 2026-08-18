<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'

import { session } from '../net/session.js'
import {
  LATENCY_SAMPLE_LIMIT,
  NET_PROBE_INTERVAL_MS,
  NET_PROBE_TIMEOUT_MS,
  NET_STATE_REFRESH_INTERVAL_MS,
  medianLatency,
  networkQuality,
  networkStatusLabel,
} from './netStatus.js'

const connectionState = ref('offline')
const latencyMs = ref(null)
const lastCheckedAt = ref(null)
const lastServerTime = ref(null)
const uid = ref(null)
const dump = ref('（等待游戏桥接）')
/** 默认收起，避免挡住农场操作；需要时再展开 */
const open = ref(false)
let refreshTimer = 0
let probeTimer = 0
let probeGeneration = 0
let probing = false
const latencySamples = []

const statusLabel = computed(() => networkStatusLabel(connectionState.value, latencyMs.value))

const statusTitle = computed(() => {
  if (connectionState.value === 'good') return `网络良好，服务器往返时延 ${latencyMs.value}ms`
  if (connectionState.value === 'weak') return `当前为弱网，服务器往返时延 ${latencyMs.value}ms`
  if (connectionState.value === 'checking') return '正在等待服务器 Ping 响应'
  if (connectionState.value === 'connecting') return '正在建立 WebSocket 连接'
  if (connectionState.value === 'reconnecting') return '连接已断开，正在自动重连'
  if (connectionState.value === 'restoring') return 'WebSocket 已重连，正在恢复农场状态'
  if (connectionState.value === 'unreachable') return 'WebSocket 已打开，但服务器 Ping 未正常响应'
  return '当前没有可用的服务器连接'
})

function clearLatency() {
  latencyMs.value = null
  latencySamples.length = 0
}

function recordLatency(value) {
  latencySamples.push(Math.max(0, Math.round(value)))
  if (latencySamples.length > LATENCY_SAMPLE_LIMIT) latencySamples.shift()
  latencyMs.value = medianLatency(latencySamples)
  connectionState.value = networkQuality(latencyMs.value)
}

function connectionSnapshot() {
  const farm = window.__farm
  const client = farm?.getNetClient?.() || null
  const sessionOnline = session.isOnline === true
  return {
    farm,
    client,
    sessionOnline,
    transport: client?.connectionState || 'offline',
  }
}

function refreshDump(snapshot = connectionSnapshot()) {
  if (!open.value) return
  const { farm, client, sessionOnline, transport } = snapshot
  if (!farm?.getState) {
    dump.value = 'game bridge 未就绪'
    return
  }
  const state = farm.getState()
  dump.value = JSON.stringify(
    {
      online: connectionState.value === 'good' || connectionState.value === 'weak',
      quality: connectionState.value,
      sessionOnline,
      transport,
      latencyMs: latencyMs.value,
      lastCheckedAt: lastCheckedAt.value == null
        ? null
        : new Date(lastCheckedAt.value).toISOString(),
      serverTime: lastServerTime.value,
      uid: session.uid,
      gold: state.gold,
      exp: state.exp,
      unlockedPlots: state.unlockedPlots,
      friends: Array.isArray(state.friends) ? state.friends.length : 0,
      net: Boolean(client),
    },
    null,
    2,
  )
}

function refresh() {
  uid.value = session.uid
  const snapshot = connectionSnapshot()
  if (!snapshot.sessionOnline || !snapshot.client) {
    connectionState.value = 'offline'
    clearLatency()
  } else if (snapshot.transport !== 'connected') {
    connectionState.value = snapshot.transport
    clearLatency()
  } else if (
    !probing &&
    !['good', 'weak', 'checking', 'unreachable'].includes(connectionState.value)
  ) {
    connectionState.value = 'checking'
    void probe()
  }
  refreshDump(snapshot)
}

async function probe() {
  if (probing) return
  const snapshot = connectionSnapshot()
  if (!snapshot.sessionOnline || !snapshot.client || snapshot.transport !== 'connected') {
    refresh()
    return
  }

  probing = true
  const generation = ++probeGeneration
  const startedAt = performance.now()
  // 稳定在线或已判定断网时保留现有展示，避免每次 Ping 都闪成“检测中”。
  if (!['good', 'weak', 'unreachable'].includes(connectionState.value)) {
    connectionState.value = 'checking'
  }
  try {
    const response = await snapshot.client.ping(Date.now(), NET_PROBE_TIMEOUT_MS)
    if (generation !== probeGeneration) return
    const current = connectionSnapshot()
    if (
      current.client !== snapshot.client ||
      !current.sessionOnline ||
      current.transport !== 'connected'
    ) {
      connectionState.value = current.sessionOnline ? current.transport : 'offline'
      clearLatency()
      return
    }
    if (!response || response.err !== 0) {
      connectionState.value = 'unreachable'
      clearLatency()
      return
    }
    recordLatency(performance.now() - startedAt)
    lastCheckedAt.value = Date.now()
    lastServerTime.value = response.payload?.server_time ?? null
  } catch {
    if (generation !== probeGeneration) return
    const current = connectionSnapshot()
    connectionState.value = current.sessionOnline && current.transport === 'connected'
      ? 'unreachable'
      : (current.sessionOnline ? current.transport : 'offline')
    clearLatency()
  } finally {
    if (generation === probeGeneration) {
      probing = false
      refreshDump()
    }
  }
}

function toggle() {
  open.value = !open.value
  refresh()
}

onMounted(() => {
  refresh()
  void probe()
  refreshTimer = window.setInterval(refresh, NET_STATE_REFRESH_INTERVAL_MS)
  probeTimer = window.setInterval(() => { void probe() }, NET_PROBE_INTERVAL_MS)
})

onUnmounted(() => {
  probeGeneration++
  if (refreshTimer) window.clearInterval(refreshTimer)
  if (probeTimer) window.clearInterval(probeTimer)
})
</script>

<template>
  <aside class="dev-net-panel" :class="{ 'dev-net-panel--collapsed': !open }" aria-label="开发诊断面板">
    <button
      type="button"
      class="dev-net-panel__toggle"
      :aria-expanded="open"
      :title="open ? '收起 Net 诊断' : '展开 Net 诊断'"
      @click="toggle"
    >
      {{ open ? '▾ Net' : 'Net' }}
      <span
        class="dev-net-panel__status"
        :class="`dev-net-panel__status--${connectionState}`"
        :title="statusTitle"
      >{{ statusLabel }}</span>
    </button>
    <template v-if="open">
      <header class="dev-net-panel__head">
        <strong>Net 诊断</strong>
      </header>
      <p class="dev-net-panel__hint">
        仅 DEV 可见。状态由真实 WebSocket 生命周期和服务端 Ping 响应判定。
      </p>
      <p class="dev-net-panel__meta">uid: {{ uid ?? '—' }}</p>
      <pre class="dev-net-panel__out">{{ dump }}</pre>
    </template>
  </aside>
</template>

<style scoped>
.dev-net-panel {
  position: fixed;
  right: 12px;
  bottom: 12px;
  z-index: 1000;
  width: 280px;
  box-sizing: border-box;
  max-height: min(40vh, 320px);
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border-radius: 10px;
  background: rgba(20, 28, 24, 0.88);
  color: #e8f0ea;
  font: 12px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
  pointer-events: auto;
}

.dev-net-panel--collapsed {
  width: 112px;
  max-height: none;
  padding: 6px 8px;
}

.dev-net-panel__toggle {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  align-items: center;
  gap: 6px;
  width: 100%;
  margin: 0;
  padding: 0;
  border: 0;
  background: transparent;
  color: inherit;
  font: inherit;
  cursor: pointer;
  text-align: left;
}

.dev-net-panel__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.dev-net-panel__status {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  justify-self: end;
  box-sizing: border-box;
  width: 68px;
  min-width: 68px;
  padding: 2px 6px;
  border-radius: 999px;
  background: rgba(190, 92, 78, 0.25);
  color: #ffb2a7;
  font-size: 11px;
  font-variant-numeric: tabular-nums;
  font-feature-settings: 'tnum';
  white-space: nowrap;
  overflow: hidden;
}

.dev-net-panel__status--good {
  background: rgba(120, 180, 130, 0.25);
  color: #b6e0bf;
}

.dev-net-panel__status--weak {
  background: rgba(230, 178, 72, 0.28);
  color: #ffe09b;
}

.dev-net-panel__status--checking,
.dev-net-panel__status--connecting {
  background: rgba(92, 153, 205, 0.25);
  color: #b9dcf8;
}

.dev-net-panel__status--restoring {
  background: rgba(139, 112, 205, 0.28);
  color: #dbcaff;
}

.dev-net-panel__status--reconnecting {
  background: rgba(218, 159, 72, 0.25);
  color: #ffe0a1;
}

.dev-net-panel__status--offline,
.dev-net-panel__status--unreachable {
  background: rgba(190, 92, 78, 0.3);
  color: #ffb2a7;
}

.dev-net-panel__hint {
  margin: 0;
  color: rgba(232, 240, 234, 0.72);
  font-size: 11px;
  line-height: 1.45;
}

.dev-net-panel__hint code {
  font: inherit;
  color: #9be15d;
}

.dev-net-panel__meta {
  margin: 0;
  color: rgba(232, 240, 234, 0.85);
}

.dev-net-panel__out {
  margin: 0;
  padding: 8px;
  overflow: auto;
  border-radius: 6px;
  background: rgba(0, 0, 0, 0.28);
  white-space: pre-wrap;
  word-break: break-word;
  flex: 1;
  min-height: 0;
}
</style>
