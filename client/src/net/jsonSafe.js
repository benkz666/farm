/**
 * 解析 JSON，并把无法在 JS Number 中精确表示的整数字面量先转成字符串。
 * @param {string} text
 * @returns {any}
 */
export function parseJSONSafe(text) {
  const src = typeof text === 'string' ? text : String(text)
  const quoteUnsafe = (_m, prefix, digits) => {
    if (digits.length < 16) return `${prefix}${digits}`
    // 与 Number 往返一致则仍可当 number；雪花通常会丢精度
    if (String(Number(digits)) === digits) return `${prefix}${digits}`
    return `${prefix}"${digits}"`
  }
  let quoted = src.replace(/(:\s*)(-?\d+)\b/g, quoteUnsafe)
  quoted = quoted.replace(/([\[,]\s*)(-?\d+)\b/g, quoteUnsafe)
  return JSON.parse(quoted)
}

/**
 * 规范 uid：保留字符串精确值；安全整数可用 number。
 * @param {unknown} uid
 * @returns {string|number|null}
 */
export function wireUid(uid) {
  if (uid == null || uid === '') return null
  if (typeof uid === 'string') {
    const s = uid.trim()
    if (!/^\d+$/.test(s)) return null
    return s
  }
  if (typeof uid === 'number' && Number.isFinite(uid) && uid > 0) {
    if (Number.isSafeInteger(uid)) return uid
    // 已截断的 Number 无法恢复，仍原样传出会错；调用方应避免
    return String(uid)
  }
  if (typeof uid === 'bigint') return uid.toString()
  return null
}
