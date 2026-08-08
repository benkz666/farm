import http from 'k6/http'
import { check } from 'k6'
import { Counter, Rate, Trend } from 'k6/metrics'
import { DEFAULT_BASE_URL, login, normalizeBaseUrl, register } from './lib/protocol.js'

const BASE_URL = normalizeBaseUrl(__ENV.BASE_URL || DEFAULT_BASE_URL)
const SCENARIO = String(__ENV.SCENARIO || 'login')
const TARGET_VUS = Math.max(1, Number(__ENV.TARGET_VUS || 10))
const TARGET_QPS = Math.max(0, Number(__ENV.TARGET_QPS || 0))
const DURATION = __ENV.DURATION || '1m'
const PASSWORD = __ENV.PASSWORD || 'k6-load-password-123!'
const success = new Counter('api_operation_success')
const failures = new Rate('api_system_failure_rate')
const latency = new Trend('api_operation_latency', true)

if (!['register', 'login', 'inviteLanding', 'debugAdvance'].includes(SCENARIO)) {
  throw new Error(`unknown SCENARIO=${SCENARIO}`)
}

const repeatable = SCENARIO === 'login' || SCENARIO === 'inviteLanding'
export const options = TARGET_QPS > 0
  ? {
      scenarios: {
        api: {
          executor: 'constant-arrival-rate',
          rate: TARGET_QPS,
          timeUnit: '1s',
          duration: DURATION,
          preAllocatedVUs: TARGET_VUS,
          maxVUs: Math.max(TARGET_VUS, TARGET_VUS * 4),
        },
      },
      thresholds: { api_system_failure_rate: ['rate<0.001'], dropped_iterations: ['count==0'] },
    }
  : repeatable
  ? {
      scenarios: { api: { executor: 'constant-vus', vus: TARGET_VUS, duration: DURATION } },
      thresholds: { api_system_failure_rate: ['rate<0.001'] },
    }
  : {
      scenarios: { api: { executor: 'shared-iterations', vus: TARGET_VUS, iterations: TARGET_VUS, maxDuration: DURATION } },
      thresholds: { api_system_failure_rate: ['rate<0.001'] },
    }

export default function () {
  const startedAt = Date.now()
  let ok = false
  if (SCENARIO === 'register') {
    const username = `h${__VU}i${__ITER}t${Date.now().toString(36)}`.slice(0, 32)
    ok = register(BASE_URL, username, PASSWORD) !== null
  } else if (SCENARIO === 'login') {
    ok = login(BASE_URL, requiredEnv('USERNAME'), PASSWORD) !== null
  } else if (SCENARIO === 'inviteLanding') {
    const response = http.get(`${BASE_URL}/i/${encodeURIComponent(requiredEnv('INVITE_TOKEN'))}`, { redirects: 0, tags: { operation: SCENARIO } })
    ok = response.status === 200 || response.status === 302 || response.status === 303
  } else {
    const response = http.post(`${BASE_URL}/api/debug/advance`, JSON.stringify({ seconds: Number(__ENV.ADVANCE_SECONDS || 60) }), {
      headers: { 'Content-Type': 'application/json' },
      tags: { operation: SCENARIO },
    })
    ok = response.status === 200
  }
  const tags = { operation: SCENARIO }
  latency.add(Date.now() - startedAt, tags)
  failures.add(!ok, tags)
  if (ok) success.add(1, tags)
  check(ok, { [`${SCENARIO} succeeded`]: (value) => value === true })
}

function requiredEnv(name) {
  const value = __ENV[name]
  if (!value) throw new Error(`${name} is required for SCENARIO=${SCENARIO}`)
  return value
}
