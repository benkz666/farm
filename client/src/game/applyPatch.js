/**
 * 服务端 snapshot / action patch → 本地 game/state 形状。
 * 数值 state ↔ PLOT 字符串；crop 数字 ↔ bailuobo 等；bag/warehouse 键转换。
 */
import { PLOT, makePlot } from './state.js'
import { WATER_SPAN } from './config.js'

/** 服务端地块状态枚举 → 本地 PLOT 字符串（与 farm.State* 一致）。 */
export const PLOT_STATE_BY_NUM = Object.freeze([
  PLOT.WASTELAND,
  PLOT.TILLED,
  PLOT.GROWING,
  PLOT.MATURE,
  PLOT.RESIDUE,
  PLOT.WITHERED,
])

/** 服务端作物 ID → 客户端字符串 id（期 2 子集）。 */
export const CROP_ID_TO_KEY = Object.freeze({
  1: 'bailuobo',
  2: 'huluobo',
  3: 'dabaicai',
  4: 'pingguo',
})

/** 客户端字符串 id → 服务端作物 ID。 */
export const CROP_KEY_TO_ID = Object.freeze({
  bailuobo: 1,
  huluobo: 2,
  dabaicai: 3,
  pingguo: 4,
})

const FERTILIZER_ID_TO_KEY = Object.freeze({
  1: 'normal',
  2: 'fast',
  3: 'super',
})

/**
 * @param {number|string|null|undefined} id
 * @returns {string|null}
 */
export function cropIdToKey(id) {
  if (id == null || id === 0 || id === '0') return null
  const n = typeof id === 'string' ? Number(id) : id
  if (!Number.isFinite(n) || n <= 0) return null
  return CROP_ID_TO_KEY[n] ?? null
}

/**
 * @param {string|null|undefined} key
 * @returns {number}
 */
export function cropKeyToId(key) {
  if (!key) return 0
  return CROP_KEY_TO_ID[key] ?? 0
}

/**
 * @param {number} stateNum
 * @returns {string}
 */
export function plotStateFromNum(stateNum) {
  const s = PLOT_STATE_BY_NUM[stateNum]
  return s ?? PLOT.WASTELAND
}

/**
 * bag `seed:N` → inventory.seeds[key]；未知 ID 跳过。
 * @param {Record<string, number>|null|undefined} bag
 * @returns {Record<string, number>}
 */
export function bagToSeeds(bag) {
  /** @type {Record<string, number>} */
  const seeds = {}
  if (!bag || typeof bag !== 'object') return seeds
  for (const [itemKey, count] of Object.entries(bag)) {
    if (!itemKey.startsWith('seed:')) continue
    const id = Number(itemKey.slice(5))
    const cropKey = cropIdToKey(id)
    if (!cropKey) continue
    const n = Number(count)
    if (!Number.isFinite(n) || n <= 0) continue
    seeds[cropKey] = n
  }
  return seeds
}

/**
 * bag `fert:N` → inventory.fertilizers[key]。
 * @param {Record<string, number>|null|undefined} bag
 * @returns {Record<string, number>}
 */
export function bagToFertilizers(bag) {
  /** @type {Record<string, number>} */
  const fertilizers = {}
  if (!bag || typeof bag !== 'object') return fertilizers
  for (const [itemKey, count] of Object.entries(bag)) {
    if (!itemKey.startsWith('fert:')) continue
    const fertilizerKey = FERTILIZER_ID_TO_KEY[Number(itemKey.slice(5))]
    if (!fertilizerKey) continue
    const n = Number(count)
    if (!Number.isFinite(n) || n <= 0) continue
    fertilizers[fertilizerKey] = n
  }
  return fertilizers
}

/**
 * warehouse `fruit:N` → warehouse[key]。
 * @param {Record<string, number>|null|undefined} warehouse
 * @returns {Record<string, number>}
 */
export function warehouseFromServer(warehouse) {
  /** @type {Record<string, number>} */
  const out = {}
  if (!warehouse || typeof warehouse !== 'object') return out
  for (const [itemKey, count] of Object.entries(warehouse)) {
    if (!itemKey.startsWith('fruit:')) continue
    const id = Number(itemKey.slice(6))
    const cropKey = cropIdToKey(id)
    if (!cropKey) continue
    const n = Number(count)
    if (!Number.isFinite(n) || n <= 0) continue
    out[cropKey] = n
  }
  return out
}

/**
 * 将单地块服务端快照写入本地 plot 对象（原地修改）。
 * @param {object} plot
 * @param {object} snap
 */
