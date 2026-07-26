// ============================================================
// QQ农场 3D —— 游戏主逻辑
// 规则实现严格对照 docs/design/game-design-full.md
// ============================================================
import {
  TIME_SCALES, CROP_MAP, CROPS, FERTILIZERS, DOGS, EXPANSION, TASK_POOL,
  EXP, DAILY_CARE_CAP, FRIEND_CARE_GOLD, W_DRY, W_WEED, W_PEST, YIELD_FLOOR,
  WATER_SPAN, RISK_WINDOW, WEED_CHANCE, PEST_CHANCE, WITHER_SPAN,
  STEAL_CAP_RATIO, STEAL_MIN, STEAL_MAX, DOG_BOWL_CAP, DOG_FOOD_PRICE,
  CATCH_PENALTY_MULT, DOG_MAX_LEVEL, DOG_CATCHES_PER_LEVEL,
  HIDDEN_DROP_CHANCE, CODEX_MILESTONES, MAX_PLOTS, seasonHours, stageCount,
  STAGE_NAMES_3, STAGE_NAMES_4, levelUpGold, logicDayMs,
} from './config.js';
import { PLOT, defaultState, saveGame, loadGame, clearSave, levelOf, drawDailyTasks } from './state.js';
import { FarmScene } from './farm3d.js';
import { UI, badgeHTML, fmtTime } from './ui.js';
import { SFX } from './audio.js';
import { enterOnline, isOnline } from '../net/session.js';
import { CMD_FERTILIZE, CMD_PLANT } from '../net/client.js';
import { errText } from '../net/errors.js';
import { applyPatch, cropKeyToId } from './applyPatch.js';
import { plotCmdForTool } from './onlineActions.js';
import { shouldApplyPatchFromError } from './onlineResponse.js';

// ---------------- 初始化 ----------------
let state = loadGame() || defaultState();
if (!state.tasks.length) drawDailyTasks(state);
state.settings = state.settings || { sound: true };

const sfx = new SFX();
sfx.enabled = state.settings.sound;

const scene = new FarmScene(document.getElementById('scene-container'));

let viewing = 'me';            // 'me' | friendId
let activeTool = null;
let selectedSeed = null;
let selectedFert = null;
let hoverPlot = null;

/** @type {import('../net/client.js').NetClient|null} */
let netClient = null;
/** 等 Rsp 期间防连点 */
let onlineBusy = false;

const hourMs = () => TIME_SCALES[state.timeScale].hourMs;
const myLevel = () => levelOf(state.exp);
const fertilizerKeyToId = (key) => ({ normal: 1, fast: 2, super: 3 }[key] || 0);

const TOOLS_HOME = [
  { id: 'till', name: '锄地', icon: '⛏️' },
  { id: 'plant', name: '播种', icon: '🌱' },
  { id: 'water', name: '浇水', icon: '💧' },
  { id: 'weed', name: '除草', icon: '🌿' },
  { id: 'pest', name: '除虫', icon: '🐛' },
  { id: 'fert', name: '施肥', icon: '🧪' },
  { id: 'harvest', name: '收获', icon: '🧺' },
];
const TOOLS_VISIT = [
  { id: 'water', name: '浇水', icon: '💧' },
  { id: 'weed', name: '除草', icon: '🌿' },
  { id: 'pest', name: '除虫', icon: '🐛' },
  { id: 'steal', name: '偷菜', icon: '🥷' },
];

// ---------------- 通用辅助 ----------------
const rand = Math.random;
const cropOf = (plot) => CROP_MAP[plot.cropId];

function currentFarm() {
  if (viewing === 'me') return { plots: state.plots, owner: 'me', unlocked: state.unlockedPlots, isMe: true };
  const f = state.friends.find(f => f.id === viewing);
  return { plots: f.plots, owner: f.id, unlocked: f.plots.length, isMe: false, friend: f };
}

// 健康度增量结算（7.5 节）
function settleHealth(plot, now) {
  if (plot.state !== PLOT.GROWING) return;
  const to = Math.min(now, plot.matureTime);
  const from = plot.settleTime;
  if (to <= from) return;
  let dry = 0, weed = 0, pest = 0;
  if (plot.waterUntil < to) dry = to - Math.max(from, plot.waterUntil);
  if (plot.weedSince) weed = to - Math.max(from, plot.weedSince);
  if (plot.pestSince) pest = to - Math.max(from, plot.pestSince);
  plot.penalty += 100 * (W_DRY * dry + W_WEED * weed + W_PEST * pest) / plot.seasonMs;
  plot.settleTime = to;
}

const healthOf = (plot) => Math.max(0, Math.min(100, 100 - plot.penalty));
const yieldFactor = (h) => YIELD_FLOOR + (1 - YIELD_FLOOR) * (h / 100);
const actualYield = (crop, h) => Math.floor(crop.yield * yieldFactor(h));

function stageOf(plot, now) {
  const crop = cropOf(plot);
  const total = stageCount(crop);
  const progress = Math.max(0, Math.min(0.9999, (now - plot.plantTime) / plot.seasonMs));
  return { stage: Math.floor(progress * total), total };
}

function stageName(crop, stage) {
  return (stageCount(crop) === 3 ? STAGE_NAMES_3 : STAGE_NAMES_4)[stage] || '';
}

// ---------------- 邮件 ----------------
function addMail(mail) {
  state.mails.push({ id: state.mailSeq++, time: Date.now(), read: false, claimed: false, gold: 0, exp: 0, ...mail });
  while (state.mails.length > 100) {
    const idx = state.mails.findIndex(m => m.read && !(m.gold || m.exp) || (m.claimed && m.read));
    if (idx === -1) break;
    state.mails.splice(idx, 1);
  }
  ui.updateHUD(state);
}

// ---------------- 经验 / 金币 ----------------
function addExp(n, silent = false) {
  if (n <= 0) return;
  const before = myLevel();
  state.exp += n;
  const after = myLevel();
  if (after > before) {
    sfx.levelup();
    ui.toast(`🎉 升到 Lv.${after}！新作物与土地已解锁`, 'gold');
    addMail({ title: '升级奖励', content: `恭喜达到 Lv.${after}，系统奖励金币 ${levelUpGold(after)}。`, gold: levelUpGold(after) });
  } else if (!silent) {
    ui.toast(`+${n} 经验`, 'info');
  }
}

