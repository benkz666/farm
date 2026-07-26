<script setup>
import { computed, onBeforeUnmount, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { authenticateAndEnter } from '../net/authFlow.js'
import { NetClient } from '../net/client.js'
import { errText } from '../net/errors.js'

const route = useRoute()
const router = useRouter()
const client = new NetClient()

const mode = ref('login')
const username = ref('')
const password = ref('')
const busy = ref(false)
const message = ref('')
const handedOff = ref(false)

const inviteToken = computed(() => {
  const raw = route.query.invite
  return Array.isArray(raw) ? String(raw[0] || '') : String(raw || '')
})
const isRegister = computed(() => mode.value === 'register')
const submitText = computed(() => {
  if (busy.value) return isRegister.value ? '正在开垦账号…' : '正在回到农场…'
  return isRegister.value ? '注册并进入农场' : '进入我的农场'
})

function selectMode(nextMode) {
  if (busy.value || mode.value === nextMode) return
  mode.value = nextMode
  message.value = ''
}

async function getFarmBridge() {
  if (window.__farm?.enterOnlineFromNet) return window.__farm
  if (window.__farmReady) return window.__farmReady
  throw new Error('game: farm bridge is not ready')
}

async function submit() {
  if (busy.value) return
  const cleanUsername = username.value.trim()
  if (!cleanUsername || !password.value) {
    message.value = errText(1002)
    return
  }

  busy.value = true
  message.value = ''
  try {
    await authenticateAndEnter({
      client,
      mode: mode.value,
      username: cleanUsername,
      password: password.value,
      inviteToken: inviteToken.value,
      getFarmBridge,
    })
    handedOff.value = true
    await router.replace({ name: 'farm' })
  } catch (error) {
    client.close()
    const code = Number(error?.code)
    message.value = Number.isFinite(code)
      ? errText(code)
      : '暂时无法连接农场，请检查网络后重试'
  } finally {
    busy.value = false
  }
}

onBeforeUnmount(() => {
  if (!handedOff.value) client.close()
})
</script>

<template>
  <main class="auth-page">
    <div class="sun" aria-hidden="true">
      <span></span>
    </div>

    <section class="brand-column" aria-labelledby="farm-title">
      <p class="brand-kicker">老朋友，新一季</p>
      <h1 id="farm-title">经典农场</h1>
      <p class="brand-copy">锄一块地，等一场成熟。登录后继续照看你的四季。</p>

      <div class="field-signature" aria-hidden="true">
        <div class="furrow furrow-one"></div>
        <div class="furrow furrow-two"></div>
        <div class="furrow furrow-three"></div>
        <div class="seedling seedling-one"><i></i><b></b></div>
        <div class="seedling seedling-two"><i></i><b></b></div>
        <div class="seedling seedling-three"><i></i><b></b></div>
      </div>
    </section>

    <section class="auth-card" aria-labelledby="form-title">
      <div class="card-stitches" aria-hidden="true"></div>

      <p v-if="inviteToken" class="invite-note">
        <span>好友邀请</span>
        登录后会自动把对方加入好友
      </p>

      <div class="mode-switch" aria-label="选择登录或注册">
        <button
          type="button"
          :class="{ active: mode === 'login' }"
          :aria-pressed="mode === 'login'"
          :disabled="busy"
          @click="selectMode('login')"
        >
          登录
        </button>
        <button
          type="button"
          :class="{ active: mode === 'register' }"
          :aria-pressed="mode === 'register'"
          :disabled="busy"
          @click="selectMode('register')"
        >
          注册
        </button>
      </div>

      <div class="form-heading">
        <span class="plot-number">田埂 01</span>
        <h2 id="form-title">{{ isRegister ? '领一块新农场' : '回到你的农场' }}</h2>
        <p>{{ isRegister ? '取个名字，第一袋种子已经备好。' : '作物还在生长，回来看看吧。' }}</p>
      </div>

      <form @submit.prevent="submit">
        <label>
          <span>用户名</span>
          <input
            v-model="username"
            name="username"
            autocomplete="username"
            inputmode="text"
            placeholder="输入农场主名字"
            :disabled="busy"
            autofocus
          />
        </label>

        <label>
          <span>密码</span>
          <input
            v-model="password"
            name="password"
            type="password"
            :autocomplete="isRegister ? 'new-password' : 'current-password'"
            placeholder="输入你的密码"
            :disabled="busy"
          />
        </label>

        <p v-if="message" class="form-message" role="alert">
          <span aria-hidden="true">!</span>
          {{ message }}
        </p>

        <button class="submit-button" type="submit" :disabled="busy">
          <span>{{ submitText }}</span>
          <i aria-hidden="true">→</i>
        </button>
      </form>

      <p class="form-footnote">登录即会连接在线农场，进度以服务器为准。</p>
    </section>
  </main>
</template>

<style scoped>
.auth-page {
  --auth-sky: #b9e4ff;
  --auth-leaf: #2f7d4a;
  --auth-soil: #4a3325;
  --auth-harvest: #f5b83d;
  --auth-tomato: #e7653f;
  --auth-paper: #f7fbf2;
  position: fixed;
  inset: 0;
  z-index: 100;
  display: grid;
  grid-template-columns: minmax(0, 1.15fr) minmax(360px, 500px);
  align-items: center;
  gap: clamp(36px, 7vw, 112px);
  overflow: hidden;
  padding: clamp(28px, 5.5vw, 88px);
  color: var(--auth-soil);
  background: var(--auth-sky);
  font-family: 'Noto Sans SC', sans-serif;
  user-select: text;
  -webkit-user-select: text;
}

.auth-page::before {
  position: absolute;
  right: -12vw;
  bottom: -37vh;
  left: -15vw;
  height: 72vh;
  border-radius: 50% 48% 0 0;
  background: var(--auth-leaf);
  content: '';
  transform: rotate(-2deg);
}

.auth-page::after {
  position: absolute;
  right: -8vw;
  bottom: -49vh;
  left: -8vw;
  height: 68vh;
  border: 18px solid rgba(74, 51, 37, 0.18);
  border-right-color: transparent;
  border-left-color: transparent;
  border-radius: 50%;
  content: '';
}

.sun {
  position: absolute;
  top: clamp(22px, 6vh, 72px);
  right: clamp(18px, 7vw, 112px);
  width: clamp(82px, 10vw, 142px);
  aspect-ratio: 1;
  border-radius: 50%;
  background: var(--auth-harvest);
  box-shadow: 0 0 0 18px rgba(245, 184, 61, 0.17);
  animation: sun-rise 1.1s cubic-bezier(0.2, 0.8, 0.2, 1) both;
}

.sun span {
  position: absolute;
  top: 27%;
  left: 25%;
  width: 18%;
  aspect-ratio: 1;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.55);
}

