import { check } from 'k6'
import { SharedArray } from 'k6/data'
import { Counter, Rate, Trend } from 'k6/metrics'
import {
  DEFAULT_BASE_URL,
  login,
  parseDurationMs,
  register,
  resolveWsUrl,
  withFarmSession,
} from './lib/protocol.js'

const BASE_URL = __ENV.BASE_URL || DEFAULT_BASE_URL
const WS_URL_OVERRIDE = String(__ENV.WS_URL_OVERRIDE || '')
const SCENARIO = String(__ENV.SCENARIO || 'all')
const TARGET_VUS = Math.max(1, Number(__ENV.TARGET_VUS || 10))
const DURATION = __ENV.DURATION || '1m'
const DATA_FILE = __ENV.DATA_FILE || ''
const PASSWORD = __ENV.PASSWORD || 'k6-load-password-123!'
const SYNC_FROM_SEQ = String(__ENV.SYNC_FROM_SEQ || '')
const FIXTURE_SHARD_INDEX = Math.max(0, Number(__ENV.FIXTURE_SHARD_INDEX || 0))
const FIXTURE_SHARD_COUNT = Math.max(1, Number(__ENV.FIXTURE_SHARD_COUNT || 1))
if (!Number.isInteger(FIXTURE_SHARD_INDEX) || !Number.isInteger(FIXTURE_SHARD_COUNT) || FIXTURE_SHARD_INDEX >= FIXTURE_SHARD_COUNT) {
  throw new Error('FIXTURE_SHARD_INDEX 必须是 [0, FIXTURE_SHARD_COUNT) 内的整数')
}

const operationLatency = new Trend('api_operation_latency', true)
const operationSuccess = new Counter('api_operation_success')
const businessRejections = new Counter('api_business_rejections')
const systemFailures = new Counter('api_system_failures')
const systemFailureRate = new Rate('api_system_failure_rate')
const pushDelivery = new Counter('api_push_delivery')
const unexpectedDisconnects = new Counter('api_unexpected_disconnects')
const wsCloses = new Counter('api_ws_closes')
const wsErrors = new Counter('api_ws_errors')