function addGold(n) {
  state.gold += n;
  ui.updateHUD(state);
}

// ---------------- 任务跟踪 ----------------
function trackEvent(type, count = 1) {
  for (const t of state.tasks) {
    const def = TASK_POOL.find(d => d.id === t.taskId);
    if (def.type !== type || t.done) continue;
    t.progress += count;
    if (t.progress >= def.target) {
      t.done = true;
      sfx.task();
      ui.toast(`📋 任务「${def.name}」完成，奖励已发送至邮箱`, 'gold');
      addMail({ title: '任务奖励', content: `完成日常任务「${def.name}」。`, gold: def.gold, exp: def.exp });
    }
  }
  ui.updateHUD(state);
}

// ---------------- 图鉴 ----------------
function unlockCodex(cropId) {
  if (state.codex.includes(cropId)) return;
  state.codex.push(cropId);
  const crop = CROP_MAP[cropId];
  ui.toast(`📖 图鉴解锁：${crop.name}`, 'gold');
  for (const [need, gold] of CODEX_MILESTONES) {
    if (state.codex.length >= need && !state.codexMilestones.includes(need)) {
      state.codexMilestones.push(need);
      addMail({ title: '图鉴里程碑', content: `收集度达到 ${need} 种，奖励金币 ${gold}。`, gold });
    }
  }
}

// ---------------- 逻辑日 ----------------
function checkLogicDay(now) {
  const dayMs = logicDayMs(state.timeScale);
  let guard = 0;
  while (now - state.daily.dayStart >= dayMs && guard++ < 100) {
    state.daily.dayStart += dayMs;
    state.daily.careCount = 0;
    drawDailyTasks(state);
    addMail({ title: '新的一天', content: '日常任务已刷新，快去查看吧。' });
  }
}

// ---------------- 地块推进（玩家与 NPC 共用） ----------------
function tickPlots(farm, now) {
  for (const plot of farm.plots) {
    if (plot.state === PLOT.GROWING) {
      // 风险窗口判定（7.2 节）
      const windowMs = plot.seasonMs * RISK_WINDOW;
      let guard = 0;
      while (plot.nextRiskTime <= now && plot.nextRiskTime < plot.matureTime && guard++ < 1000) {
        if (!plot.weedSince && rand() < WEED_CHANCE) plot.weedSince = plot.nextRiskTime;
        if (!plot.pestSince && rand() < PEST_CHANCE) plot.pestSince = plot.nextRiskTime;
        plot.nextRiskTime += windowMs;
      }
      settleHealth(plot, now);
      if (now >= plot.matureTime) {
        plot.state = PLOT.MATURE;
        plot.stolenTotal = 0;
        plot.stolenBy = [];
        plot.stealRound++;
        if (farm.isMe) ui.toast(`✨ ${cropOf(plot).name} 成熟了！`, 'gold');
      }
    } else if (plot.state === PLOT.MATURE) {
      if (now > plot.matureTime + plot.seasonMs * WITHER_SPAN) {
        plot.state = PLOT.WITHERED;
        if (farm.isMe) ui.toast(`🥀 ${cropOf(plot).name} 枯萎了，产量全失…`, 'err');
      }
    }
  }
}

// ---------------- 农事动作 ----------------
function fail(msg) { sfx.error(); ui.toast(msg, 'err'); }
function ok(msg, type = 'ok') { ui.toast(msg, type); }

function careExp() {  // 维护动作经验（每日 150 次上限，4.4 节）
  if (state.daily.careCount < DAILY_CARE_CAP) {
    state.daily.careCount++;
    return EXP.care;
  }
  return 0;
}

function doTill(plot, now) {
  const fromState = plot.state;
  if (fromState !== PLOT.WASTELAND && fromState !== PLOT.RESIDUE && fromState !== PLOT.WITHERED) {
    return fail('这块地不需要锄地');
  }
  plot.state = PLOT.TILLED;
  Object.assign(plot, { cropId: null, season: 0, penalty: 0, weedSince: 0, pestSince: 0, stolenTotal: 0, stolenBy: [] });
  sfx.till();
  scene.burst(plot.id, 0x8d6e63, 12, true);
  addExp(EXP.till, true);
  ok(fromState === PLOT.WASTELAND ? `锄地完成 +${EXP.till} 经验` : `清理完成 +${EXP.till} 经验`);
  // 3% 隐藏种子掉落（6.5 节）
  if (rand() < HIDDEN_DROP_CHANCE) {
    const pool = CROPS.filter(c => c.hidden && c.dropLevel <= myLevel());
    if (pool.length) {
      const drop = pool[Math.floor(rand() * pool.length)];
      state.inventory.seeds[drop.id] = (state.inventory.seeds[drop.id] || 0) + 1;
      sfx.mail();
      ui.toast(`✨ 意外发现隐藏种子：${drop.name}！已放入背包`, 'gold');
      scene.magicAnim(plot.id);
    }
  }
}

function doPlant(plot, now) {
  if (plot.state !== PLOT.TILLED) return fail('需要先锄地才能播种');
  const crop = CROP_MAP[selectedSeed];
  if (!crop) return fail('请先选择种子');
  if (!crop.hidden && myLevel() < crop.unlock) return fail(`需要 Lv.${crop.unlock} 解锁`);
  const count = state.inventory.seeds[crop.id] || 0;
  if (count <= 0) return fail('背包中没有该种子，请先去商店购买');
  state.inventory.seeds[crop.id]--;
  const seasonMs = seasonHours(crop, 0) * hourMs();
  Object.assign(plot, {
    state: PLOT.GROWING, cropId: crop.id, season: 0,
    plantTime: now, matureTime: now + seasonMs, seasonMs,
    penalty: 0, settleTime: now,
    waterUntil: now + seasonMs * WATER_SPAN,   // 播种视为已浇水（7.2 节）
    weedSince: 0, pestSince: 0,
    nextRiskTime: now + seasonMs * RISK_WINDOW,
    fertilizedStages: [], stolenTotal: 0, stolenBy: [],
  });
  sfx.plant();
  scene.burst(plot.id, 0x9be15d, 12, true);
  addExp(EXP.plant, true);
  ok(`播种 ${crop.name} +${EXP.plant} 经验`);
  trackEvent('plant');
  if ((state.inventory.seeds[crop.id] || 0) <= 0) refreshSubBar();
}

