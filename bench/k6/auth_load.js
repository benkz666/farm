/**
 * 注册 + 登录压测。
 *
 * Prometheus remote write:
 *   K6_PROMETHEUS_RW_SERVER_URL=http://prometheus:9090/api/v1/write \
 *   k6 run --out experimental-prometheus-rw bench/k6/auth_load.js
 * k6 会读取 K6_PROMETHEUS_RW_SERVER_URL；脚本本身不需要额外 exporter。
 */
import { check } from 'k6'
import {
  DEFAULT_BASE_URL,
  login,
  register,
} from './lib/protocol.js'

const BASE_URL = __ENV.BASE_URL || DEFAULT_BASE_URL
const PASSWORD = __ENV.PASSWORD || __ENV.AUTH_PASSWORD || 'k6-load-password-123!'
const LOGIN_USERNAME = __ENV.USERNAME || __ENV.AUTH_USERNAME || ''
const SCENARIO = String(__ENV.SCENARIO || 'register-login').toLowerCase()
const TARGET_VUS = Math.max(1, Number(__ENV.TARGET_VUS || 50))

export const options = {
  stages: [
    { duration: __ENV.RAMP_UP || '30s', target: Math.max(1, Math.ceil(TARGET_VUS * 0.2)) },
    { duration: __ENV.RAMP_HOLD || '1m', target: TARGET_VUS },
    { duration: __ENV.RAMP_DOWN || '30s', target: 0 },
  ],
  thresholds: {
    // 注册和登录合计的 HTTP 端到端延迟 SLO。
    http_req_duration: ['p(95)<200'],
    // 失败率必须严格低于 0.1%。
    http_req_failed: ['rate<0.001'],
    checks: ['rate>0.999'],
  },
}

function isRegisterOnly() {
  return SCENARIO === 'register' ||
    SCENARIO === 'register-only' ||
    SCENARIO === 'register_only'
}

function isLoginOnly() {
  return SCENARIO === 'login' ||
    SCENARIO === 'login-only' ||
    SCENARIO === 'login_only'
}

function uniqueUsername() {
  // account.username VARCHAR(32)；保留前缀短一些。
  return `a${__VU}i${__ITER}t${Date.now().toString(36)}`.slice(0, 32)
}

function checkAuthResult(operation, result) {
  return check(result, {
    [`${operation} returned credentials`]: (value) =>
      value !== null &&
      value !== undefined &&
      typeof value.token === 'string' &&
      value.token.length > 0,
  })
}

export default function () {
  if (isLoginOnly()) {
    const configured = check(null, {
      'login-only credentials are configured': () => LOGIN_USERNAME.length > 0,
    })
    if (!configured) return

    checkAuthResult('login', login(BASE_URL, LOGIN_USERNAME, PASSWORD))
    return
  }

  const username = uniqueUsername()
  const registered = register(BASE_URL, username, PASSWORD)
  const registerOK = checkAuthResult('register', registered)
  if (!registerOK || isRegisterOnly()) return

  // 未知 SCENARIO 也按 register-login 处理，避免误把压测变成空跑。
  checkAuthResult('login', login(BASE_URL, username, PASSWORD))
}
