// 地块视觉信息计算 —— 从 main.js 抽出的纯函数，便于单测。
// 规则对照 docs/design/game-design-full.md 5.1 / 6.4。
import { CROP_MAP, EXPANSION, stageCount } from './config.js';
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