function doWater(plot, now, isFriend) {
  if (plot.state !== PLOT.GROWING) return fail('该地块没有生长中的作物');
  if (now < plot.waterUntil) return fail('水分本已充足，无需浇水');
  settleHealth(plot, now);
  plot.waterUntil = now + plot.seasonMs * WATER_SPAN;
  sfx.water();
  scene.waterAnim(plot.id);
  const exp = careExp();
  if (exp) addExp(exp, true);
  ok(exp ? `浇水完成 +${exp} 经验` : '浇水完成（今日维护经验已达上限）');
  trackEvent('water');
  if (isFriend) trackEvent('help');
}

function doWeed(plot, now, isFriend) {
  if (plot.state !== PLOT.GROWING) return fail('该地块没有生长中的作物');
  if (!plot.weedSince) return fail('该地块没有杂草');
  settleHealth(plot, now);
  plot.weedSince = 0;
  sfx.weed();
  scene.burst(plot.id, 0x81c784, 10, true);
  const exp = careExp();
  if (exp) addExp(exp, true);
  let msg = exp ? `除草完成 +${exp} 经验` : '除草完成';
  if (isFriend) {
    if (exp) { addGold(FRIEND_CARE_GOLD); msg += ` +${FRIEND_CARE_GOLD} 金币`; }
    trackEvent('help');
  }
  ok(msg);
}

function doPest(plot, now, isFriend) {
  if (plot.state !== PLOT.GROWING) return fail('该地块没有生长中的作物');
  if (!plot.pestSince) return fail('该地块没有害虫');
  settleHealth(plot, now);
  plot.pestSince = 0;
  sfx.pest();
  scene.burst(plot.id, 0xffb74d, 10, true);
  const exp = careExp();
  if (exp) addExp(exp, true);
  let msg = exp ? `除虫完成 +${exp} 经验` : '除虫完成';
  if (isFriend) {
    if (exp) { addGold(FRIEND_CARE_GOLD); msg += ` +${FRIEND_CARE_GOLD} 金币`; }
    trackEvent('help');
  }
  ok(msg);
}

function doFertilize(plot, now) {
  if (plot.state !== PLOT.GROWING) return fail('该地块没有生长中的作物');
  const fert = FERTILIZERS.find(f => f.id === selectedFert);
  if (!fert) return fail('请先选择化肥');
  if ((state.inventory.fertilizers[fert.id] || 0) <= 0) return fail('背包中没有该化肥');
  const { stage } = stageOf(plot, now);
  if (plot.fertilizedStages.includes(stage)) return fail('当前阶段已经施过肥了');
  const crop = cropOf(plot);
  const total = stageCount(crop);
  const stageEnd = plot.plantTime + plot.seasonMs * ((stage + 1) / total);
  const reduce = Math.min(fert.reduceH * hourMs(), stageEnd - now);  // 不超过当前阶段剩余（9.1 节）
  if (reduce <= 0) return fail('当前阶段即将结束，无法施肥');
  state.inventory.fertilizers[fert.id]--;
  plot.fertilizedStages.push(stage);
  plot.matureTime -= reduce;
  plot.seasonMs -= reduce;
  sfx.fertilize();
  scene.magicAnim(plot.id);
  ok(`施肥成功，成熟期提前 ${Math.round(reduce / 1000)} 秒`);
  trackEvent('fertilize');
  refreshSubBar();
}

function doHarvest(plot, now, farm) {
  if (plot.state !== PLOT.MATURE) return fail('该地块作物还未成熟');
  const crop = cropOf(plot);
  settleHealth(plot, plot.matureTime);
  const h = healthOf(plot);
  const totalYield = actualYield(crop, h);
  const got = Math.max(0, totalYield - plot.stolenTotal);
  state.warehouse[crop.id] = (state.warehouse[crop.id] || 0) + got;
  sfx.harvest();
  scene.harvestAnim(plot.id);
  unlockCodex(crop.id);
  addExp(crop.harvestExp, true);
  ok(`收获 ${crop.name} ×${got}（健康度 ${Math.round(h)}）+${crop.harvestExp} 经验`, 'gold');
  if (plot.stolenTotal > 0) ui.toast(`😿 有 ${plot.stolenTotal} 个果实被偷走了`, 'err');
  trackEvent('harvest');

  if (plot.season < crop.seasons - 1) {   // 多季作物进入下一季（6.3 节）
    const seasonMs = seasonHours(crop, plot.season + 1) * hourMs();
    Object.assign(plot, {
      state: PLOT.GROWING, season: plot.season + 1,
      plantTime: now, matureTime: now + seasonMs, seasonMs,
      penalty: 0, settleTime: now,
      waterUntil: now + seasonMs * WATER_SPAN,
      weedSince: 0, pestSince: 0,
      nextRiskTime: now + seasonMs * RISK_WINDOW,
      fertilizedStages: [], stolenTotal: 0, stolenBy: [],
    });
  } else {
    plot.state = PLOT.RESIDUE;
  }
}

// 偷菜（11.3 节）
function doSteal(farm, plot, now) {
  if (plot.state !== PLOT.MATURE) return fail('该地块没有成熟的作物');
  if (plot.stolenBy.includes('me')) return fail('本轮成熟你已经偷过这块地了');
  const crop = cropOf(plot);
  const h = healthOf(plot);
  const cap = Math.floor(actualYield(crop, h) * STEAL_CAP_RATIO);
  const remain = cap - plot.stolenTotal;
  if (remain <= 0) return fail('这块地已被偷到上限，主人至少保留 60%');

  // 狗拦截判定（12.4 节）
  const friend = farm.friend;
  if (friend && friend.dog && friend.dogBowl > 0) {
    const dogDef = DOGS.find(d => d.id === friend.dog);
    if (rand() < dogDef.intercept) {
      const penalty = crop.fruitPrice * CATCH_PENALTY_MULT;
      plot.stolenBy.push('me');
      state.gold = Math.max(0, state.gold - penalty);
      sfx.dog();
      ui.toast(`🐕 被 ${friend.name} 家的${dogDef.name}抓住了！赔付 ${penalty} 金币`, 'err');
      ui.updateHUD(state);
      return;
    }
  }
  const n = Math.min(STEAL_MIN + Math.floor(rand() * (STEAL_MAX - STEAL_MIN + 1)), remain);
  plot.stolenTotal += n;
  plot.stolenBy.push('me');
  state.warehouse[crop.id] = (state.warehouse[crop.id] || 0) + n;
  sfx.steal();
  scene.harvestAnim(plot.id);
  ok(`🥷 偷到 ${crop.name} ×${n}，已放入仓库`, 'gold');
  trackEvent('steal');
}