const OPERATIONS = Object.freeze([
  { id: 'ping', cmd: 102, repeatable: true, payload: () => ({ client_time: Date.now() }) },
  { id: 'enterFarm', cmd: 200, repeatable: true, payload: (f) => ({ owner_uid: f.owner_uid || '0' }) },
  { id: 'leaveFarm', cmd: 202, payload: () => ({}) },
  { id: 'syncFarm', cmd: 204, repeatable: true, enter: true, payload: (f) => ({ owner_uid: f.owner_uid || '0', from_seq: SYNC_FROM_SEQ || f.from_seq || '0' }) },
  { id: 'till', cmd: 206, enter: true, payload: (f) => plotPayload(f, 0) },
  { id: 'clear', cmd: 208, enter: true, payload: (f) => plotPayload(f, 1) },
  { id: 'plant', cmd: 210, enter: true, payload: (f) => plotPayload(f, f.crop_id || 1) },
  { id: 'water', cmd: 212, enter: true, payload: (f) => plotPayload(f, 0) },
  { id: 'removeWeed', cmd: 214, enter: true, payload: (f) => plotPayload(f, 0) },
  { id: 'removePest', cmd: 216, enter: true, payload: (f) => plotPayload(f, 0) },
  { id: 'fertilize', cmd: 218, enter: true, payload: (f) => plotPayload(f, f.fertilizer_id || 1) },
  { id: 'harvest', cmd: 220, enter: true, payload: (f) => plotPayload(f, 0) },
  { id: 'steal', cmd: 222, enter: true, payload: (f) => ({ owner_uid: required(f, 'peer_uid'), plot_index: f.plot_index || 0, crop_id: f.crop_id || 1 }) },
  { id: 'buy', cmd: 302, payload: (f) => ({ item_id: f.item_id || 1, quantity: f.quantity || 1 }) },
  { id: 'sell', cmd: 304, payload: (f) => ({ item_id: f.item_id || 1, quantity: f.quantity || 1 }) },
  { id: 'friendList', cmd: 400, repeatable: true, payload: () => ({}) },
  { id: 'genShareLink', cmd: 402, repeatable: true, payload: () => ({}) },
  { id: 'acceptInvite', cmd: 404, payload: (f) => ({ token: f.invite_token || 'invalid-benchmark-token' }) },
  { id: 'removeFriend', cmd: 406, payload: (f) => ({ peer_uid: required(f, 'peer_uid') }) },
  { id: 'addFriendByUID', cmd: 408, payload: (f) => ({ peer_uid: required(f, 'peer_uid') }) },
  { id: 'searchUser', cmd: 410, repeatable: true, payload: (f) => ({ username: required(f, 'peer_username') }) },
  { id: 'requestFriend', cmd: 412, payload: (f) => ({ peer_uid: required(f, 'peer_uid') }) },
  { id: 'listFriendRequests', cmd: 414, repeatable: true, payload: () => ({}) },
  { id: 'acceptFriendRequest', cmd: 416, payload: (f) => ({ from_uid: required(f, 'from_uid') }) },
  { id: 'rejectFriendRequest', cmd: 418, payload: (f) => ({ from_uid: required(f, 'from_uid') }) },
  { id: 'petStatus', cmd: 500, repeatable: true, payload: () => ({}) },
  { id: 'petActivate', cmd: 502, payload: (f) => ({ dog_type: f.dog_type || 1 }) },
  { id: 'petFeed', cmd: 504, payload: (f) => ({ grams: f.grams || 1 }) },
  { id: 'taskList', cmd: 600, repeatable: true, payload: () => ({}) },
  { id: 'taskClaim', cmd: 602, payload: (f) => ({ task_id: required(f, 'task_id') }) },
  { id: 'mailList', cmd: 604, repeatable: true, payload: () => ({}) },
  { id: 'mailRead', cmd: 606, payload: () => ({ all: true }) },
  { id: 'mailClaim', cmd: 608, payload: (f) => ({ mail_id: required(f, 'mail_id') }) },
  { id: 'mailDelete', cmd: 610, payload: () => ({ all: true }) },
  { id: 'codexList', cmd: 612, repeatable: true, payload: () => ({}) },
  { id: 'claimDailyLogin', cmd: 614, payload: () => ({}) },
  { id: 'setTimeProfile', cmd: 616, payload: (f) => ({ time_profile: f.time_profile || 'demo' }) },
])

const BY_ID = Object.freeze(Object.fromEntries(OPERATIONS.map((operation) => [operation.id, operation])))
const selected = SCENARIO === 'all' ? null : BY_ID[SCENARIO]
if (SCENARIO !== 'all' && SCENARIO !== 'handshake' && !selected) {
  throw new Error(`unknown SCENARIO=${SCENARIO}`)
}

// k6 creates a separate JavaScript runtime for every VU. Parsing a large fixture
// file directly here would duplicate the complete account pool in every runtime
// and can exhaust the load-generator memory before the target service is busy.
// SharedArray parses it once and exposes a read-only view to all VUs.
const fixtures = DATA_FILE
  ? new SharedArray('api-fixtures', () => loadFixtures(DATA_FILE))
  : []
const selectedRepeatable = selected && selected.repeatable
const loadMode = String(__ENV.MODE || 'baseline').toLowerCase() === 'load'
const loadEligible = selected && (selectedRepeatable || __ENV.ALLOW_STATEFUL_LOAD === '1')

export const options = loadMode && loadEligible
  ? {
      summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
      scenarios: {
        api: { executor: 'constant-vus', vus: TARGET_VUS, duration: DURATION },
      },
      thresholds: thresholdsFor(selected.id),
    }
  : {
      summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
      scenarios: {
        api: { executor: 'shared-iterations', vus: TARGET_VUS, iterations: TARGET_VUS, maxDuration: DURATION },
      },
      thresholds: { api_system_failure_rate: ['rate<0.001'], checks: ['rate>0.99'] },
    }

export function setup() {
  if (fixtures.length === 0 && __ENV.USERNAME) return null
  if (fixtures.length === 0 && __ENV.ALLOW_RUNTIME_REGISTER !== '1') {
    throw new Error('DATA_FILE 或 USERNAME 必填；测量窗口内默认禁止注册，临时冒烟可设置 ALLOW_RUNTIME_REGISTER=1')
  }
  return null
}

