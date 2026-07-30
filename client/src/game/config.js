// ============================================================
// 游戏配置 —— 数值严格对照 docs/design/game-design-full.md 第 18 章
// ============================================================

// 时间档（3.2 节）：1 缩放小时对应的真实毫秒数
export const TIME_SCALES = {
  demo:      { scale: 1 / 600,  label: '演示档 (1h=6s)',   hourMs: 6000 },
  fast:      { scale: 1 / 60,   label: '快速档 (1h=1min)', hourMs: 60000 },
  authentic: { scale: 1,        label: '真实档 (1h=1h)',   hourMs: 3600000 },
};

// 逻辑日真实时长下限 5 分钟（3.4 节）
export const LOGIC_DAY_MIN_MS = 5 * 60 * 1000;
export const logicDayMs = (ts) => Math.max(24 * TIME_SCALES[ts].hourMs, LOGIC_DAY_MIN_MS);

/** 当前时刻所属逻辑日起点，与服务端 LogicDayID 同口径：floor(now / dayMs) * dayMs。 */
export function logicDayStart(nowMs, timeScaleKey) {
  const dayMs = logicDayMs(timeScaleKey);
  if (!Number.isFinite(nowMs) || nowMs <= 0 || !dayMs) return 0;
  return Math.floor(nowMs / dayMs) * dayMs;
}

/**
 * 全局昼夜相位 [0, 1)：所有客户端用同一墙钟（或校准后的 serverNow）应得到同一值。
 * 不依赖本机会话的 dayStart 偏移。
 */
export function logicDayPhase(nowMs, timeScaleKey) {
  const dayMs = logicDayMs(timeScaleKey);
  if (!Number.isFinite(nowMs) || !dayMs) return 0;
  const rem = ((nowMs % dayMs) + dayMs) % dayMs;
  return rem / dayMs;
}

// ---- 等级与土地（4.x）----
export const EXP_PER_LEVEL = 200;                 // 累计经验 = N × 200
export const INITIAL_GOLD = 1000;
export const INITIAL_PLOTS = 6;
export const MAX_PLOTS = 18;

// 扩地链（4.5 节）：[开垦后总数, 等级要求, 金币]
export const EXPANSION = [
  [7, 5, 10000], [8, 7, 20000], [9, 9, 30000], [10, 11, 50000],
  [11, 13, 70000], [12, 15, 90000], [13, 17, 120000], [14, 19, 150000],
  [15, 21, 180000], [16, 23, 230000], [17, 25, 300000], [18, 27, 500000],
];

// ---- 动作经验（18.2）----
export const EXP = { till: 3, plant: 2, care: 2 };
export const DAILY_CARE_CAP = 150;   // 维护动作计经验上限 / 逻辑日
export const FRIEND_CARE_GOLD = 5;   // 好友农场除草除虫金币

// ---- 健康度与照料（18.3）----
export const W_DRY = 0.44, W_WEED = 0.26, W_PEST = 0.30;
export const YIELD_FLOOR = 0.60;            // 产量系数下限
export const WATER_SPAN = 0.35;             // 水分持续 = 本季 × 35%
export const RISK_WINDOW = 0.10;            // 风险窗口 = 本季 × 10%
export const WEED_CHANCE = 0.12;            // 单窗口长草概率
export const PEST_CHANCE = 0.10;            // 单窗口生虫概率
export const WITHER_SPAN = 3.0;             // 枯萎时限 = 3 × 本季时长

// ---- 偷菜与守卫（18.4）----
export const STEAL_CAP_RATIO = 0.40;        // 被偷上限 = 实际产量 40%
export const STEAL_MIN = 1, STEAL_MAX = 10; // 单次偷取 1-10 随机
export const DOG_BOWL_CAP = 120;            // 狗盆容量 g
export const DOG_FOOD_PRICE = 1;            // 狗粮 1 金币/g
export const CATCH_PENALTY_MULT = 10;       // 被抓赔付 = 果实单价 × 10
export const DOG_MAX_LEVEL = 5;
export const DOG_CATCHES_PER_LEVEL = 20;

