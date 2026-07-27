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
// 作物表（6.2 / 6.3 / 6.5 节）
// body 为 3D 体型类别，colors 用于模型与 UI 徽章
// ============================================================
export const CROPS = [
  // ---- 单季 ----
  { id: 'bailuobo', name: '白萝卜', unlock: 0,  seedPrice: 125, seasons: 1, cycleH: 10, yield: 16, fruitPrice: 17, harvestExp: 15, body: 'root',    color: '#f5f0e1', fruit: 0xf5f0e1, leaf: 0x5aa54a },
  { id: 'huluobo',  name: '胡萝卜', unlock: 0,  seedPrice: 163, seasons: 1, cycleH: 13, yield: 17, fruitPrice: 21, harvestExp: 18, body: 'root',    color: '#ff8c42', fruit: 0xff8c42, leaf: 0x4e9b47 },
  { id: 'dabaicai', name: '大白菜', unlock: 1,  seedPrice: 168, seasons: 1, cycleH: 14, yield: 17, fruitPrice: 22, harvestExp: 19, body: 'cabbage', color: '#a8d5a2', fruit: 0xcdeac0, leaf: 0x6fbf63 },
  { id: 'xiaomai',  name: '小麦',   unlock: 2,  seedPrice: 168, seasons: 1, cycleH: 14, yield: 18, fruitPrice: 21, harvestExp: 19, body: 'cereal',  color: '#e8c96a', fruit: 0xe8c96a, leaf: 0x9db85c },
  { id: 'shuidao',  name: '水稻',   unlock: 2,  seedPrice: 168, seasons: 1, cycleH: 14, yield: 18, fruitPrice: 21, harvestExp: 19, body: 'cereal',  color: '#d9c25e', fruit: 0xd9c25e, leaf: 0x7fb069 },
  { id: 'yumi',     name: '玉米',   unlock: 3,  seedPrice: 175, seasons: 1, cycleH: 14, yield: 17, fruitPrice: 23, harvestExp: 19, body: 'corn',    color: '#ffd54a', fruit: 0xffd54a, leaf: 0x5da352 },
  { id: 'tudou',    name: '土豆',   unlock: 4,  seedPrice: 188, seasons: 1, cycleH: 15, yield: 18, fruitPrice: 24, harvestExp: 20, body: 'bush',    color: '#d9b382', fruit: 0xd9b382, leaf: 0x5aa54a },
  { id: 'hongzao',  name: '红枣',   unlock: 5,  seedPrice: 237, seasons: 1, cycleH: 16, yield: 20, fruitPrice: 25, harvestExp: 21, body: 'tree',    color: '#c0392b', fruit: 0xc0392b, leaf: 0x4e8f43 },
  { id: 'qiezi',    name: '茄子',   unlock: 5,  seedPrice: 237, seasons: 1, cycleH: 16, yield: 20, fruitPrice: 25, harvestExp: 21, body: 'bush',    color: '#8e44ad', fruit: 0x8e44ad, leaf: 0x5aa54a },
  { id: 'fanqie',   name: '番茄',   unlock: 6,  seedPrice: 251, seasons: 1, cycleH: 17, yield: 21, fruitPrice: 26, harvestExp: 22, body: 'bush',    color: '#e74c3c', fruit: 0xe74c3c, leaf: 0x55a04a },
  { id: 'wandou',   name: '豌豆',   unlock: 7,  seedPrice: 266, seasons: 1, cycleH: 18, yield: 22, fruitPrice: 27, harvestExp: 23, body: 'bush',    color: '#7dc95e', fruit: 0x7dc95e, leaf: 0x4e9b47 },
  { id: 'hongmeigui', name: '红玫瑰', unlock: 7, seedPrice: 266, seasons: 1, cycleH: 18, yield: 22, fruitPrice: 27, harvestExp: 23, body: 'rose',   color: '#e5386d', fruit: 0xe5386d, leaf: 0x4a8f43 },
  { id: 'lajiao',   name: '辣椒',   unlock: 8,  seedPrice: 296, seasons: 1, cycleH: 20, yield: 24, fruitPrice: 28, harvestExp: 25, body: 'bush',    color: '#d7263d', fruit: 0xd7263d, leaf: 0x55a04a },
  { id: 'nangua',   name: '南瓜',   unlock: 9,  seedPrice: 325, seasons: 1, cycleH: 22, yield: 25, fruitPrice: 30, harvestExp: 27, body: 'ground',  color: '#ff9f1c', fruit: 0xff9f1c, leaf: 0x5aa54a },
  // ---- 多季 ----
  { id: 'pingguo',  name: '苹果',   unlock: 10, seedPrice: 578,  seasons: 2, cycleH: 30,  yield: 23, fruitPrice: 24, harvestExp: 18, body: 'tree',  color: '#e63946', fruit: 0xe63946, leaf: 0x4e8f43 },
  { id: 'caomei',   name: '草莓',   unlock: 10, seedPrice: 605,  seasons: 2, cycleH: 35,  yield: 24, fruitPrice: 27, harvestExp: 20, body: 'low',   color: '#ff4d6d', fruit: 0xff4d6d, leaf: 0x5aa54a },
  { id: 'xigua',    name: '西瓜',   unlock: 11, seedPrice: 708,  seasons: 2, cycleH: 41,  yield: 27, fruitPrice: 29, harvestExp: 23, body: 'ground',color: '#2e933c', fruit: 0x3d9970, leaf: 0x5aa54a },
  { id: 'xiangjiao',name: '香蕉',   unlock: 12, seedPrice: 900,  seasons: 2, cycleH: 45,  yield: 29, fruitPrice: 32, harvestExp: 25, body: 'palm',  color: '#ffe066', fruit: 0xffe066, leaf: 0x6fbf63 },
  { id: 'taozi',    name: '桃子',   unlock: 13, seedPrice: 1200, seasons: 2, cycleH: 60,  yield: 32, fruitPrice: 40, harvestExp: 33, body: 'tree',  color: '#ffb3c1', fruit: 0xffa8b8, leaf: 0x4e8f43 },
  { id: 'chengzi',  name: '橙子',   unlock: 14, seedPrice: 1587, seasons: 3, cycleH: 59,  yield: 26, fruitPrice: 41, harvestExp: 25, body: 'tree',  color: '#ff9f1c', fruit: 0xff9f1c, leaf: 0x4e8f43 },
  { id: 'putao',    name: '葡萄',   unlock: 15, seedPrice: 1978, seasons: 3, cycleH: 86,  yield: 29, fruitPrice: 47, harvestExp: 30, body: 'vine',  color: '#9b5de5', fruit: 0x9b5de5, leaf: 0x55a04a },
  { id: 'shiliu',   name: '石榴',   unlock: 16, seedPrice: 2425, seasons: 3, cycleH: 96,  yield: 30, fruitPrice: 54, harvestExp: 34, body: 'tree',  color: '#d90429', fruit: 0xd90429, leaf: 0x4e8f43 },
  { id: 'youzi',    name: '柚子',   unlock: 17, seedPrice: 2855, seasons: 3, cycleH: 113, yield: 33, fruitPrice: 58, harvestExp: 39, body: 'tree',  color: '#f4d35e', fruit: 0xf4d35e, leaf: 0x4e8f43 },
  { id: 'boluo',    name: '菠萝',   unlock: 18, seedPrice: 3480, seasons: 3, cycleH: 116, yield: 35, fruitPrice: 62, harvestExp: 40, body: 'pineapple', color: '#f6bd60', fruit: 0xf6bd60, leaf: 0x6fbf63 },
  { id: 'yezi',     name: '椰子',   unlock: 19, seedPrice: 3720, seasons: 4, cycleH: 124, yield: 27, fruitPrice: 65, harvestExp: 32, body: 'palm',  color: '#a0785a', fruit: 0x8d6e63, leaf: 0x5da352 },
  { id: 'hulu',     name: '葫芦',   unlock: 20, seedPrice: 4742, seasons: 4, cycleH: 139, yield: 30, fruitPrice: 71, harvestExp: 36, body: 'vine',  color: '#a7c957', fruit: 0xa7c957, leaf: 0x55a04a },
  // ---- 隐藏作物（种子价 0，不掉落于商店）----
  { id: 'renshen',  name: '人参',   unlock: 0,  seedPrice: 0, seasons: 1, cycleH: 40,  yield: 22, fruitPrice: 41, harvestExp: 60,  body: 'root',   color: '#e8d8b9', fruit: 0xe8d8b9, leaf: 0x6a994e, hidden: true, dropLevel: 0 },
  { id: 'lingzhi',  name: '灵芝',   unlock: 10, seedPrice: 0, seasons: 1, cycleH: 60,  yield: 30, fruitPrice: 60, harvestExp: 80,  body: 'fungus',color: '#9c6644', fruit: 0x9c6644, leaf: 0x7f5539, hidden: true, dropLevel: 10 },
  { id: 'yaoqianshu', name: '摇钱树', unlock: 20, seedPrice: 0, seasons: 3, cycleH: 100, yield: 25, fruitPrice: 55, harvestExp: 100, body: 'money', color: '#ffd700', fruit: 0xffd700, leaf: 0xd4af37, hidden: true, dropLevel: 20 },
];

export const CROP_MAP = Object.fromEntries(CROPS.map(c => [c.id, c]));

// 多季拆分规则（6.3）：后续每季 = 全周期/(季数+1)，首季 = 2 倍
export function seasonHours(crop, seasonIndex) {
  if (crop.seasons === 1) return crop.cycleH;
  const later = crop.cycleH / (crop.seasons + 1);
  return seasonIndex === 0 ? later * 2 : later;
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