.brand-column,
.auth-card {
  position: relative;
  z-index: 2;
}

.brand-column {
  align-self: stretch;
  display: flex;
  flex-direction: column;
  justify-content: center;
  min-height: 520px;
  animation: brand-arrive 0.75s 0.08s cubic-bezier(0.2, 0.85, 0.25, 1) both;
}

.brand-kicker {
  width: max-content;
  margin-bottom: 14px;
  padding: 7px 13px;
  border: 2px solid var(--auth-soil);
  border-radius: 999px;
  background: var(--auth-harvest);
  font-size: 13px;
  font-weight: 800;
  letter-spacing: 0.18em;
}

.brand-column h1 {
  max-width: 750px;
  color: var(--auth-soil);
  font-family: 'ZCOOL KuaiLe', cursive;
  font-size: clamp(74px, 10vw, 154px);
  font-weight: 400;
  line-height: 0.92;
  letter-spacing: 0.02em;
  text-shadow: 7px 8px 0 rgba(247, 251, 242, 0.75);
}

.brand-copy {
  max-width: 420px;
  margin-top: 24px;
  font-size: clamp(16px, 1.5vw, 21px);
  font-weight: 650;
  line-height: 1.8;
}

.field-signature {
  position: absolute;
  right: -8vw;
  bottom: -10vh;
  left: -8vw;
  height: 32vh;
  min-height: 190px;
  pointer-events: none;
}

.furrow {
  position: absolute;
  left: 0;
  width: 115%;
  height: 74%;
  border: clamp(7px, 0.8vw, 13px) solid rgba(74, 51, 37, 0.28);
  border-right-color: transparent;
  border-left-color: transparent;
  border-radius: 50%;
}

.furrow-one {
  bottom: -35%;
  transform: rotate(2deg);
}

.furrow-two {
  bottom: -13%;
  left: 5%;
  transform: rotate(-1deg);
}

.furrow-three {
  bottom: 8%;
  left: 10%;
  transform: rotate(1deg);
}

.seedling {
  position: absolute;
  bottom: 38%;
  width: 9px;
  height: 54px;
  border-radius: 9px;
  background: var(--auth-soil);
  transform-origin: bottom center;
  animation: seedling-sway 2.8s ease-in-out infinite;
}