// NPC 偷玩家（单机模拟）
function npcStealFromMe(friend, now) {
  const mature = state.plots.filter(p => p.state === PLOT.MATURE && !p.stolenBy.includes(friend.id));
  if (!mature.length) return;
  const plot = mature[Math.floor(rand() * mature.length)];
  const crop = cropOf(plot);
  const cap = Math.floor(actualYield(crop, healthOf(plot)) * STEAL_CAP_RATIO);
  const remain = cap - plot.stolenTotal;
  if (remain <= 0) return;

  if (state.dog && state.dogBowl > 0) {
    const dogDef = DOGS.find(d => d.id === state.dog.id);
    const rate = dogDef.intercept + state.dog.level * 0.01;
    if (rand() < rate) {
      const gain = crop.fruitPrice * CATCH_PENALTY_MULT;
      plot.stolenBy.push(friend.id);
      addGold(gain);
      state.dog.catches++;
      const newLv = Math.min(DOG_MAX_LEVEL, Math.floor(state.dog.catches / DOG_CATCHES_PER_LEVEL));
      if (newLv > state.dog.level) {
        state.dog.level = newLv;
        addMail({ title: '狗狗升级了', content: `${dogDef.name}升到 Lv.${newLv}，拦截率提升至 ${Math.round((dogDef.intercept + newLv * 0.01) * 100)}%！` });
      }
      sfx.dog();
      addMail({ title: '🐶 拦截成功', content: `${friend.name} 试图偷你的 ${crop.name}，被${dogDef.name}当场抓住，对方赔付 ${gain} 金币！`, gold: 0 });
      ui.toast(`🐶 ${dogDef.name}抓住了 ${friend.name}！获赔 ${gain} 金币`, 'gold');
      return;
    }
  }
  const n = Math.min(STEAL_MIN + Math.floor(rand() * (STEAL_MAX - STEAL_MIN + 1)), remain);
  plot.stolenTotal += n;
  plot.stolenBy.push(friend.id);
  addMail({ title: '😿 作物被偷', content: `${friend.name} 偷走了你的 ${crop.name} ×${n}。`, gold: 0 });
}

// ---------------- online 入口（DevNetPanel / 联调） ----------------
/**
 * 登录 + Handshake + EnterFarm 成功后切入 online：applyPatch 快照并记录会话。
 * @param {import('../net/client.js').NetClient} client
 * @param {import('../net/client.js').Envelope} enterEnv
 */
function enterOnlineFromNet(client, enterEnv) {
  if (!client?.uid || !client?.token) {
    throw new Error('enterOnlineFromNet: missing uid/token');
  }
  if (!enterEnv || enterEnv.err !== 0) {
    throw new Error(`enterOnlineFromNet: enterFarm err=${enterEnv?.err}`);
  }
  applyPatch(state, enterEnv.payload || {});
  enterOnline({ uid: client.uid, token: client.token });
  netClient = client;
  viewing = 'me';
  ui.setVisitor?.(null);
  refreshToolbar();
  refreshSubBar();
  syncAllPlots();
  ui.updateHUD(state);
  ui.toast('已进入 online 模式：操作将发往服务端', 'ok');
}

function playOnlineFx(tool, plotId) {
  switch (tool) {
    case 'till':
      sfx.till();
      scene.burst(plotId, 0x8d6e63, 12, true);
      break;
    case 'plant':
      sfx.plant();
      scene.burst(plotId, 0x9be15d, 12, true);
      break;
    case 'water':
      sfx.water();
      scene.waterAnim(plotId);
      break;
    case 'weed':
      sfx.weed();
      scene.burst(plotId, 0x81c784, 10, true);
      break;
    case 'pest':
      sfx.pest();
      scene.burst(plotId, 0xffb74d, 10, true);
      break;
    case 'fert':
      sfx.fertilize();
      scene.magicAnim(plotId);
      break;
    case 'harvest':
      sfx.harvest();
      scene.harvestAnim(plotId);
      break;
    default:
      break;
  }
}

/** online：发意图 → 等 Rsp → applyPatch；失败 toast 不改地块 */
async function onPlotClickOnline(plotId) {
  if (onlineBusy || !netClient) return;
  const farm = currentFarm();
  const plot = farm.plots[plotId];
  if (!plot || plotId >= farm.unlocked) return;

  if (!activeTool) return showPlotTip(plotId);

  const cmd = plotCmdForTool(activeTool, plot.state);
  if (cmd == null) {
    if (activeTool === 'till') return fail('这块地不需要锄地');
    return showPlotTip(plotId);
  }

  let arg = 0;
  if (cmd === CMD_PLANT) {
    arg = cropKeyToId(selectedSeed);
    if (!arg) return fail('请先选择种子');
  }
  if (cmd === CMD_FERTILIZE) {
    arg = fertilizerKeyToId(selectedFert);
    if (!arg) return fail('请先选择化肥');
  }

  onlineBusy = true;
  try {
    const rsp = await netClient.plotAction(cmd, plotId, arg);
    if (rsp.err !== 0) {
      if (shouldApplyPatchFromError(rsp.err, rsp.payload)) {
        applyPatch(state, rsp.payload);
        ui.updateHUD(state);
        syncAllPlots();
        refreshSubBar();
      }
      fail(errText(rsp.err));
      return;
    }
    applyPatch(state, rsp.payload || {});
    playOnlineFx(activeTool, plotId);
    ok(activeTool === 'harvest' ? '收获成功' : '操作成功');
    ui.updateHUD(state);
    syncAllPlots();
    refreshSubBar();
  } catch (e) {
    fail(e instanceof Error ? e.message : String(e));
  } finally {
    onlineBusy = false;
  }
}