export default function () {
  const fixture = fixtureForVU()
  const auth = authenticate(fixture)
  if (!auth) return

  const operations = SCENARIO === 'all' ? OPERATIONS : (SCENARIO === 'handshake' ? [] : [selected])
  const durationMs = Math.max(5_000, parseDurationMs(DURATION))
  const handshakeStartedAt = Date.now()
  withFarmSession(WS_URL_OVERRIDE || resolveWsUrl(auth, BASE_URL), auth.token, (session) => {
    if (SCENARIO === 'handshake') {
      session.close('handshake baseline complete', true)
      return
    }
    executeSequence(session, operations, fixture, 0)
  }, {
    durationMs,
    ignorePendingOnExpectedClose: loadMode,
    tags: { scenario: SCENARIO },
    onHandshake: (response, error) => {
      if (SCENARIO === 'handshake') recordResponse({ id: 'handshake', cmd: 100 }, handshakeStartedAt, response, error)
    },
    onPush: (envelope) => pushDelivery.add(1, { push_cmd: String(envelope.cmd) }),
    onProtocolError: () => recordSystemFailure({ id: SCENARIO, cmd: 0 }),
    onError: (error) => {
	  wsErrors.add(1, { scenario: SCENARIO, kind: classifyWSError(error) })
	  recordSystemFailure({ id: SCENARIO, cmd: 0 })
	},
    onClose: (_session, info) => {
	  wsCloses.add(1, {
	    scenario: SCENARIO,
	    expected: info.expected ? 'true' : 'false',
	    code: closeCodeClass(info.code),
	    client_reason: clientCloseReason(info.closeReason),
	  })
      if (!info.expected) unexpectedDisconnects.add(1, { scenario: SCENARIO })
    },
  })
}

function executeSequence(session, operations, fixture, index) {
  if (index >= operations.length) {
    session.close('baseline sequence complete', true)
    return
  }
  const operation = operations[index]
  const executeTarget = () => {
    let payload
    try {
      payload = operation.payload(fixture)
    } catch (error) {
      recordSystemFailure(operation)
      check(error, { [`${operation.id} fixture is valid`]: () => false })
      session.close('invalid fixture', false)
      return
    }
    const startedAt = Date.now()
    session.request(operation.cmd, payload, (response, error) => {
      recordResponse(operation, startedAt, response, error, fixture)
      if (loadMode && loadEligible) {
        // 按相邻两次请求的起点限速。若在收到响应后固定等待完整间隔，
        // 实际周期会变成“接口延迟 + 间隔”，负载发生器将无法达到目标 QPS。
        // 仍保持串行请求，因此单连接不会超过 8 命令/秒。
        const elapsedMs = Math.max(0, Date.now() - startedAt)
        const remainingMs = Math.max(0, commandIntervalMs() - elapsedMs)
        session.setTimeout(() => executeSequence(session, operations, fixture, index), remainingMs)
      } else {
        executeSequence(session, operations, fixture, index + 1)
      }
    })
  }

  if (!operation.enter || session.benchmarkFarmEntered) {
    executeTarget()
    return
  }
  const enterOwner = operation.id === 'steal' ? required(fixture, 'peer_uid') : (fixture.owner_uid || '0')
  session.enterFarm(enterOwner, (response, error) => {
    if (error || !response || response.err !== 0) {
      recordSystemFailure(operation)
      session.close('farm precondition failed', false)
      return
    }
    session.benchmarkFarmEntered = true
    executeTarget()
  })
}

function recordResponse(operation, startedAt, response, error, fixture = {}) {
  const tags = { operation: operation.id, cmd: String(operation.cmd) }
  operationLatency.add(Math.max(0, Date.now() - startedAt), tags)
  if (error || !response) {
    recordSystemFailure(operation)
    check(response, { [`${operation.id} received response`]: () => false })
    return
  }
  const expected = Array.isArray(fixture.expected_errors) ? fixture.expected_errors.map(Number) : []
  if (response.err === 0) {
    operationSuccess.add(1, tags)
    systemFailureRate.add(false, tags)
    check(response, { [`${operation.id} succeeded`]: (value) => value.err === 0 })
    return
  }
  if (expected.includes(Number(response.err))) {
    businessRejections.add(1, { ...tags, err: String(response.err), expected: 'true' })
    systemFailureRate.add(false, tags)
    return
  }
  businessRejections.add(1, { ...tags, err: String(response.err), expected: 'false' })
  systemFailureRate.add(false, tags)
  check(response, { [`${operation.id} returned no unexpected business error`]: () => false })
}

