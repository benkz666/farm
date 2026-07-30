<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'

import { session } from '../net/session.js'

const online = ref(false)
const uid = ref(null)
const dump = ref('（等待游戏桥接）')
/** 默认收起，避免挡住农场操作；需要时再展开 */
const open = ref(false)
let timer = 0

const statusLabel = computed(() => (online.value ? 'online' : 'offline'))

function refresh() {
  online.value = session.isOnline === true
  uid.value = session.uid
  if (!open.value) return
  const farm = window.__farm
  if (!farm?.getState) {
    dump.value = 'game bridge 未就绪'
    return
  }
  const state = farm.getState()
  dump.value = JSON.stringify(
    {
      online: farm.isOnline?.() ?? online.value,
      uid: session.uid,
      gold: state.gold,
      exp: state.exp,
      unlockedPlots: state.unlockedPlots,
      friends: Array.isArray(state.friends) ? state.friends.length : 0,
      net: Boolean(farm.getNetClient?.()),
    },
    null,
    2,
  )
}

function toggle() {
  open.value = !open.value
  refresh()
}

onMounted(() => {
  refresh()
  timer = window.setInterval(refresh, 1000)
})

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
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
      <span class="dev-net-panel__status">{{ statusLabel }}</span>
    </button>
    <template v-if="open">
      <header class="dev-net-panel__head">
        <strong>Net 诊断</strong>
      </header>
      <p class="dev-net-panel__hint">
        仅 DEV 可见。登录入口在 <code>/login</code>；此处不提供注册/进房。
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
  width: auto;
  max-height: none;
  padding: 6px 8px;
}

.dev-net-panel__toggle {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
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
  padding: 2px 8px;
  border-radius: 999px;
  background: rgba(120, 180, 130, 0.25);
  color: #b6e0bf;
  font-size: 11px;
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