async function onlineBuySeed(id) {
  if (onlineBusy || !netClient) return;
  const itemId = cropKeyToId(id);
  if (!itemId) return fail('商品不存在');
  onlineBusy = true;
  try {
    const rsp = await netClient.buy(itemId, 1);
    if (rsp.err !== 0) {
      fail(errText(rsp.err));
      return;
    }
    applyPatch(state, rsp.payload || {});
    sfx.gold();
    const c = CROP_MAP[id];
    ui.toast(`购买 ${c?.name || id} 种子 ×1`, 'ok');
    ui.updateHUD(state);
  } catch (e) {
    fail(e instanceof Error ? e.message : String(e));
  } finally {
    onlineBusy = false;
  }
}

async function onlineBuyFertilizer(id) {
  if (onlineBusy || !netClient) return;
  const fertilizer = FERTILIZERS.find(item => item.id === id);
  if (!fertilizer?.shopItemId) return fail('商品不存在');
  onlineBusy = true;
  try {
    const rsp = await netClient.buy(fertilizer.shopItemId, 1);
    if (rsp.err !== 0) {
      fail(errText(rsp.err));
      return;
    }
    applyPatch(state, rsp.payload || {});
    sfx.gold();
    ui.toast(`购买 ${fertilizer.name} ×1`, 'ok');
    ui.updateHUD(state);
    refreshSubBar();
  } catch (e) {
    fail(e instanceof Error ? e.message : String(e));
  } finally {
    onlineBusy = false;
  }
}

async function onlineSell(id, n) {
  if (onlineBusy || !netClient) return;
  const itemId = cropKeyToId(id);
  if (!itemId) return fail('该物品不可出售');
  const have = state.warehouse[id] || 0;
  const count = Math.min(n, have);
  if (count <= 0) return;
  onlineBusy = true;
  try {
    const rsp = await netClient.sell(itemId, count);
    if (rsp.err !== 0) {
      fail(errText(rsp.err));
      return;
    }
    applyPatch(state, rsp.payload || {});
    sfx.gold();
    const c = CROP_MAP[id];
    ui.toast(`出售 ${c?.name || id} ×${count}`, 'gold');
    ui.updateHUD(state);
  } catch (e) {
    fail(e instanceof Error ? e.message : String(e));
  } finally {
    onlineBusy = false;
  }
}

async function onlineSellAll() {
  if (onlineBusy || !netClient) return;
  const entries = Object.entries(state.warehouse).filter(([, n]) => n > 0);
  if (!entries.length) return;
  onlineBusy = true;
  try {
    let sold = 0;
    let lastErrMsg = null;
    for (const [id, n] of entries) {
      const itemId = cropKeyToId(id);
      if (!itemId) continue;
      const rsp = await netClient.sell(itemId, n);
      if (rsp.err !== 0) {
        lastErrMsg = errText(rsp.err);
        break;
      }
      applyPatch(state, rsp.payload || {});
      sold++;
    }
    if (sold > 0) {
      sfx.gold();
      if (lastErrMsg) {
        ui.toast(`部分出售成功（${sold} 项），其余失败：${lastErrMsg}`, 'err');
      } else {
        ui.toast('出售完成', 'gold');
      }
      ui.updateHUD(state);
    } else if (lastErrMsg) {
      fail(lastErrMsg);
    }
  } catch (e) {
    fail(e instanceof Error ? e.message : String(e));
  } finally {
    onlineBusy = false;
  }
}

// ---------------- 地块点击 ----------------
function onPlotClick(plotId) {
  // online 自家农场：发 WS 意图，等 Rsp 再 patch
  if (isOnline() && viewing === 'me') {
    void onPlotClickOnline(plotId);
    return;
  }
  if (isOnline()) {
    fail('线上暂不支持');
    return;
  }

  const now = Date.now();
  const farm = currentFarm();
  const plot = farm.plots[plotId];
  if (!plot) return;
  if (farm.isMe) {
    switch (activeTool) {
      case 'till': doTill(plot, now); break;
      case 'plant': doPlant(plot, now); break;
      case 'water': doWater(plot, now, false); break;
      case 'weed': doWeed(plot, now, false); break;
      case 'pest': doPest(plot, now, false); break;
      case 'fert': doFertilize(plot, now); break;
      case 'harvest': doHarvest(plot, now, farm); break;
      default: return showPlotTip(plotId);
    }
  } else {
    switch (activeTool) {
      case 'water': doWater(plot, now, true); break;
      case 'weed': doWeed(plot, now, true); break;
      case 'pest': doPest(plot, now, true); break;
      case 'steal': doSteal(farm, plot, now); break;
      default: return showPlotTip(plotId);
    }
  }
  ui.updateHUD(state);
  syncAllPlots();
}

// 未选工具时点击：显示地块信息 toast
function showPlotTip(plotId) {
  const farm = currentFarm();
  const plot = farm.plots[plotId];
  if (!plot || plotId >= farm.unlocked) return;
  const tips = {
    [PLOT.WASTELAND]: '选择 ⛏️ 锄地来翻整这块荒地',
    [PLOT.TILLED]: '选择 🌱 播种工具种下作物',
    [PLOT.GROWING]: '作物生长中，记得浇水除草除虫',
    [PLOT.MATURE]: farm.isMe ? '选择 🧺 收获工具收取果实' : '选择 🥷 偷菜工具摘取果实',
    [PLOT.RESIDUE]: '选择 ⛏️ 清理残株后重新播种',
    [PLOT.WITHERED]: '作物已枯萎，选择 ⛏️ 清理',
  };
  ui.toast(tips[plot.state] || '', 'info');
}