function recordSystemFailure(operation) {
  const tags = { operation: operation.id, cmd: String(operation.cmd) }
  systemFailures.add(1, tags)
  systemFailureRate.add(true, tags)
}

function authenticate(fixture) {
  if (fixture.token) return { token: fixture.token, ws_url: fixture.ws_url || '' }
  if (fixture.username) return login(BASE_URL, fixture.username, fixture.password || PASSWORD)
  if (__ENV.USERNAME) return login(BASE_URL, __ENV.USERNAME, PASSWORD)
  if (__ENV.ALLOW_RUNTIME_REGISTER === '1') {
    const username = `b${__VU}i${__ITER}t${Date.now().toString(36)}`.slice(0, 32)
    return register(BASE_URL, username, PASSWORD)
  }
  return null
}

function fixtureForVU() {
  if (fixtures.length === 0) return {}
  // Separate load-generator processes target Gateway instances directly and
  // consume disjoint accounts. This avoids token replacement, actor contention
  // and an accidental single-Gateway bottleneck in a 3:1 topology.
  const fixtureIndex = ((__VU - 1) * FIXTURE_SHARD_COUNT + FIXTURE_SHARD_INDEX) % fixtures.length
  return fixtures[fixtureIndex]
}

function loadFixtures(path) {
  if (!path) return []
  const parsed = JSON.parse(open(path))
  const accounts = Array.isArray(parsed) ? parsed : parsed.accounts
  if (!Array.isArray(accounts)) throw new Error('DATA_FILE 必须是数组或包含 accounts 数组')
  return accounts
}

function plotPayload(fixture, arg) {
  return {
    owner_uid: fixture.owner_uid || '0',
    plot_index: fixture.plot_index || 0,
    arg,
  }
}

function required(fixture, key) {
  const value = fixture[key]
  if (value === undefined || value === null || value === '') throw new Error(`fixture missing ${key}`)
  return value
}

function commandIntervalMs() {
  const targetQPS = Math.max(1, Number(__ENV.TARGET_QPS || TARGET_VUS))
  return Math.max(125, Math.ceil((1000 * TARGET_VUS) / targetQPS))
}

function closeCodeClass(code) {
  const value = Number(code)
  if (value === 1000) return 'normal'
  if (value === 1001) return 'going_away'
  if (value === 1005 || !Number.isFinite(value) || value === 0) return 'no_status'
  if (value === 1006) return 'abnormal'
  if (value >= 1002 && value <= 1015) return `protocol_${value}`
  return 'other'
}

function clientCloseReason(reason) {
  const allowed = new Set([
    'connection duration complete',
    'baseline sequence complete',
    'handshake baseline complete',
    'handshake failed',
    'onReady callback failed',
    'invalid fixture',
    'farm precondition failed',
  ])
  return allowed.has(String(reason || '')) ? String(reason) : 'server_or_network'
}

function classifyWSError(error) {
  const message = String(error && error.message ? error.message : error || '').toLowerCase()
  if (message.includes('request timeout')) return 'request_timeout'
  if (message.includes('handshake err=')) return 'handshake_rejected'
  if (message.includes('websocket is closed') || message.includes('websocket closed')) return 'socket_closed'
  if (message.includes('connection reset')) return 'connection_reset'
  if (message.includes('broken pipe')) return 'broken_pipe'
  return 'other'
}

function thresholdsFor(operation) {
  const crossService = ['steal', 'searchUser'].includes(operation)
  const writes = ['water', 'harvest', 'buy', 'sell'].includes(operation)
  const p95 = crossService ? 250 : writes ? 150 : 100
  const p99 = crossService ? 500 : writes ? 300 : 250
  return {
    api_system_failure_rate: ['rate<0.001'],
    [`api_operation_latency{operation:${operation}}`]: [`p(95)<${p95}`, `p(99)<${p99}`],
  }
}
