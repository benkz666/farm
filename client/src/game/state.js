// ============================================================
// 游戏状态：创建、派生计算、本地存档
// ============================================================
import { INITIAL_GOLD, INITIAL_PLOTS, MAX_PLOTS, EXP_PER_LEVEL, TASK_POOL, DAILY_TASK_COUNT, DOG_BOWL_CAP } from './config.js';

// 地块状态机（5.1 节）
export const PLOT = { WASTELAND: 'wasteland', TILLED: 'tilled', GROWING: 'growing', MATURE: 'mature', RESIDUE: 'residue', WITHERED: 'withered' };

export function makePlot(id) {
  return {
    id,
    state: PLOT.WASTELAND,
    cropId: null,
    season: 0,            // 当前季（0 起）
    plantTime: 0,         // 本季开始时刻
    matureTime: 0,        // 本季成熟时刻
    seasonMs: 0,          // 本季生长时长（已缩放）
    penalty: 0,           // 本季累计健康度扣减
    settleTime: 0,        // 上次结算时刻
    waterUntil: 0,        // 水分充足截止时刻
    weedSince: 0,         // 杂草存在起始时刻（0 = 无草）
    pestSince: 0,         // 害虫存在起始时刻（0 = 无虫）
    nextRiskTime: 0,      // 下一次风险窗口判定时刻
    fertilizedStages: [], // 已施肥的生长阶段序号
    stolenTotal: 0,       // 本轮成熟被偷总量
    stolenBy: [],         // 本轮已偷过的访客 id
    stealRound: 0,        // 成熟轮次
  };
}

// NPC 好友（单机模拟，替代 11 章的分享链接加好友）
const NPC_DEFS = [
  { id: 'npc1', name: '小芳', level: 2,  plots: 6,  dog: null },
  { id: 'npc2', name: '阿强', level: 6,  plots: 8,  dog: 'tugou' },
  { id: 'npc3', name: '丽丽', level: 12, plots: 10, dog: 'muyang' },
  { id: 'npc4', name: '老王', level: 18, plots: 12, dog: null },
];

function makeNpcFarm(def) {
  return {
    ...def,
    plots: Array.from({ length: def.plots }, (_, i) => makePlot(i)),
    dogBowl: def.dog ? DOG_BOWL_CAP : 0,
    nextActionTime: 0,    // NPC 下一次自主操作时刻
  };
}

export function defaultState() {
  const now = Date.now();
  return {
    version: 1,
    createdAt: now,
    timeScale: 'demo',
    gold: INITIAL_GOLD,
    exp: 0,
    plots: Array.from({ length: MAX_PLOTS }, (_, i) => makePlot(i)),
    unlockedPlots: INITIAL_PLOTS,
    inventory: { seeds: {}, fertilizers: { normal: 0, fast: 0, super: 0 } },
    warehouse: {},        // cropId -> 果实数
    dog: null,            // { id, level, catches }
    dogBowl: 0,
    codex: [],            // 已解锁作物 id
    codexMilestones: [],  // 已发放的里程碑条数
    mails: [],
    mailSeq: 1,
    friends: NPC_DEFS.map(makeNpcFarm),
    tasks: [],            // [{taskId, progress, done}]
    daily: { dayStart: now - 2 * 60 * 1000, careCount: 0 },  // 初始落在上午时段
    stats: { tilled: 0, planted: 0, watered: 0, weeded: 0, depested: 0, harvested: 0, stolen: 0, helped: 0, fertilized: 0, sold: 0, caught: 0 },
    seenUnlockTip: 0,     // 上次提示扩地时的等级
    settings: { sound: true },
    lastSeen: now,
  };
}

// ---- 派生计算 ----
export const levelOf = (exp) => Math.floor(exp / EXP_PER_LEVEL);
export const expProgress = (exp) => (exp % EXP_PER_LEVEL) / EXP_PER_LEVEL;

export function drawDailyTasks(state) {
  const pool = [...TASK_POOL];
  const tasks = [];
  for (let i = 0; i < DAILY_TASK_COUNT; i++) {
    const idx = Math.floor(Math.random() * pool.length);
    const def = pool.splice(idx, 1)[0];
    tasks.push({ taskId: def.id, progress: 0, done: false });
  }
  state.tasks = tasks;
}

// ---- 存档 ----
const SAVE_KEY = 'farm3d_save_v1';

export function saveGame(state) {
  state.lastSeen = Date.now();
  try { localStorage.setItem(SAVE_KEY, JSON.stringify(state)); } catch (e) { /* 存储满时静默 */ }
}

export function loadGame() {
  try {
    const raw = localStorage.getItem(SAVE_KEY);
    if (!raw) return null;
    const s = JSON.parse(raw);
    if (!s || s.version !== 1) return null;
    return s;
  } catch (e) { return null; }
}

export function clearSave() {
  localStorage.removeItem(SAVE_KEY);
}