// ---------------- Tooltip ----------------
function tooltipHTML(plotId) {
  const farm = currentFarm();
  const now = Date.now();
  const plot = farm.plots[plotId];
  if (!plot) return '';
  if (plotId >= farm.unlocked) {
    const expDef = EXPANSION.find(x => x[0] === plotId + 1);
    if (!expDef || !farm.isMe) return '';
    return `<h4>🔒 未开垦土地</h4><div class="row"><span>开垦条件</span><b>Lv.${expDef[1]} · 💰${expDef[2].toLocaleString()}</b></div>`;
  }
  switch (plot.state) {
    case PLOT.WASTELAND: return `<h4>荒地</h4><div class="row"><span>状态</span><b>需要锄地</b></div>`;
    case PLOT.TILLED: return `<h4>空闲土地</h4><div class="row"><span>状态</span><b>已翻耕，可播种</b></div>`;
    case PLOT.RESIDUE: return `<h4>待清理</h4><div class="row"><span>状态</span><b>残株待清理</b></div>`;
    case PLOT.WITHERED: {
      const c = cropOf(plot);
      return `<h4>🥀 ${c.name}（枯萎）</h4><div class="row"><span>状态</span><b>产量全失，需清理</b></div>`;
    }
    case PLOT.GROWING:
    case PLOT.MATURE: {
      const c = cropOf(plot);
      const { stage, total } = stageOf(plot, now);
      const h = Math.round(healthOf(plot));
      const dry = now > plot.waterUntil;
      const mature = plot.state === PLOT.MATURE;
      const yieldEst = actualYield(c, h);
      const cap = Math.floor(yieldEst * STEAL_CAP_RATIO);
      let rows = `<div class="row"><span>第 ${plot.season + 1}/${c.seasons} 季</span><b>${mature ? '✨ 已成熟' : stageName(c, stage)}</b></div>`;
      rows += `<div class="row"><span>${mature ? '枯萎倒计时' : '成熟倒计时'}</span><b>${fmtTime(mature ? plot.matureTime + plot.seasonMs * WITHER_SPAN - now : plot.matureTime - now)}</b></div>`;
      rows += `<div class="row"><span>健康度</span><b>${h}</b></div><div class="bar"><i style="width:${h}%"></i></div>`;
      rows += `<div class="row"><span>预计产量</span><b>${yieldEst}${plot.stolenTotal ? `（被偷 ${plot.stolenTotal}）` : ''}</b></div>`;
      let tags = '';
      if (!mature) {
        if (dry) tags += `<span class="tag dry">💧 缺水</span>`;
        if (plot.weedSince) tags += `<span class="tag weed">🌿 杂草</span>`;
        if (plot.pestSince) tags += `<span class="tag pest">🐛 害虫</span>`;
      } else {
        tags += `<span class="tag steal">🥷 可偷 ${Math.max(0, cap - plot.stolenTotal)}</span>`;
      }
      return `<h4><span>${c.name}</span><span class="stage">${c.hidden ? '✨' : ''}</span></h4>${rows}${tags ? `<div class="tags">${tags}</div>` : ''}`;
    }
  }
  return '';
}

// ---------------- 3D 同步 ----------------
function syncAllPlots() {
  const now = Date.now();
  const farm = currentFarm();
  scene.forEachPlot((g, i) => {
    const unlocked = i < farm.unlocked;
    const plot = farm.plots[i];
    if (!plot) { scene.updatePlot(g, { unlocked: false, lockText: '', state: PLOT.WASTELAND }); return; }
    let info = { unlocked, lockText: '', state: plot.state, cropDef: null, stage: 0, totalStages: 3, dry: false, weed: false, pest: false };
    if (!unlocked) {
      const expDef = EXPANSION.find(x => x[0] === i + 1);
      info.lockText = expDef ? `Lv.${expDef[1]}` : '';
    } else if (plot.state === PLOT.GROWING || plot.state === PLOT.MATURE || plot.state === PLOT.WITHERED) {
      const crop = cropOf(plot);
      const { stage, total } = stageOf(plot, now);
      info.cropDef = crop;
      info.stage = stage;
      info.totalStages = total;
      info.dry = plot.state === PLOT.GROWING && now > plot.waterUntil;
      info.weed = !!plot.weedSince && plot.state === PLOT.GROWING;
      info.pest = !!plot.pestSince && plot.state === PLOT.GROWING;
    }
    scene.updatePlot(g, info);
  });
}

// ---------------- 工具选择 ----------------
function refreshToolbar() {
  const tools = viewing === 'me' ? TOOLS_HOME : TOOLS_VISIT;
  ui.renderToolbar(tools, activeTool);
}

function refreshSubBar() {
  if (activeTool === 'plant') {
    const items = Object.entries(state.inventory.seeds)
      .filter(([, n]) => n > 0)
      .map(([id, n]) => ({ id, name: CROP_MAP[id].name, count: n, badge: badgeHTML(CROP_MAP[id], true) }));
    if (!items.find(i => i.id === selectedSeed)) selectedSeed = items[0]?.id || null;
    ui.showSubBar(items, selectedSeed, (id) => { selectedSeed = id; refreshSubBar(); sfx.click(); });
  } else if (activeTool === 'fert') {
    const items = FERTILIZERS
      .map(f => ({ id: f.id, name: f.name, icon: f.icon, count: state.inventory.fertilizers[f.id] || 0 }))
      .filter(i => i.count > 0);
    if (!items.find(i => i.id === selectedFert)) selectedFert = items[0]?.id || null;
    ui.showSubBar(items, selectedFert, (id) => { selectedFert = id; refreshSubBar(); sfx.click(); });
  } else {
    ui.showSubBar(null);
  }
}