export function applyPlotSnapshot(plot, snap) {
  if (!plot || !snap) return plot
  const stateNum = typeof snap.state === 'number' ? snap.state : 0
  plot.state = plotStateFromNum(stateNum)
  plot.cropId = cropIdToKey(snap.crop_id)
  plot.season = Number(snap.season_index) || 0
  plot.matureTime = Number(snap.mature_at) || 0
  const seasonMs = Number(snap.season_duration) || 0
  plot.seasonMs = seasonMs
  plot.weedSince = Number(snap.weed_since) || 0
  plot.pestSince = Number(snap.pest_since) || 0
  // 本地水分截止 ≈ 上次浇水 + 本季×WATER_SPAN（与服务端 WaterSpanRatio 对齐）
  const lastWater = Number(snap.last_water_at) || 0
  plot.waterUntil = lastWater > 0 && seasonMs > 0
    ? lastWater + Math.floor(seasonMs * WATER_SPAN)
    : 0
  // 无 SeasonStartAt 时用成熟点回推，便于倒计时展示
  if (plot.matureTime > 0 && seasonMs > 0) {
    plot.plantTime = plot.matureTime - seasonMs
  } else if (stateNum === 0 || stateNum === 1) {
    plot.plantTime = 0
    plot.matureTime = 0
    plot.seasonMs = 0
  }
  if (stateNum === 0 || stateNum === 1) {
    plot.cropId = null
  }
  return plot
}

/**
 * @param {object} state
 * @param {object} snap FarmSnapshotJSON
 */
function applyFullSnapshot(state, snap) {
  if (typeof snap.coin === 'number') state.gold = snap.coin
  if (typeof snap.exp === 'number') state.exp = snap.exp
  if (typeof snap.unlocked_plots === 'number') state.unlockedPlots = snap.unlocked_plots

  if (snap.bag) {
    if (!state.inventory) state.inventory = { seeds: {}, fertilizers: {} }
    state.inventory.seeds = bagToSeeds(snap.bag)
    state.inventory.fertilizers = bagToFertilizers(snap.bag)
  }
  if (snap.warehouse) {
    state.warehouse = warehouseFromServer(snap.warehouse)
  }

  if (Array.isArray(snap.plots)) {
    if (!Array.isArray(state.plots)) state.plots = []
    for (const p of snap.plots) {
      const idx = typeof p.index === 'number' ? p.index : -1
      if (idx < 0) continue
      while (state.plots.length <= idx) {
        state.plots.push(makePlot(state.plots.length))
      }
      const local = state.plots[idx]
      if (!local.id && local.id !== 0) local.id = idx
      applyPlotSnapshot(local, p)
    }
  }
}

/**
 * @param {object} state
 * @param {object} patch PatchJSON
 */
function applyActionPatch(state, patch) {
  if (typeof patch.coin === 'number') state.gold = patch.coin
  if (typeof patch.exp === 'number') state.exp = patch.exp

  if (patch.bag) {
    if (!state.inventory) state.inventory = { seeds: {}, fertilizers: {} }
    state.inventory.seeds = bagToSeeds(patch.bag)
    state.inventory.fertilizers = bagToFertilizers(patch.bag)
  }
  if (patch.warehouse) {
    state.warehouse = warehouseFromServer(patch.warehouse)
  }

  if (patch.plot && typeof patch.plot === 'object') {
    const idx = typeof patch.plot.index === 'number'
      ? patch.plot.index
      : (typeof patch.plot_index === 'number' ? patch.plot_index : -1)
    if (idx >= 0) {
      if (!Array.isArray(state.plots)) state.plots = []
      while (state.plots.length <= idx) {
        state.plots.push(makePlot(state.plots.length))
      }
      const local = state.plots[idx]
      if (!local.id && local.id !== 0) local.id = idx
      applyPlotSnapshot(local, patch.plot)
    }
  }
}

/**
 * 应用服务端 FarmDelta。Delta 只含发生变化的地块，不覆盖本地其余镜像。
 * @param {object} state
 * @param {{ plots?: object[] }} delta
 * @returns {object} state
 */
export function applyFarmDelta(state, delta) {
  if (!state || !Array.isArray(delta?.plots)) return state
  applyFullSnapshot(state, { plots: delta.plots })
  return state
}

/**
 * 将服务端 snapshot 或动作 Rsp 补丁写入本地 state（原地修改并返回）。
 *
 * 可接受：
 * - FarmSnapshotJSON（含 plots[]）
 * - PatchJSON（含 plot / coin / bag）
 * - EnterFarm Rsp payload：`{ snapshot, farm_seq, ... }`
 * - 动作 Rsp payload：`{ farm_seq, patch }`
 * - FarmDelta：`{ owner_uid, farm_seq, plots[] }`
 *
 * @param {object} state
 * @param {object} source
 * @returns {object} state
 */
export function applyPatch(state, source) {
  if (!state || !source || typeof source !== 'object') return state

  if (source.snapshot && typeof source.snapshot === 'object') {
    applyFullSnapshot(state, source.snapshot)
    return state
  }
  if (source.patch && typeof source.patch === 'object') {
    applyActionPatch(state, source.patch)
    return state
  }
  if (Array.isArray(source.plots)) {
    applyFarmDelta(state, source)
    return state
  }
  // 单地块 / 商店补丁（无外层包装）
  if (source.plot || source.bag || source.warehouse || typeof source.coin === 'number') {
    applyActionPatch(state, source)
  }
  return state
}
