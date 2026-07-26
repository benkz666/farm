<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { acceptInviteForSession } from '../net/authFlow.js'
import { errText } from '../net/errors.js'

const route = useRoute()
const router = useRouter()
const status = ref('loading')
const message = ref('正在确认好友邀请…')
let returnTimer = 0

const token = computed(() => String(route.params.token || ''))

async function acceptInvite() {
  try {
    const farm = window.__farm?.getNetClient
      ? window.__farm
      : await window.__farmReady
    const client = farm?.getNetClient?.()
    if (!client || !token.value) {
      throw new Error('invite: active session is unavailable')
    }

    await acceptInviteForSession(client, token.value)
    status.value = 'success'
    message.value = '已成为好友，正在返回农场'
    returnTimer = window.setTimeout(() => {
      router.replace({ name: 'farm' })
    }, 900)
  } catch (error) {
    status.value = 'error'
    const code = Number(error?.code)
    message.value = Number.isFinite(code)
      ? errText(code)
      : '暂时无法处理这份邀请'
  }
}

function goFarm() {
  router.replace({ name: 'farm' })
}

onMounted(acceptInvite)
onBeforeUnmount(() => window.clearTimeout(returnTimer))
</script>

<template>
  <main class="invite-page">
    <section class="invite-card" :data-status="status" aria-live="polite">
      <div class="invite-mark" aria-hidden="true">
        <span v-if="status === 'loading'">···</span>
        <span v-else-if="status === 'success'">✓</span>
        <span v-else>!</span>
      </div>
      <p class="eyebrow">经典农场 · 好友邀请</p>
      <h1>
        {{ status === 'error' ? '这份邀请没能落地' : '把田埂连在一起' }}
      </h1>
      <p class="status-copy">{{ message }}</p>
      <button v-if="status === 'error'" type="button" @click="goFarm">
        返回我的农场
      </button>
    </section>
  </main>
</template>

<style scoped>
.invite-page {
  --invite-sky: #b9e4ff;
  --invite-leaf: #2f7d4a;
  --invite-soil: #4a3325;
  --invite-harvest: #f5b83d;
  --invite-tomato: #e7653f;
  --invite-paper: #f7fbf2;
  position: fixed;
  inset: 0;
  z-index: 100;
  display: grid;
  place-items: center;
  overflow: hidden;
  padding: 24px;
  color: var(--invite-soil);
  background: var(--invite-sky);
  font-family: 'Noto Sans SC', sans-serif;
}

.invite-page::before,
.invite-page::after {
  position: absolute;
  right: -12vw;
  left: -12vw;
  border-radius: 50%;
  content: '';
}

.invite-page::before {
  bottom: -46vh;
  height: 72vh;
  background: var(--invite-leaf);
}

.invite-page::after {
  bottom: -54vh;
  height: 67vh;
  border: 16px solid rgba(74, 51, 37, 0.2);
  border-right-color: transparent;
  border-left-color: transparent;
}

.invite-card {
  position: relative;
  z-index: 1;
  width: min(480px, 100%);
  padding: 48px 40px 38px;
  border: 3px solid var(--invite-soil);
  border-radius: 30px;
  background: var(--invite-paper);
  box-shadow: 12px 13px 0 var(--invite-soil);
  text-align: center;
  animation: invite-arrive 480ms cubic-bezier(0.18, 0.9, 0.24, 1.1) both;
}

.invite-mark {
  display: grid;
  width: 78px;
  height: 78px;
  margin: -88px auto 24px;
  place-items: center;
  border: 3px solid var(--invite-soil);
  border-radius: 50%;
  color: var(--invite-soil);
  background: var(--invite-harvest);
  box-shadow: 4px 5px 0 var(--invite-soil);
  font-family: 'ZCOOL KuaiLe', cursive;
  font-size: 34px;
}

[data-status='loading'] .invite-mark span {
  animation: waiting 1.1s ease-in-out infinite;
}

[data-status='error'] .invite-mark {
  color: #fff;
  background: var(--invite-tomato);
}

.eyebrow {
  color: var(--invite-leaf);
  font-size: 11px;
  font-weight: 900;
  letter-spacing: 0.16em;
}

h1 {
  margin-top: 12px;
  font-family: 'ZCOOL KuaiLe', cursive;
  font-size: clamp(34px, 7vw, 52px);
  font-weight: 400;
  line-height: 1.15;
}

.status-copy {
  margin-top: 16px;
  color: rgba(74, 51, 37, 0.66);
  font-size: 14px;
  font-weight: 650;
}

button {
  min-height: 48px;
  margin-top: 26px;
  padding: 0 22px;
  border: 2px solid var(--invite-soil);
  border-radius: 12px;
  color: var(--invite-paper);
  background: var(--invite-leaf);
  box-shadow: 4px 5px 0 var(--invite-soil);
  font: 800 14px/1 'Noto Sans SC', sans-serif;
  cursor: pointer;
}

button:hover {
  box-shadow: 2px 3px 0 var(--invite-soil);
  transform: translate(2px, 2px);
}

button:focus-visible {
  outline: 4px solid rgba(245, 184, 61, 0.75);
  outline-offset: 4px;
}

@keyframes invite-arrive {
  from {
    opacity: 0;
    transform: translateY(34px) scale(0.96);
  }
}

@keyframes waiting {
  50% {
    opacity: 0.35;
    transform: translateY(-2px);
  }
}

@media (max-width: 520px) {
  .invite-card {
    padding: 44px 22px 30px;
    border-width: 2px;
    box-shadow: 7px 8px 0 var(--invite-soil);
  }
}

@media (prefers-reduced-motion: reduce) {
  .invite-card,
  [data-status='loading'] .invite-mark span {
    animation: none;
  }

  button {
    transition: none;
  }
}
</style>