// ---------------- NPC 模拟 ----------------
function npcAction(friend, now) {
  if (now < friend.nextActionTime) return;
  friend.nextActionTime = now + 8000 + rand() * 14000;

  const farmRef = { plots: friend.plots, isMe: false };
  const byState = (s) => friend.plots.filter(p => p.state === s);
  const r = rand();

  // 优先收获/清理/锄地/播种，保证 NPC 农场持续运转
  const mature = byState(PLOT.MATURE);
  if (mature.length && r < 0.5) {
    const plot = mature[Math.floor(rand() * mature.length)];
    const crop = cropOf(plot);
    if (plot.season < crop.seasons - 1) {
      const seasonMs = seasonHours(crop, plot.season + 1) * hourMs();
      Object.assign(plot, { state: PLOT.GROWING, season: plot.season + 1, plantTime: now, matureTime: now + seasonMs, seasonMs, penalty: 0, settleTime: now, waterUntil: now + seasonMs * WATER_SPAN, weedSince: 0, pestSince: 0, nextRiskTime: now + seasonMs * RISK_WINDOW, fertilizedStages: [], stolenTotal: 0, stolenBy: [] });
    } else plot.state = PLOT.RESIDUE;
    return;
  }
  const residue = byState(PLOT.RESIDUE).concat(byState(PLOT.WITHERED));
  if (residue.length && r < 0.75) {
    const plot = residue[Math.floor(rand() * residue.length)];
    Object.assign(plot, { state: PLOT.TILLED, cropId: null, penalty: 0, weedSince: 0, pestSince: 0 });
    return;
  }
  const waste = byState(PLOT.WASTELAND);
  if (waste.length) { waste[0].state = PLOT.TILLED; return; }
  const tilled = byState(PLOT.TILLED);
  if (tilled.length) {
    const pool = CROPS.filter(c => !c.hidden && c.unlock <= friend.level);
    const crop = pool[Math.floor(rand() * pool.length)];
    const plot = tilled[Math.floor(rand() * tilled.length)];
    const seasonMs = seasonHours(crop, 0) * hourMs();
    Object.assign(plot, { state: PLOT.GROWING, cropId: crop.id, season: 0, plantTime: now, matureTime: now + seasonMs, seasonMs, penalty: 0, settleTime: now, waterUntil: now + seasonMs * WATER_SPAN, weedSince: 0, pestSince: 0, nextRiskTime: now + seasonMs * RISK_WINDOW, fertilizedStages: [], stolenTotal: 0, stolenBy: [] });
    return;
  }
  // NPC 照料：概率较低，让草虫有机会积累
  const growing = byState(PLOT.GROWING);
  if (growing.length) {
    const plot = growing[Math.floor(rand() * growing.length)];
    settleHealth(plot, now);
    if (now > plot.waterUntil) plot.waterUntil = now + plot.seasonMs * WATER_SPAN;
    else if (plot.weedSince && rand() < 0.45) plot.weedSince = 0;
    else if (plot.pestSince && rand() < 0.45) plot.pestSince = 0;
  }
}

// ---------------- UI 回调 ----------------
const ui = new UI({
  getState: () => state,

  onTool(id) {
    sfx.click();
    activeTool = activeTool === id ? null : id;
    refreshToolbar();
    refreshSubBar();
  },

  onPanel(panel) {
    sfx.click();
    switch (panel) {
      case 'shop': ui.renderShop(state); break;
      case 'bag': ui.renderBag(state); break;
      case 'barn': ui.renderBarn(state); break;
      case 'tasks': ui.renderTasks(state, logicDayMs(state.timeScale) - (Date.now() - state.daily.dayStart)); break;
      case 'codex': ui.renderCodex(state); break;
      case 'mail': ui.renderMail(state); break;
      case 'friends': ui.renderFriends(state); break;
    }
  },

  async onBuySeed(id) {
    if (isOnline()) return onlineBuySeed(id);
    const c = CROP_MAP[id];
    if (state.gold < c.seedPrice) return fail('金币不足');
    addGold(-c.seedPrice);
    state.inventory.seeds[id] = (state.inventory.seeds[id] || 0) + 1;
    sfx.gold();
    ui.toast(`购买 ${c.name} 种子 ×1`, 'ok');
  },

  async onBuyFert(id) {
    if (isOnline()) return onlineBuyFertilizer(id);
    const f = FERTILIZERS.find(f => f.id === id);
    if (state.gold < f.price) return fail('金币不足');
    addGold(-f.price);
    state.inventory.fertilizers[id]++;
    sfx.gold();
    ui.toast(`购买 ${f.name} ×1`, 'ok');
  },

  onBuyFood(g) {
    // 狗粮属更后期；online 禁止本地改 gold/dogBowl
    if (isOnline()) return fail('线上暂不支持');
    const grams = g === -1 ? DOG_BOWL_CAP - Math.floor(state.dogBowl) : g;
    if (grams <= 0) return fail('狗盆已经是满的');
    const cost = grams * DOG_FOOD_PRICE;
    if (state.gold < cost) return fail('金币不足');
    addGold(-cost);
    state.dogBowl = Math.min(DOG_BOWL_CAP, state.dogBowl + grams);
    sfx.gold();
    ui.toast(`狗粮 +${grams}g`, 'ok');
  },

  onBuyDog(id) {
    // 买狗属更后期；online 禁止本地改 gold/dog
    if (isOnline()) return fail('线上暂不支持');
    const d = DOGS.find(d => d.id === id);
    if (myLevel() < d.unlock) return fail(`需要 Lv.${d.unlock}`);
    if (state.gold < d.price) return fail('金币不足');
    addGold(-d.price);
    // 更换狗种时各自等级独立保留（12.3 节）
    state.dogArchive = state.dogArchive || {};
    if (state.dog) state.dogArchive[state.dog.id] = { level: state.dog.level, catches: state.dog.catches };
    const arch = state.dogArchive[id] || { level: 0, catches: 0 };
    state.dog = { id, level: arch.level, catches: arch.catches };
    if (state.dogBowl <= 0) state.dogBowl = DOG_BOWL_CAP / 2;  // 新狗附赠半盆粮
    sfx.dog();
    ui.toast(`🐶 ${d.name} 开始为你看家护院！`, 'gold');
  },

  async onSell(id, n) {
    if (isOnline()) return onlineSell(id, n);
    const c = CROP_MAP[id];
    const have = state.warehouse[id] || 0;
    const count = Math.min(n, have);
    if (count <= 0) return;
    state.warehouse[id] -= count;
    const gain = count * c.fruitPrice;
    addGold(gain);
    sfx.gold();
    ui.toast(`出售 ${c.name} ×${count}，+${gain} 金币`, 'gold');
    trackEvent('sell');
  },

  async onSellAll() {
    if (isOnline()) return onlineSellAll();
    let gain = 0, any = false;
    for (const [id, n] of Object.entries(state.warehouse)) {
      if (n > 0) { gain += n * CROP_MAP[id].fruitPrice; state.warehouse[id] = 0; any = true; }
    }
    if (!any) return;
    addGold(gain);
    sfx.gold();
    ui.toast(`全部出售，+${gain} 金币`, 'gold');
    trackEvent('sell');
  },

  onClaimMail(id) {
    if (isOnline()) return fail('线上暂不支持');
    const m = state.mails.find(m => m.id === id);
    if (!m || m.claimed) return fail('该邮件附件已领取');
    m.claimed = true; m.read = true;
    if (m.gold) addGold(m.gold);
    if (m.exp) addExp(m.exp, true);
    sfx.gold();
    ui.toast(`领取成功${m.gold ? ` +${m.gold} 金币` : ''}${m.exp ? ` +${m.exp} 经验` : ''}`, 'gold');
    ui.updateHUD(state);
  },

  onVisit(friendId) {
    if (isOnline()) return fail('线上暂不支持');
    viewing = friendId;
    activeTool = null;
    const f = state.friends.find(f => f.id === friendId);
    ui.setVisitor(f.name);
    refreshToolbar();
    refreshSubBar();
    syncAllPlots();
    scene.setDog(f.dog ? DOGS.find(d => d.id === f.dog) : null, false);
    ui.toast(`来到 ${f.name} 的农场，可以帮忙照料或偷菜`, 'info');
  },

  onBackHome() {
    viewing = 'me';
    activeTool = null;
    ui.setVisitor(null);
    refreshToolbar();
    refreshSubBar();
    syncAllPlots();
    sfx.click();
  },

  onExpand() {
    if (isOnline()) return fail('线上暂不支持');
    const next = EXPANSION.find(e => e[0] === state.unlockedPlots + 1);
    if (!next) return;
    const [, needLv, cost] = next;
    if (myLevel() < needLv) return fail(`需要 Lv.${needLv}`);
    if (state.gold < cost) return fail(`金币不足（需要 💰${cost.toLocaleString()}）`);
    addGold(-cost);
    state.unlockedPlots++;
    sfx.till();
    ui.toast(`🚜 开垦成功！现在拥有 ${state.unlockedPlots} 块土地`, 'gold');
    syncAllPlots();
    ui.updateHUD(state);
  },

  onSetTimeScale(id) { state.timeScale = id; sfx.click(); },
  onSetSound(v) { state.settings.sound = v; sfx.enabled = v; sfx.click(); },
  onReset() { clearSave(); location.reload(); },
});