.seedling i,
.seedling b {
  position: absolute;
  top: 8px;
  width: 34px;
  height: 20px;
  background: var(--auth-paper);
}

.seedling i {
  right: 5px;
  border-radius: 90% 8% 90% 12%;
  transform: rotate(19deg);
}

.seedling b {
  left: 5px;
  border-radius: 8% 90% 12% 90%;
  transform: rotate(-19deg);
}

.seedling-one {
  left: 18%;
  animation-delay: -0.4s;
}

.seedling-two {
  left: 47%;
  bottom: 29%;
  transform: scale(0.8);
  animation-delay: -1.2s;
}

.seedling-three {
  left: 75%;
  bottom: 45%;
  transform: scale(1.12);
  animation-delay: -2s;
}

.auth-card {
  width: 100%;
  max-width: 500px;
  justify-self: end;
  padding: clamp(28px, 3.4vw, 48px);
  border: 3px solid var(--auth-soil);
  border-radius: 30px 30px 22px 22px;
  background: var(--auth-paper);
  box-shadow: 13px 14px 0 var(--auth-soil);
  animation: card-plant 0.72s 0.18s cubic-bezier(0.18, 0.9, 0.24, 1.15) both;
}

.card-stitches {
  position: absolute;
  top: 13px;
  right: 22px;
  left: 22px;
  height: 5px;
  background: repeating-linear-gradient(
    90deg,
    var(--auth-soil) 0 9px,
    transparent 9px 18px
  );
  opacity: 0.23;
}

.invite-note {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 1px 0 18px;
  padding: 10px 12px;
  border: 1px solid rgba(47, 125, 74, 0.3);
  border-radius: 12px;
  color: var(--auth-leaf);
  background: rgba(47, 125, 74, 0.08);
  font-size: 12px;
  font-weight: 700;
}

.invite-note span {
  flex: none;
  padding: 3px 7px;
  border-radius: 7px;
  color: var(--auth-paper);
  background: var(--auth-leaf);
}

.mode-switch {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 5px;
  padding: 5px;
  border: 2px solid rgba(74, 51, 37, 0.14);
  border-radius: 14px;
  background: rgba(74, 51, 37, 0.06);
}

.mode-switch button {
  min-height: 42px;
  border: 0;
  border-radius: 9px;
  color: rgba(74, 51, 37, 0.58);
  background: transparent;
  font: inherit;
  font-weight: 800;
  cursor: pointer;
  transition: color 160ms ease, background 160ms ease, transform 160ms ease;
}

.mode-switch button.active {
  color: var(--auth-paper);
  background: var(--auth-leaf);
  transform: translateY(-1px);
}

.form-heading {
  margin: 30px 0 24px;
}

.plot-number {
  color: var(--auth-tomato);
  font-size: 11px;
  font-weight: 900;
  letter-spacing: 0.2em;
  text-transform: uppercase;
}

.form-heading h2 {
  margin-top: 8px;
  font-size: clamp(26px, 2.4vw, 36px);
  font-weight: 900;
  line-height: 1.2;
}

.form-heading p {
  margin-top: 8px;
  color: rgba(74, 51, 37, 0.66);
  font-size: 14px;
  font-weight: 550;
}

form {
  display: grid;
  gap: 17px;
}

label {
  display: grid;
  gap: 8px;
  color: var(--auth-soil);
  font-size: 13px;
  font-weight: 800;
}

input {
  width: 100%;
  min-height: 52px;
  padding: 0 16px;
  border: 2px solid rgba(74, 51, 37, 0.22);
  border-radius: 12px;
  outline: 0;
  color: var(--auth-soil);
  background: #fff;
  font: 600 16px/1 'Noto Sans SC', sans-serif;
  user-select: text;
  -webkit-user-select: text;
  transition: border-color 160ms ease, box-shadow 160ms ease, transform 160ms ease;
}

input::placeholder {
  color: rgba(74, 51, 37, 0.38);
}

input:focus {
  border-color: var(--auth-leaf);
  box-shadow: 0 0 0 4px rgba(47, 125, 74, 0.14);
  transform: translateY(-1px);
}

.form-message {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 10px 12px;
  border: 1px solid rgba(231, 101, 63, 0.34);
  border-radius: 11px;
  color: #9e351f;
  background: rgba(231, 101, 63, 0.1);
  font-size: 13px;
  font-weight: 700;
}

