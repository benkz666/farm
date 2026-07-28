/* 全局配置：作物、等级、道具、地块与随机事件参数 */
window.Farm = window.Farm || {};

(function (Farm) {
  'use strict';

  const S = 1000; // 秒 -> 毫秒

  /**
   * 作物配置
   * shape 决定 SVG 画法：
   *   root    地下根茎（成熟时顶部露出果实）
   *   leaf    叶球类
   *   fruit   直立茎上挂果
   *   tall    高秆作物
   *   vine    地面藤蔓大果
   *   cluster 成串果实
   */
  const CROPS = [
    {
      id: 'radish', name: '白萝卜', shape: 'root', level: 1,
      seedPrice: 80, sellPrice: 42, yield: 3, exp: 8, growMs: 90 * S,
      leaf: '#5fbf5a', deep: '#3d9440', fruit: '#f7f3ea', accent: '#e8e1d0'
    },
    {
      id: 'cabbage', name: '大白菜', shape: 'leaf', level: 1,
      seedPrice: 110, sellPrice: 58, yield: 3, exp: 11, growMs: 130 * S,
      leaf: '#79c95c', deep: '#4d9b3f', fruit: '#c8e6a0', accent: '#eef7d6'
    },
    {
      id: 'carrot', name: '胡萝卜', shape: 'root', level: 2,
      seedPrice: 150, sellPrice: 76, yield: 3, exp: 15, growMs: 170 * S,
      leaf: '#66c65e', deep: '#3f9243', fruit: '#f5893a', accent: '#ffb066'
    },
    {
      id: 'potato', name: '土豆', shape: 'root', level: 3,
      seedPrice: 200, sellPrice: 98, yield: 3, exp: 20, growMs: 210 * S,
      leaf: '#5cb85c', deep: '#3c8c3f', fruit: '#c99a5b', accent: '#e2b97c'
    },
    {
      id: 'tomato', name: '番茄', shape: 'fruit', level: 4,
      seedPrice: 280, sellPrice: 132, yield: 3, exp: 27, growMs: 260 * S,
      leaf: '#57b45b', deep: '#38853d', fruit: '#e8402f', accent: '#ff7a63'
    },
    {
      id: 'corn', name: '玉米', shape: 'tall', level: 5,
      seedPrice: 360, sellPrice: 168, yield: 3, exp: 35, growMs: 320 * S,
      leaf: '#7bbd4a', deep: '#4f8f34', fruit: '#f7cf3f', accent: '#ffe680'
    },
    {
      id: 'eggplant', name: '茄子', shape: 'fruit', level: 6,
      seedPrice: 460, sellPrice: 212, yield: 3, exp: 44, growMs: 380 * S,
      leaf: '#5cae5c', deep: '#3a8340', fruit: '#7b46b5', accent: '#a273d8'
    },
    {
      id: 'strawberry', name: '草莓', shape: 'fruit', level: 7,
      seedPrice: 580, sellPrice: 268, yield: 3, exp: 55, growMs: 450 * S,
      leaf: '#63bd5c', deep: '#3f8f43', fruit: '#e83a5a', accent: '#ff7a90'
    },
    {
      id: 'sunflower', name: '向日葵', shape: 'tall', level: 8,
      seedPrice: 720, sellPrice: 330, yield: 3, exp: 68, growMs: 520 * S,
      leaf: '#6cb84f', deep: '#458a36', fruit: '#f9c92c', accent: '#7a4a1e'
    },
    {
      id: 'pumpkin', name: '南瓜', shape: 'vine', level: 10,
      seedPrice: 980, sellPrice: 452, yield: 3, exp: 92, growMs: 640 * S,
      leaf: '#63b552', deep: '#3f8a3a', fruit: '#f2872c', accent: '#ffab5c'
    },
    {
      id: 'watermelon', name: '西瓜', shape: 'vine', level: 12,
      seedPrice: 1320, sellPrice: 610, yield: 3, exp: 124, growMs: 780 * S,
      leaf: '#5bb058', deep: '#3b8340', fruit: '#2f9e4f', accent: '#8fd48a'
    },
    {
      id: 'grape', name: '葡萄', shape: 'cluster', level: 14,
      seedPrice: 1780, sellPrice: 820, yield: 3, exp: 168, growMs: 900 * S,
      leaf: '#66b060', deep: '#3f8542', fruit: '#7b4fb8', accent: '#a880e0'
    },
    {
      id: 'ginseng', name: '人参', shape: 'root', level: 17,
      seedPrice: 2600, sellPrice: 1210, yield: 3, exp: 250, growMs: 1080 * S,
      leaf: '#4fa85a', deep: '#357f43', fruit: '#e8d9b0', accent: '#f2e8cc'
    },
    {
      id: 'goldtree', name: '摇钱树', shape: 'cluster', level: 20,
      seedPrice: 4200, sellPrice: 1980, yield: 3, exp: 400, growMs: 1320 * S,
      leaf: '#4fa15c', deep: '#337a42', fruit: '#f4c62a', accent: '#ffe071'
    }
  ];

  const CROP_MAP = {};
  CROPS.forEach(function (c) { CROP_MAP[c.id] = c; });

  /** 生长阶段：progress 为总生长进度占比的下限 */
  const STAGES = [
    { key: 'seed', name: '种子', at: 0 },
    { key: 'sprout', name: '发芽', at: 0.16 },
    { key: 'small', name: '幼苗', at: 0.36 },
    { key: 'grown', name: '成株', at: 0.58 },
    { key: 'flower', name: '开花', at: 0.8 },
    { key: 'ripe', name: '成熟', at: 1 }
  ];

  /** 升到下一级所需经验（索引 0 表示 1 级 -> 2 级） */
  const LEVEL_EXP = [];
  for (let i = 0; i < 40; i++) {
    LEVEL_EXP.push(Math.round(45 * Math.pow(1.3, i) / 5) * 5);
  }

  const RULES = {
    tickMs: 1000,
    plotCount: 24,          // 农场总格数（4 行 x 6 列）
    initialUnlocked: 6,     // 初始已开垦地块
    initialCoins: 1200,
    waterDrainPerMin: 22,   // 每分钟水分流失
    dryGrowthFactor: 0.35,  // 缺水时生长速度系数
    weedGrowthFactor: 0.6,  // 有杂草时生长速度系数
    pestGrowthFactor: 0.7,  // 有害虫时生长速度系数
    weedChancePerMin: 0.16, // 每分钟长草概率
    pestChancePerMin: 0.11, // 每分钟生虫概率
    pestYieldPenalty: 1,    // 成熟时仍有虫，减产数量
    witherAfterRipeMs: 45 * 60 * S, // 成熟后未收获，超时枯萎
    fertilizerPrice: 400,
    fertilizerBoost: 0.28,  // 一次化肥推进的生长进度
    stealCooldownMs: 5 * 60 * S,
    stealCaughtChance: 0.12,
    stealFine: 60,
    stealExp: 6,
    exp: { till: 1, plant: 2, water: 1, weed: 2, pest: 3, steal: 6 }
  };

  /** 地块解锁价格与等级要求 */
  function plotUnlockCost(index) {
    const n = index - RULES.initialUnlocked; // 第 0 块待解锁地
    return Math.round(600 * Math.pow(1.42, n) / 10) * 10;
  }
  function plotUnlockLevel(index) {
    const n = index - RULES.initialUnlocked;
    return 2 + Math.floor(n / 2);
  }

  const NEIGHBOR_NAMES = [
    { name: '隔壁老王', avatar: '#f2a54b' },
    { name: '菜园小美', avatar: '#ef7fa5' },
    { name: '种田大侠', avatar: '#5aa9e6' },
    { name: '摸鱼阿呆', avatar: '#8bc34a' }
  ];

  Farm.config = {
    CROPS: CROPS,
    CROP_MAP: CROP_MAP,
    STAGES: STAGES,
    LEVEL_EXP: LEVEL_EXP,
    RULES: RULES,
    NEIGHBOR_NAMES: NEIGHBOR_NAMES,
    plotUnlockCost: plotUnlockCost,
    plotUnlockLevel: plotUnlockLevel
  };
})(window.Farm);