// ---------------- 事件绑定 ----------------
scene.clickCb = onPlotClick;
scene.hoverCb = (plotId, x, y) => {
  hoverPlot = plotId;
  if (plotId === null || ui.modalOpen) { ui.showTooltip(null); return; }
  ui.showTooltip(tooltipHTML(plotId), x, y);
};

addEventListener('beforeunload', () => saveGame(state));
document.addEventListener('visibilitychange', () => { if (document.hidden) saveGame(state); });

// ---------------- 主循环 ----------------
const CLOCK_ICONS = [[0.22, '🌙'], [0.28, '🌅'], [0.42, '☀️'], [0.68, '☀️'], [0.78, '🌇'], [0.84, '🌙'], [1.01, '🌙']];
let lastSave = 0;
let lastTick = Date.now();

function tick() {
  const now = Date.now();
  const dt = now - lastTick;
  lastTick = now;

  checkLogicDay(now);

  // online：自家地块以服务端为准，不做本地权威推进。
  if (!isOnline()) {
    tickPlots({ plots: state.plots, isMe: true }, now);
    // NPC 好友仅属本地模式；online 禁止其改写本地镜像。
    for (const f of state.friends) {
      tickPlots({ plots: f.plots, isMe: false }, now);
      npcAction(f, now);
      // NPC 狗粮消耗与补充（简化：保持有粮）
      if (f.dog && f.dogBowl <= 0) f.dogBowl = DOG_BOWL_CAP;
    }
  }

  // 玩家狗粮消耗（12.2 节）；online 暂保留本地狗粮动画
  if (state.dog && state.dogBowl > 0) {
    const dogDef = DOGS.find(d => d.id === state.dog.id);
    state.dogBowl = Math.max(0, state.dogBowl - (dogDef.consumption / hourMs()) * dt);
    if (state.dogBowl <= 0) ui.toast('🐶 狗粮吃完了，看家狗罢工了！', 'err');
  }

  // NPC 来偷菜（随机）；online 不跑假偷菜，避免改写服务端镜像
  if (!isOnline() && state.plots.some(p => p.state === PLOT.MATURE)) {
    for (const f of state.friends) {
      f.nextStealTime = f.nextStealTime || now + 40000 + rand() * 50000;
      if (now >= f.nextStealTime) {
        f.nextStealTime = now + 45000 + rand() * 50000;
        if (rand() < 0.35) npcStealFromMe(f, now);
      }
    }
  }

  // 日夜循环（跟随逻辑日）
  const dayMs = logicDayMs(state.timeScale);
  const phase = ((now - state.daily.dayStart) % dayMs) / dayMs;
  scene.setDayPhase(phase);
  const icon = CLOCK_ICONS.find(([t]) => phase < t)?.[1] || '☀️';
  ui.setClock(icon);

  // 狗模型（仅自己农场显示）
  if (viewing === 'me') {
    scene.setDog(state.dog ? DOGS.find(d => d.id === state.dog.id) : null, state.dog ? state.dogBowl <= 0 : false);
  }

  syncAllPlots();
  ui.updateHUD(state);

  // 悬停 tooltip 实时刷新
  if (hoverPlot !== null && !ui.modalOpen) {
    const el = ui.tooltip;
    if (!el.classList.contains('hidden')) {
      const [x, y] = lastMouse;
      ui.showTooltip(tooltipHTML(hoverPlot), x, y);
    }
  }

  if (now - lastSave > 5000) { lastSave = now; saveGame(state); }
}

const lastMouse = [0, 0];
addEventListener('pointermove', (e) => { lastMouse[0] = e.clientX; lastMouse[1] = e.clientY; });

// ---------------- 启动 ----------------
// DevNetPanel / 调试：暴露 online 切入与状态
window.__farm = {
  getState: () => state,
  scene,
  enterOnlineFromNet,
  isOnline,
  getNetClient: () => netClient,
};
refreshToolbar();
syncAllPlots();
scene.start();
setInterval(tick, 300);
tick();

// 离线收益提示
const offline = Date.now() - (state.lastSeen || Date.now());
if (offline > 60000) {
  setTimeout(() => ui.toast(`👋 欢迎回来！离开了 ${Math.round(offline / 60000)} 分钟，农场已继续运转`, 'info'), 800);
} else if (state.stats && state.exp === 0) {
  setTimeout(() => ui.toast('🌾 欢迎来到农场！先去 🛒 商店买种子，再 ⛏️ 锄地播种吧', 'info'), 800);
}