.form-message span {
  display: inline-grid;
  flex: none;
  width: 21px;
  height: 21px;
  place-items: center;
  border-radius: 50%;
  color: #fff;
  background: var(--auth-tomato);
  font-size: 12px;
}

.submit-button {
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-height: 58px;
  margin-top: 3px;
  padding: 0 12px 0 20px;
  border: 2px solid var(--auth-soil);
  border-radius: 13px;
  color: var(--auth-paper);
  background: var(--auth-leaf);
  box-shadow: 5px 6px 0 var(--auth-soil);
  font: 800 15px/1 'Noto Sans SC', sans-serif;
  cursor: pointer;
  transition: box-shadow 150ms ease, transform 150ms ease, background 150ms ease;
}

.submit-button i {
  display: grid;
  width: 36px;
  height: 36px;
  place-items: center;
  border-radius: 50%;
  color: var(--auth-soil);
  background: var(--auth-harvest);
  font-style: normal;
  font-size: 20px;
  transition: transform 170ms ease;
}

.submit-button:not(:disabled):hover {
  background: #256d3e;
  box-shadow: 3px 4px 0 var(--auth-soil);
  transform: translate(2px, 2px);
}

.submit-button:not(:disabled):hover i {
  transform: translateX(3px);
}

.submit-button:disabled,
.mode-switch button:disabled,
input:disabled {
  cursor: wait;
  opacity: 0.68;
}

.mode-switch button:focus-visible,
.submit-button:focus-visible {
  outline: 4px solid rgba(245, 184, 61, 0.7);
  outline-offset: 3px;
}

.form-footnote {
  margin-top: 20px;
  color: rgba(74, 51, 37, 0.52);
  font-size: 11px;
  font-weight: 600;
  line-height: 1.6;
  text-align: center;
}

@keyframes sun-rise {
  from {
    opacity: 0;
    transform: translateY(90px) scale(0.7);
  }
}

@keyframes brand-arrive {
  from {
    opacity: 0;
    transform: translateX(-38px);
  }
}

@keyframes card-plant {
  from {
    opacity: 0;
    transform: translateY(42px) rotate(1.5deg) scale(0.96);
  }
}

@keyframes seedling-sway {
  0%,
  100% {
    rotate: -3deg;
  }
  50% {
    rotate: 4deg;
  }
}

@media (max-width: 900px) {
  .auth-page {
    grid-template-columns: 1fr;
    align-content: center;
    gap: 28px;
    overflow-x: hidden;
    overflow-y: auto;
    padding: 40px clamp(18px, 6vw, 54px) 56px;
  }

  .auth-page::before {
    bottom: -45vh;
    height: 80vh;
  }

  .sun {
    top: 22px;
    right: 8vw;
    width: 78px;
  }

  .brand-column {
    align-self: auto;
    min-height: 0;
    padding-top: 8px;
  }

  .brand-column h1 {
    font-size: clamp(64px, 18vw, 108px);
  }

  .brand-copy {
    max-width: 520px;
    margin-top: 13px;
    font-size: 15px;
  }

  .field-signature {
    display: none;
  }

  .auth-card {
    max-width: 580px;
    justify-self: start;
  }
}

@media (max-width: 520px) {
  .auth-page {
    display: block;
    padding: 30px 16px 42px;
  }

  .sun {
    top: 18px;
    right: 18px;
    width: 62px;
    box-shadow: 0 0 0 11px rgba(245, 184, 61, 0.16);
  }

  .brand-column {
    padding: 32px 8px 26px;
  }

  .brand-kicker {
    margin-bottom: 10px;
    padding: 5px 10px;
    font-size: 10px;
  }

  .brand-column h1 {
    font-size: clamp(56px, 20vw, 82px);
    text-shadow: 4px 5px 0 rgba(247, 251, 242, 0.75);
  }

  .brand-copy {
    max-width: 300px;
    font-size: 13px;
    line-height: 1.65;
  }

  .auth-card {
    padding: 27px 21px 24px;
    border-width: 2px;
    border-radius: 24px 24px 18px 18px;
    box-shadow: 7px 8px 0 var(--auth-soil);
  }

  .form-heading {
    margin: 24px 0 20px;
  }

  .form-heading h2 {
    font-size: 26px;
  }
}

@media (prefers-reduced-motion: reduce) {
  .sun,
  .brand-column,
  .auth-card,
  .seedling {
    animation: none;
  }

  .mode-switch button,
  input,
  .submit-button,
  .submit-button i {
    transition: none;
  }
}
</style>
