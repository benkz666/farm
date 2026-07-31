// 地块视觉信息计算 —— 从 main.js 抽出的纯函数，便于单测。
// 规则对照 docs/design/game-design-full.md 5.1 / 6.4。
import { CROP_MAP, EXPANSION, W_DRY, W_WEED, W_PEST, stageCount } from './config.js';
import { PLOT } from './state.js';

/** 由 plot.cropId 取作物定义；crop_id 被服务端清空时返回 undefined。 */
export const cropOf = (plot) => CROP_MAP[plot.cropId];

/**
 * 生长阶段（6.4）。仅对 GROWING/MATURE 有意义；调用方需保证 plot 存在有效 crop。
 * @returns {{ stage: number, total: number }}
 */
export function stageOf(plot, now) {
  const crop = cropOf(plot);
  const total = stageCount(crop);
  const progress = Math.max(0, Math.min(0.9999, (now - plot.plantTime) / plot.seasonMs));
  return { stage: Math.floor(progress * total), total };
}

/**
 * 基于服务端最近一次健康度结算点，插值当前仍在持续的不良状态影响。
 * 新生成的草/虫仍以服务端风险窗口裁决为准；到窗口边界会由同步调度器刷新。
 */
export function projectedHealthOf(plot, now) {
  const authoritative = Number.isFinite(Number(plot?.health))
    ? Number(plot.health)
    : 100 - (Number(plot?.penalty) || 0);
  const base = Math.max(0, Math.min(100, authoritative));
  if (!plot || plot.state !== PLOT.GROWING || plot.seasonMs <= 0) return base;

  const from = Number(plot.settleTime) || 0;
  const matureAt = Number(plot.matureTime) || 0;
  const to = matureAt > 0 ? Math.min(Number(now) || 0, matureAt) : Number(now) || 0;
  if (from <= 0 || to <= from) return base;

  const activeMs = (since) => {
    const start = Math.max(from, Number(since) || 0);
    return start > 0 && start < to ? to - start : 0;
  };
  const dryMs = activeMs(plot.waterUntil);
  const weedMs = plot.weedSince ? activeMs(plot.weedSince) : 0;
  const pestMs = plot.pestSince ? activeMs(plot.pestSince) : 0;
  const extraPenalty = 100 * (
    W_DRY * dryMs +
    W_WEED * weedMs +
    W_PEST * pestMs
  ) / plot.seasonMs;
  return Math.max(0, Math.min(100, base - extraPenalty));
}

/**
 * 计算 syncAllPlots 喂给 scene.updatePlot 的 info 对象。
 * @param {object} plot
 * @param {{ unlocked: boolean, index: number, now: number }} ctx
 */
export function computePlotInfo(plot, { unlocked, index, now }) {
  if (!plot) return { unlocked: false, lockText: '', state: PLOT.WASTELAND };
  const info = {
    unlocked,
    lockText: '',
    state: plot.state,
    cropDef: null,
    stage: 0,
    totalStages: 3,
    dry: false,
    weed: false,
    pest: false,
  };
  if (!unlocked) {
    const expDef = EXPANSION.find((x) => x[0] === index + 1);
    info.lockText = expDef ? `Lv.${expDef[1]}` : '';
    return info;
  }
  if (plot.state === PLOT.GROWING || plot.state === PLOT.MATURE) {
    const crop = cropOf(plot);
    const { stage, total } = stageOf(plot, now);
    info.cropDef = crop;
    info.stage = stage;
    info.totalStages = total;
    info.dry = plot.state === PLOT.GROWING && now > plot.waterUntil;
    info.weed = !!plot.weedSince && plot.state === PLOT.GROWING;
    info.pest = !!plot.pestSince && plot.state === PLOT.GROWING;
  } else if (plot.state === PLOT.WITHERED) {
    // 服务端使地块枯萎时会清空 crop_id（crop_id=0 → applyPatch 映射为 null）。
    // 此时无作物定义：不调用 stageOf（会因 stageCount(undefined) 崩溃），
    // 也不伪造具体作物；交由 scene.updatePlot 渲染为通用枯萎残株。
    info.cropDef = cropOf(plot) ?? null;
    info.stage = 0;
    info.totalStages = 3;
  }
  return info;
}
