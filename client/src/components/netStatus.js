export const NET_STATE_REFRESH_INTERVAL_MS = 250
export const NET_PROBE_INTERVAL_MS = 1_000
export const NET_PROBE_TIMEOUT_MS = 1_500
export const GOOD_NETWORK_MAX_LATENCY_MS = 200
export const LATENCY_SAMPLE_LIMIT = 3

const ONLINE_QUALITY_STATES = new Set(['good', 'weak'])

/**
 * 最近少量 RTT 取中位数，避免单次抖动导致良好/弱网颜色频繁闪烁。
 * @param {number[]} samples
 */
export function medianLatency(samples) {
  const values = samples
    .map(Number)
    .filter((value) => Number.isFinite(value) && value >= 0)
    .sort((left, right) => left - right)
  if (values.length === 0) return null
  return Math.round(values[Math.floor(values.length / 2)])
}

/** @param {number|null|undefined} latencyMs */
export function networkQuality(latencyMs) {
  if (!Number.isFinite(latencyMs) || latencyMs < 0) return 'checking'
  return latencyMs <= GOOD_NETWORK_MAX_LATENCY_MS ? 'good' : 'weak'
}

/**
 * 良好和弱网只显示 RTT；非在线阶段显示明确的连接生命周期。
 * @param {string} state
 * @param {number|null|undefined} latencyMs
 */
export function networkStatusLabel(state, latencyMs) {
  if (ONLINE_QUALITY_STATES.has(state) && Number.isFinite(latencyMs)) {
    return `${Math.max(0, Math.round(latencyMs))}ms`
  }
  switch (state) {
    case 'connecting': return '连接中'
    case 'reconnecting': return '重连中'
    case 'restoring': return '同步中'
    case 'checking': return '检测中'
    case 'offline':
    case 'unreachable':
    default:
      return '断网'
  }
}
