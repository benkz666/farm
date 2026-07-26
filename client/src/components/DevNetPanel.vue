<script setup>
import { ref } from 'vue'
import { NetClient } from '../net/client.js'

const client = new NetClient()

const username = ref(`dev${Date.now().toString(36).slice(-6)}`)
const password = ref('secret12')
const busy = ref(false)
const resultJson = ref('（尚未操作）')
const status = ref('idle')

function show(label, value) {
  status.value = label
  resultJson.value = typeof value === 'string' ? value : JSON.stringify(value, null, 2)
}

async function run(label, fn) {
  if (busy.value) return
  busy.value = true
  try {
    const value = await fn()
    show(label, value)
  } catch (err) {
    show(`${label} ERROR`, err instanceof Error ? err.message : String(err))
  } finally {
    busy.value = false
  }
}

function onRegister() {
  return run('register', () => client.register(username.value.trim(), password.value))
}

function onLogin() {
  return run('login', () => client.login(username.value.trim(), password.value))
}

/** 登录（或沿用已有 token）→ connect → handshake → enterFarm(0) → 切入 online */
function onFetchSnapshot() {
  return run('enterFarm', async () => {
    if (!client.token) {
      await client.login(username.value.trim(), password.value)
    }
    await client.connect()
    const hs = await client.handshake()
    if (hs.err !== 0) {
      throw new Error(`handshake err=${hs.err}`)
    }
    const enter = await client.enterFarm(0)
    if (enter.err !== 0) {
      throw new Error(`enterFarm err=${enter.err}`)
    }
    const farm = window.__farm
    if (!farm?.enterOnlineFromNet) {
      throw new Error('game main not ready (__farm.enterOnlineFromNet)')
    }
    farm.enterOnlineFromNet(client, enter)
    return { err: enter.err, online: true, uid: client.uid, payload: enter.payload }
  })
}
</script>

<template>
  <aside class="dev-net-panel" aria-label="网络联调面板">
    <header class="dev-net-panel__head">
      <strong>Net 联调</strong>
      <span class="dev-net-panel__status">{{ status }}</span>
    </header>
    <label>
      用户名
      <input v-model="username" autocomplete="username" :disabled="busy" />
    </label>
    <label>
      密码
      <input v-model="password" type="password" autocomplete="current-password" :disabled="busy" />
    </label>
    <div class="dev-net-panel__actions">
      <button type="button" :disabled="busy" @click="onRegister">注册</button>
      <button type="button" :disabled="busy" @click="onLogin">登录</button>
      <button type="button" :disabled="busy" @click="onFetchSnapshot">拉快照</button>
    </div>
    <pre class="dev-net-panel__out">{{ resultJson }}</pre>
  </aside>
</template>

<style scoped>
.dev-net-panel {
  position: fixed;
  right: 12px;
  bottom: 12px;
  z-index: 1000;
  width: 320px;
  max-height: min(48vh, 420px);
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border-radius: 10px;
  background: rgba(20, 28, 24, 0.88);
  color: #e8f0e9;
  font: 12px/1.35 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
  pointer-events: auto;
  user-select: text;
  -webkit-user-select: text;
}

.dev-net-panel__head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.dev-net-panel__status {
  color: #9ec9a4;
  font-size: 11px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 60%;
}

.dev-net-panel label {
  display: flex;
  flex-direction: column;
  gap: 2px;
  color: #b7c8ba;
}

.dev-net-panel input {
  padding: 4px 6px;
  border: 1px solid #3a4a3e;
  border-radius: 4px;
  background: #121a16;
  color: #e8f0e9;
}

.dev-net-panel__actions {
  display: flex;
  gap: 6px;
}

.dev-net-panel__actions button {
  flex: 1;
  padding: 5px 0;
  border: 1px solid #4a6b52;
  border-radius: 4px;
  background: #2a4032;
  color: #e8f0e9;
  cursor: pointer;
}

.dev-net-panel__actions button:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.dev-net-panel__actions button:not(:disabled):hover {
  background: #355240;
}

.dev-net-panel__out {
  margin: 0;
  padding: 6px;
  overflow: auto;
  flex: 1;
  min-height: 80px;
  max-height: 220px;
  border-radius: 4px;
  background: #0c1210;
  white-space: pre-wrap;
  word-break: break-word;
}
</style>