export const DOGS = [
  { id: 'tugou',   name: '土狗',   unlock: 0,  intercept: 0.25, consumption: 4, price: 2000, color: 0xb08968, shopItemId: 2001, dogType: 1 },
  { id: 'muyang',  name: '牧羊犬', unlock: 10, intercept: 0.35, consumption: 5, price: 4500, color: 0x8d99ae, shopItemId: 0, dogType: 0 },
  { id: 'zangao',  name: '藏獒',   unlock: 20, intercept: 0.45, consumption: 7, price: 8000, color: 0x4a3728, shopItemId: 0, dogType: 0 },
];

/** 狗粮商店 item_id（按克购买）。 */
export const DOG_FOOD_SHOP_ITEM_ID = 2000;

// ---- 化肥（18.5）----
export const FERTILIZERS = [
  { id: 'normal', name: '普通化肥', shopItemId: 1001, reduceH: 1.0, price: 50,  icon: '🧂' },
  { id: 'fast',   name: '高速化肥', shopItemId: 1002, reduceH: 2.5, price: 200, icon: '⚡' },
  { id: 'super',  name: '急速化肥', shopItemId: 1003, reduceH: 5.5, price: 500, icon: '🚀' },
];

// ---- 隐藏种子（6.5 / 18.6）----
export const HIDDEN_DROP_CHANCE = 0.03;     // 每次锄地/清理 3%

// ---- 图鉴里程碑（16 章）：[条数, 金币] ----
export const CODEX_MILESTONES = [[8, 1000], [15, 3000], [22, 8000], [29, 20000]];

// ---- 日常任务池（14.1 节）----
export const TASK_POOL = [
  { id: 'water',    name: '浇水 10 次',     type: 'water',      target: 10, gold: 200, exp: 20 },
  { id: 'harvest',  name: '收获 5 次',      type: 'harvest',    target: 5,  gold: 300, exp: 30 },
  { id: 'help',     name: '帮好友照料 5 次', type: 'help',       target: 5,  gold: 250, exp: 25 },
  { id: 'steal',    name: '偷菜成功 3 次',   type: 'steal',      target: 3,  gold: 200, exp: 15 },
  { id: 'plant',    name: '播种 6 次',      type: 'plant',      target: 6,  gold: 200, exp: 20 },
  { id: 'sell',     name: '出售果实 1 次',   type: 'sell',       target: 1,  gold: 150, exp: 10 },
  { id: 'fertilize',name: '施肥 1 次',      type: 'fertilize',  target: 1,  gold: 100, exp: 10 },
];
export const DAILY_TASK_COUNT = 3;

// ============================================================
// 作物表 —— 由 config/crops.csv 经 make gen 生成，此处仅重导出以保持 import 兼容
// ============================================================
export { CROPS } from './gen/crops.js';
import { CROPS } from './gen/crops.js';

export const CROP_MAP = Object.fromEntries(CROPS.map(c => [c.id, c]));

// 多季拆分规则（6.3）：权威单位为整数分钟；后续每季 = cycleMinutes/(seasons+1)，首季 = 2 倍。
export function seasonMinutes(crop, seasonIndex) {
  if (crop.seasons <= 1) return crop.cycleMinutes;
  const later = crop.cycleMinutes / (crop.seasons + 1);
  return seasonIndex === 0 ? later * 2 : later;
}

/** @deprecated 兼容旧名；内部转调 seasonMinutes（可能含分数小时，勿用于权威时长）。 */
export function seasonHours(crop, seasonIndex) {
  return seasonMinutes(crop, seasonIndex) / 60;
}

/** 本季时长真实毫秒；与服务端 SeasonDurationMs 同公式：minutes * hourMs / 60。 */
export function seasonDurationMs(crop, seasonIndex, timeScaleKey = 'demo') {
  const hourMs = TIME_SCALES[timeScaleKey]?.hourMs;
  if (!hourMs || hourMs % 60 !== 0) return 0;
  return (seasonMinutes(crop, seasonIndex) * hourMs) / 60;
}

// 生长阶段数（6.4）：解锁 <3 级 3 阶段，≥3 级 4 阶段
export function stageCount(crop) {
  return crop.unlock < 3 ? 3 : 4;
}

export const STAGE_NAMES_3 = ['发芽', '小叶', '大叶'];
export const STAGE_NAMES_4 = ['发芽', '小叶', '大叶', '开花'];

// 升级奖励金币（15.1 升级奖励，设计值）
export const levelUpGold = (level) => level * 100;

// 好友数上限
export const FRIEND_CAP = 200;
