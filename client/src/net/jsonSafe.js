/**
 * 解析 JSON，并把超出 JS 安全整数范围的整数字面量先转成字符串。
 *
 * 2^53 本身虽然能被 Number 表示，但它的相邻整数不能；ID/序列一旦进入这个
 * 范围就不能再当 Number 使用。因此必须按 Number.isSafeInteger 的边界判断，
 * 不能用一次字符串往返是否碰巧相等来判断。
 * @param {string} text
 * @returns {any}
 */
export function parseJSONSafe(text) {
  const src = typeof text === 'string' ? text : String(text)
  let quoted = ''
  let cursor = 0
  let index = 0
  const limit = BigInt(Number.MAX_SAFE_INTEGER)

  while (index < src.length) {
    if (src[index] === '"') {
      index++
      while (index < src.length) {
        if (src[index] === '\\') {
          index += 2
          continue
        }
        if (src[index++] === '"') break
      }
      continue
    }
    if (src[index] !== '-' && (src[index] < '0' || src[index] > '9')) {
      index++
      continue
    }

    const start = index
    if (src[index] === '-') index++
    const integerStart = index
    if (src[index] === '0') {
      index++
    } else if (src[index] >= '1' && src[index] <= '9') {
      while (src[index] >= '0' && src[index] <= '9') index++
    }
    if (index === integerStart) {
      index = start + 1
      continue
    }
    let integer = true
    if (src[index] === '.') {
      integer = false
      index++
      while (src[index] >= '0' && src[index] <= '9') index++
    }
    if (src[index] === 'e' || src[index] === 'E') {
      integer = false
      index++
      if (src[index] === '+' || src[index] === '-') index++
      while (src[index] >= '0' && src[index] <= '9') index++
    }

    const token = src.slice(start, index)
    if (integer && token.length >= 16) {
      try {
        const value = BigInt(token)
        if (value < -limit || value > limit) {
          quoted += src.slice(cursor, start) + `"${token}"`
          cursor = index
        }
      } catch {
        // 交给 JSON.parse 返回标准语法错误。
      }
    }
  }
  quoted += src.slice(cursor)
  return JSON.parse(quoted)
}

const MAX_UINT64 = (1n << 64n) - 1n

/**
 * 将 uint64 规范为可安全透传的值。
 * 字符串不经过 Number；安全整数保持 number 以兼容旧协议；bigint 转字符串。
 * 已经落入不安全 Number 的值无法恢复原数，因此直接拒绝。
 *
 * @param {unknown} value
 * @returns {string|number|null}
 */
export function wireUint64(value) {
  if (value == null || value === '') return null
  if (typeof value === 'number') {
    return Number.isSafeInteger(value) && value >= 0 ? value : null
  }
  let parsed
  if (typeof value === 'bigint') {
    parsed = value
  } else if (typeof value === 'string') {
    const text = value.trim()
    if (!/^\d+$/.test(text)) return null
    try {
      parsed = BigInt(text)
    } catch {
      return null
    }
  } else {
    return null
  }
  if (parsed < 0n || parsed > MAX_UINT64) return null
  return parsed.toString()
}

/**
 * 规范 uid：保留字符串精确值；安全整数可用 number；0 不是有效 UID。
 * @param {unknown} uid
 * @returns {string|number|null}
 */
export function wireUid(uid) {
  const value = wireUint64(uid)
  return value == null || String(value) === '0' ? null : value
}

function uint64BigInt(value) {
  const normalized = wireUint64(value)
  return normalized == null ? null : BigInt(normalized)
}

/**
 * 比较两个 uint64。任一值非法时返回 null。
 * @returns {-1|0|1|null}
 */
export function compareUint64(left, right) {
  const a = uint64BigInt(left)
  const b = uint64BigInt(right)
  if (a == null || b == null) return null
  return a < b ? -1 : a > b ? 1 : 0
}

/** 判断 candidate 是否恰好是 current 的下一个 uint64。 */
export function isNextUint64(candidate, current) {
  const next = uint64BigInt(candidate)
  const previous = uint64BigInt(current)
  return next != null && previous != null && previous < MAX_UINT64 && next === previous + 1n
}

/** 比较两个 uint64 是否表示同一个整数。 */
export function sameUint64(left, right) {
  return compareUint64(left, right) === 0
}

/**
 * 比较 UID 时统一使用十进制字符串，避免 19 位雪花落入 JS Number 后丢精度。
 * @param {unknown} left
 * @param {unknown} right
 */
export function sameUid(left, right) {
  const a = wireUid(left)
  const b = wireUid(right)
  if (a == null || b == null) return false
  return sameUint64(a, b)
}
