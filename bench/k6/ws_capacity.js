/**
 * WebSocket 长连接承载压测（可选带业务活动）。
 *
 * 每个 VU：注册 → 登录 → 握手 → 进入自己的农场。
 *
 * ACTIVITY：
 * - idle（默认）：仅每 30s ping，测纯连接承载/内存
 * - sync：连接存活期间按 SYNC_INTERVAL_MS 循环 SyncFarm（默认 1s 一次，
 *   即单连接 1 QPS，仍远低于令牌桶 8 QPS 上限）
 *
 * 压测机应运行在 Gateway/集群之外。
 */
import { check } from 'k6'
import { Counter, Gauge, Rate, Trend } from 'k6/metrics'
import {
  DEFAULT_BASE_URL,
  login,
  parseDurationMs,
  register,
  resolveWsUrl,
  withFarmSession,
} from './lib/protocol.js'

const BASE_URL = __ENV.BASE_URL || DEFAULT_BASE_URL
const PASSWORD = __ENV.PASSWORD || 'k6-capacity-password-123!'
const TARGET_CONNECTIONS = Math.max(
  1,
  Number(__ENV.TARGET_CONNECTIONS || __ENV.TARGET_VUS || 100),
)
const TEST_DURATION = __ENV.TEST_DURATION || __ENV.CONNECTION_DURATION || '10m'
const CONNECTION_DURATION_MS = parseDurationMs(TEST_DURATION)
const ACTIVITY = String(__ENV.ACTIVITY || 'idle').toLowerCase()
const SYNC_INTERVAL_MS = Math.max(
  125,
  Number(__ENV.SYNC_INTERVAL_MS || 1000),
)

export const handshakeLatency = new Trend('ws_handshake_latency', true)
export const activeConnections = new Gauge('ws_active_connections')
export const connectionDrops = new Counter('ws_connection_drops')
export const connectionDropRate = new Rate('ws_connection_drop_rate')
export const syncLatency = new Trend('ws_sync_latency', true)
export const syncFailures = new Counter('ws_sync_failures')
export const syncSuccess = new Counter('ws_sync_success')

const thresholds = {
  'ws_handshake_latency': ['p(95)<500'],
  'ws_connection_drops': ['count==0'],
  'ws_connection_drop_rate': ['rate==0'],
  checks: ['rate>0.999'],
}
if (ACTIVITY === 'sync') {
  thresholds.ws_sync_latency = ['p(95)<100', 'p(99)<200']
  thresholds.ws_sync_failures = ['count==0']
}

export const options = {
  stages: [
    { duration: __ENV.RAMP_UP || '1m', target: Math.max(1, Math.ceil(TARGET_CONNECTIONS * 0.1)) },
    { duration: __ENV.RAMP_STEP || '2m', target: Math.max(1, Math.ceil(TARGET_CONNECTIONS * 0.5)) },
    { duration: __ENV.RAMP_HOLD || TEST_DURATION, target: TARGET_CONNECTIONS },
    { duration: __ENV.RAMP_DOWN || '30s', target: 0 },
  ],
  thresholds,
}

function checkCredentials(operation, auth) {
  return check(auth, {
    [`${operation} returned a token`]: (value) =>
      value !== null &&
      value !== undefined &&
      typeof value.token === 'string' &&
      value.token.length > 0,
  })
}

function scheduleSync(session, state) {
  session.setTimeout((currentSession) => {
    if (currentSession.closed) return
    const startedAt = Date.now()
    currentSession.syncFarm(0, state.farmSeq, (response, error) => {
      if (currentSession.closed && currentSession.expectedClose && error) return

      const ok = !error &&
        response !== null &&
        response !== undefined &&
        response.err === 0
      if (ok) {
        syncSuccess.add(1)
        syncLatency.add(Date.now() - startedAt)
        const next = response.payload && response.payload.farm_seq
        if (next !== undefined && next !== null) state.farmSeq = next
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
  }, SYNC_INTERVAL_MS)
}

export default function () {
  // account.username VARCHAR(32)
  const username = `c${__VU}i${__ITER}t${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`.slice(0, 32)
  const registration = register(BASE_URL, username, PASSWORD)
  if (!checkCredentials('register', registration)) return

  const auth = login(BASE_URL, username, PASSWORD)
  if (!checkCredentials('login', auth)) return

  let openedAt = 0
  let connectionWasOpened = false
  const wsResponse = withFarmSession(
    resolveWsUrl(auth, BASE_URL),
    auth.token,
    (session) => {
      session.enterFarm(0, (response, error) => {
        const entered = check(response, {
          'enter farm succeeds': (value) =>
            !error && value !== null && value !== undefined && value.err === 0,
        })
        if (!entered) {
          session.close('enter farm failed', false)
          return
        }

        // 保活 ping 始终保留。
        session.setInterval((currentSession) => {
          currentSession.ping((pong, pingError) => {
            const pingOK = !pingError &&
              pong !== null &&
              pong !== undefined &&
              pong.err === 0
            if (!pingOK) currentSession.close('ping failed', false)
          })
        }, 30_000)

        if (ACTIVITY === 'sync') {
          const state = {
            farmSeq: (response.payload && response.payload.farm_seq) || '0',
          }
          scheduleSync(session, state)
        }
      })
    },
    {
      durationMs: CONNECTION_DURATION_MS,
      onOpen: () => {
        openedAt = Date.now()
        connectionWasOpened = true
        activeConnections.add(1)
      },
      onHandshake: (response, error) => {
        if (openedAt > 0) {
          handshakeLatency.add(Date.now() - openedAt)
        }
        check(response, {
          'websocket handshake succeeds': (value) =>
            !error &&
            value !== null &&
            value !== undefined &&
            value.err === 0,
        })
      },
      onClose: (_session, info) => {
        if (!connectionWasOpened) return
        activeConnections.add(-1)
        const dropped = !info.expected
        connectionDropRate.add(dropped ? 1 : 0)
        if (dropped) connectionDrops.add(1)
      },
    },
  )

  check(wsResponse, {
    'websocket upgrade returns 101': (response) =>
      response !== null &&
      response !== undefined &&
      response.status === 101,
  })
}
