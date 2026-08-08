/**
 * 农场快照/增量同步读路径压测。
 *
 * 默认每个 VU 自行注册并登录；也可通过 AUTH_USERNAME/AUTH_PASSWORD
 * （或 USERNAME_TEMPLATE）复用已有账号。每个 VU 建立一条 WS，随后串行
 * SyncFarm；请求间隔至少 125ms，即单连接不超过 8 commands/sec。
 */
import { check } from 'k6'
import { Counter, Trend } from 'k6/metrics'
import {
  DEFAULT_BASE_URL,
  login,
  parseDurationMs,
  register,
  resolveWsUrl,
  withFarmSession,
} from './lib/protocol.js'

const BASE_URL = __ENV.BASE_URL || DEFAULT_BASE_URL
const PASSWORD = __ENV.PASSWORD || __ENV.AUTH_PASSWORD || 'k6-read-password-123!'
const USERNAME = __ENV.USERNAME || __ENV.AUTH_USERNAME || ''
const USERNAME_TEMPLATE = __ENV.USERNAME_TEMPLATE || __ENV.AUTH_USERNAME_TEMPLATE || ''
const PASSWORD_TEMPLATE = __ENV.PASSWORD_TEMPLATE || __ENV.AUTH_PASSWORD_TEMPLATE || ''
const TARGET_VUS = Math.max(1, Number(__ENV.TARGET_VUS || 100))
const TEST_DURATION_MS = parseDurationMs(__ENV.TEST_DURATION || '10m')
const CONFIGURED_QPS = Number(__ENV.QPS || 8)
const REQUEST_INTERVAL_MS = Math.max(
  125,
  1000 / Math.min(8, CONFIGURED_QPS > 0 ? CONFIGURED_QPS : 8),
)

export const syncLatency = new Trend('ws_sync_latency', true)
export const syncFailures = new Counter('ws_sync_failures')
export const connectionDrops = new Counter('ws_read_connection_drops')

export const options = {
  stages: [
    { duration: __ENV.RAMP_UP || '1m', target: Math.max(1, Math.ceil(TARGET_VUS * 0.1)) },
    { duration: __ENV.RAMP_STEP || '2m', target: Math.max(1, Math.ceil(TARGET_VUS * 0.5)) },
    { duration: __ENV.RAMP_HOLD || __ENV.TEST_DURATION || '10m', target: TARGET_VUS },
    { duration: __ENV.RAMP_DOWN || '30s', target: 0 },
  ],
  thresholds: {
    'ws_sync_latency': ['p(95)<100', 'p(99)<200'],
    'ws_sync_failures': ['count==0'],
    'ws_read_connection_drops': ['count==0'],
    checks: ['rate>0.999'],
  },
}

function checkAuthCredentials(operation, auth) {
  return check(auth, {
    [`${operation} returned a token`]: (value) =>
      value !== null &&
      value !== undefined &&
      typeof value.token === 'string' &&
      value.token.length > 0 &&
      value.uid !== undefined &&
      value.uid !== null,
  })
}

function nextFarmSeq(response, previous) {
  const value = response && response.payload && response.payload.farm_seq
  return value === undefined || value === null ? previous : value
}

function credential(template, fallback) {
  return String(template || fallback)
    .replace(/\{vu\}/g, String(__VU))
    .replace(/\{iter\}/g, String(__ITER))
}

function scheduleSync(session, state) {
  session.setTimeout((currentSession) => {
    if (currentSession.closed) return
    const startedAt = Date.now()
    currentSession.syncFarm(state.ownerUid, state.farmSeq, (response, error) => {
      if (currentSession.closed && currentSession.expectedClose && error) return

      const succeeded = !error &&
        response !== null &&
        response !== undefined &&
        response.err === 0
      if (succeeded) {
        syncLatency.add(Date.now() - startedAt)
        state.farmSeq = nextFarmSeq(response, state.farmSeq)
        scheduleSync(currentSession, state)
        return
      }

      syncFailures.add(1)
      check(response, {
        'sync farm succeeds': (value) =>
          !error &&
          value !== null &&
          value !== undefined &&
          value.err === 0,
      })
      currentSession.close('sync farm failed', false)
    })
  }, REQUEST_INTERVAL_MS)
}

function authenticate() {
  // 显式账号或模板：复用已有用户。否则每个 VU 自行注册。
  if (USERNAME_TEMPLATE || USERNAME) {
    const username = credential(USERNAME_TEMPLATE, USERNAME)
    const password = credential(PASSWORD_TEMPLATE, PASSWORD)
    const auth = login(BASE_URL, username, password)
    if (!checkAuthCredentials('login', auth)) return null
    return auth
  }

  const username = `r${__VU}i${__ITER}t${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`.slice(0, 32)
  const registration = register(BASE_URL, username, PASSWORD)
  if (!checkAuthCredentials('register', registration)) return null

  const auth = login(BASE_URL, username, PASSWORD)
  if (!checkAuthCredentials('login', auth)) return null
  return auth
}

export default function () {
  const auth = authenticate()
  if (!auth) return

  const state = {
    // owner_uid=0 表示自己的农场；与客户端一致。
    ownerUid: 0,
    farmSeq: '0',
  }
  let connectionWasOpened = false

  const wsResponse = withFarmSession(
    resolveWsUrl(auth, BASE_URL),
    auth.token,
    (session) => {
      session.enterFarm(state.ownerUid, (response, error) => {
        const entered = check(response, {
          'enter farm for read test succeeds': (value) =>
            !error &&
            value !== null &&
            value !== undefined &&
            value.err === 0 &&
            value.payload &&
            value.payload.farm_seq !== undefined,
        })
        if (!entered) {
          session.close('enter farm failed', false)
          return
        }
        state.farmSeq = response.payload.farm_seq
        scheduleSync(session, state)
      })
    },
    {
      durationMs: TEST_DURATION_MS,
      onOpen: () => {
        connectionWasOpened = true
      },
      onHandshake: (response, error) => {
        check(response, {
          'read test websocket handshake succeeds': (value) =>
            !error &&
            value !== null &&
            value !== undefined &&
            value.err === 0,
        })
      },
      onClose: (_session, info) => {
        if (connectionWasOpened && !info.expected) connectionDrops.add(1)
      },
    },
  )

  check(wsResponse, {
    'read test websocket upgrade returns 101': (response) =>
      response !== null &&
      response !== undefined &&
      response.status === 101,
  })
}
