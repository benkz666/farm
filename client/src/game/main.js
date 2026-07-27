// ============================================================
// QQ农场 3D —— 游戏主逻辑
// 规则实现严格对照 docs/design/game-design-full.md
// ============================================================
import {
  TIME_SCALES, CROP_MAP, FERTILIZERS, DOGS, EXPANSION, TASK_POOL,
  YIELD_FLOOR, WITHER_SPAN, STEAL_CAP_RATIO,
  CODEX_MILESTONES, stageCount, STAGE_NAMES_3, STAGE_NAMES_4, levelUpGold, logicDayMs,
} from './config.js';
import { PLOT, defaultState, clearSave, levelOf, drawDailyTasks } from './state.js';
import { FarmScene } from './farm3d.js';
import { UI, badgeHTML, fmtTime } from './ui.js';
import { SFX } from './audio.js';
import { enterOnline, isOnline, session, setFarmView } from '../net/session.js';
import { CMD_FERTILIZE, CMD_PLANT } from '../net/client.js';
import { errText } from '../net/errors.js';
import { applyPatch, cropKeyToId } from './applyPatch.js';
import { createFarmMirror } from './farmMirror.js';
import { plotCmdForTool } from './onlineActions.js';
import { shouldApplyPatchFromError } from './onlineResponse.js';

// ---------------- 初始化（期 3：无本地权威存档；状态由 snapshot/Rsp 驱动） ----------------
clearSave(); // 清理遗留 farm3d_save_v1，避免误以为本地仍可玩
let state = defaultState();
if (!state.tasks.length) drawDailyTasks(state);
state.settings = state.settings || { sound: true };

const sfx = new SFX();
sfx.enabled = state.settings.sound;

const scene = new FarmScene(document.getElementById('scene-container'));

let activeTool = null;
let selectedSeed = null;
let selectedFert = null;
let hoverPlot = null;

/** @type {import('../net/client.js').NetClient|null} */
let netClient = null;
let farmMirror = null;
let stopDeltaSubscription = null;
let stopPlayerDeltaSubscription = null;
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
// ---------------- 通用辅助 ----------------
const cropOf = (plot) => CROP_MAP[plot.cropId];

function currentFarm() {
  return {
    plots: state.plots,
    owner: session.viewingOwnerUid,
    unlocked: state.unlockedPlots,
    isMe: session.relation !== 'FRIEND',
  };
}

const isVisitingFriend = () => session.relation === 'FRIEND';

function applyResponsePatch(payload) {
  applyPatch(state, payload || {});
  const farmSeq = Number(payload?.farm_seq);
  if (session.relation === 'SELF' && Number.isSafeInteger(farmSeq) && farmSeq >= session.lastFarmSeq) {
    session.lastFarmSeq = farmSeq;
  }
}

function refreshFarmMirror() {
  const visiting = isVisitingFriend();
  activeTool = visiting ? null : activeTool;
  ui.setReadOnly?.(visiting);
  ui.setVisitor?.(visiting ? `UID ${session.viewingOwnerUid}` : null);
  refreshToolbar();
  refreshSubBar();
  syncAllPlots();
  ui.updateHUD(state);
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

// ---------------- 农事反馈 ----------------
function fail(msg) { sfx.error(); ui.toast(msg, 'err'); }
function ok(msg, type = 'ok') { ui.toast(msg, type); }

// ---------------- online 入口（登录页 authFlow / DEV 诊断） ----------------
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
  const payload = enterEnv.payload || {};
  const snapshot = payload.snapshot || {};
  const ownerUid = Number(snapshot.owner_uid) || client.uid;
  const relation = payload.relation === 'FRIEND' ? 'FRIEND' : 'SELF';
  applyPatch(state, payload);
  enterOnline({ uid: client.uid, token: client.token });
  netClient = client;
  setFarmView({
    ownerUid,
    farmSeq: Number(payload.farm_seq) || 0,
    relation,
  });
  stopDeltaSubscription?.();
  farmMirror = createFarmMirror({
    state,
    session,
    syncFarm: async (viewOwnerUid, fromSeq) => {
      const response = await client.syncFarm(viewOwnerUid, fromSeq);
      if (response.err !== 0) throw new Error(errText(response.err));
      return response;
    },
    onApplied: refreshFarmMirror,
  });
  stopDeltaSubscription = client.onDelta((deltaEnv) => {
    void farmMirror.onDelta(deltaEnv.payload).catch((error) => {
      fail(error instanceof Error ? error.message : String(error));
    });
  });
  stopPlayerDeltaSubscription?.();
  stopPlayerDeltaSubscription = client.onPlayerDelta((deltaEnv) => {
    applyPatch(state, deltaEnv.payload);
    ui.updateHUD(state);
    refreshSubBar();
  });
  refreshFarmMirror();
  ui.toast('已进入 online 模式：操作将发往服务端', 'ok');
}

async function enterFarm(ownerUid, nickname = '') {
  if (!netClient) return;
  onlineBusy = true;
  try {
    const response = await netClient.enterFarm(ownerUid);
    if (response.err !== 0) {
      fail(errText(response.err));
      return;
    }
    const payload = response.payload || {};
    const snapshot = payload.snapshot || {};
    applyPatch(state, payload);
    setFarmView({
      ownerUid: Number(snapshot.owner_uid) || (ownerUid || netClient.uid),
      farmSeq: Number(payload.farm_seq) || 0,
      relation: payload.relation === 'FRIEND' ? 'FRIEND' : 'SELF',
    });
    refreshFarmMirror();
    if (isVisitingFriend()) {
      ui.setVisitor?.(nickname || snapshot.nickname || `UID ${session.viewingOwnerUid}`);
      ui.toast('已进入好友农场，只可浏览', 'info');
    }
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
  } finally {
    onlineBusy = false;
  }
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
        applyResponsePatch(rsp.payload);
        ui.updateHUD(state);
        syncAllPlots();
        refreshSubBar();
      }
      fail(errText(rsp.err));
      return;
    }
    applyResponsePatch(rsp.payload);
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
  if (onlineBusy || !netClient || isVisitingFriend()) return;
  const itemId = cropKeyToId(id);
  if (!itemId) return fail('商品不存在');
  onlineBusy = true;
  try {
    const rsp = await netClient.buy(itemId, 1);
    if (rsp.err !== 0) {
      fail(errText(rsp.err));
      return;
    }
    applyResponsePatch(rsp.payload);
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
  if (onlineBusy || !netClient || isVisitingFriend()) return;
  const fertilizer = FERTILIZERS.find(item => item.id === id);
  if (!fertilizer?.shopItemId) return fail('商品不存在');
  onlineBusy = true;
  try {
    const rsp = await netClient.buy(fertilizer.shopItemId, 1);
    if (rsp.err !== 0) {
      fail(errText(rsp.err));
      return;
    }
    applyResponsePatch(rsp.payload);
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
  if (onlineBusy || !netClient || isVisitingFriend()) return;
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
    applyResponsePatch(rsp.payload);
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
  if (onlineBusy || !netClient || isVisitingFriend()) return;
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
      applyResponsePatch(rsp.payload);
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
  // 期 3：仅 online 意图；未登录不可种收（路由已挡 /farm，此处再兜底）
  if (!isOnline() || !netClient) {
    fail('请先登录后再操作');
    return;
  }
  if (isVisitingFriend()) {
    fail('好友农场仅可浏览');
    return;
  }
  void onPlotClickOnline(plotId);
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
  const tools = isVisitingFriend() ? [] : TOOLS_HOME;
  ui.renderToolbar(tools, activeTool);
}

async function refreshFriends() {
  if (!netClient) return false;
  try {
    const response = await netClient.friendList();
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    state.friends = Array.isArray(response.payload?.friends) ? response.payload.friends : [];
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function generateShareLink() {
  if (!netClient) return '';
  try {
    const response = await netClient.genShareLink();
    if (response.err !== 0 || !response.payload?.path) {
      fail(errText(response.err));
      return '';
    }
    return new URL(response.payload.path, location.origin).href;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return '';
  }
}

function inviteTokenFromInput(value) {
  const input = value.trim();
  if (!input) return '';
  try {
    const url = new URL(input, location.origin);
    return url.pathname.startsWith('/i/') ? decodeURIComponent(url.pathname.slice(3)) : '';
  } catch {
    return input.startsWith('/i/') ? decodeURIComponent(input.slice(3)) : '';
  }
}

async function addFriend(value) {
  if (!netClient) return false;
  const input = value.trim();
  if (!input) {
    fail('请输入用户名、UID 或分享链接');
    return false;
  }
  try {
    const token = inviteTokenFromInput(input);
    const peerUid = Number(input);
    let response;
    if (token) {
      response = await netClient.acceptInvite(token);
    } else if (Number.isSafeInteger(peerUid) && peerUid > 0) {
      response = await netClient.addFriendByUID(peerUid);
    } else {
      const search = await netClient.searchUser(input);
      if (search.err !== 0) {
        fail(errText(search.err));
        return false;
      }
      const foundUID = Number(search.payload?.uid);
      if (!Number.isSafeInteger(foundUID) || foundUID <= 0) {
        fail('搜索结果无效');
        return false;
      }
      response = await netClient.addFriendByUID(foundUID);
    }
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    await refreshFriends();
    ui.toast('添加好友成功', 'ok');
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function removeFriend(peerUid) {
  if (!netClient) return false;
  try {
    const response = await netClient.removeFriend(peerUid);
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    await refreshFriends();
    ui.toast('已删除好友', 'info');
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
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
    if (panel === 'shop' && isVisitingFriend()) {
      return fail('好友农场无法使用商店');
    }
    switch (panel) {
      case 'shop': ui.renderShop(state); break;
      case 'bag': ui.renderBag(state); break;
      case 'barn': ui.renderBarn(state); break;
      case 'tasks': ui.renderTasks(state, logicDayMs(state.timeScale) - (Date.now() - state.daily.dayStart)); break;
      case 'codex': ui.renderCodex(state); break;
      case 'mail': ui.renderMail(state); break;
      case 'friends':
        ui.renderFriends(state);
        void refreshFriends().then((ok) => {
          if (ok && ui.modalOpen) ui.renderFriends(state);
        });
        break;
    }
  },

  async onBuySeed(id) {
    if (!isOnline()) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('好友农场无法购买');
    return onlineBuySeed(id);
  },

  async onBuyFert(id) {
    if (!isOnline()) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('好友农场无法购买');
    return onlineBuyFertilizer(id);
  },

  onBuyFood(_g) {
    return fail('线上暂不支持');
  },

  onBuyDog(_id) {
    return fail('线上暂不支持');
  },

  async onSell(id, n) {
    if (!isOnline()) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('好友农场无法出售');
    return onlineSell(id, n);
  },

  async onSellAll() {
    if (!isOnline()) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('好友农场无法出售');
    return onlineSellAll();
  },

  onClaimMail(_id) {
    return fail('线上暂不支持');
  },

  onVisit(friendId, nickname) {
    void enterFarm(friendId, nickname);
  },

  onBackHome() {
    if (!netClient || !isVisitingFriend()) return;
    void enterFarm(0);
    sfx.click();
  },

  getSession() {
    return session;
  },

  onGenShareLink() {
    return generateShareLink();
  },

  onAddFriend(value) {
    return addFriend(value);
  },

  onRemoveFriend(peerUid) {
    return removeFriend(peerUid);
  },

  onExpand() {
    return fail('线上暂不支持');
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

// ---------------- 主循环 ----------------
const CLOCK_ICONS = [[0.22, '🌙'], [0.28, '🌅'], [0.42, '☀️'], [0.68, '☀️'], [0.78, '🌇'], [0.84, '🌙'], [1.01, '🌙']];
let lastTick = Date.now();

function tick() {
  const now = Date.now();
  const dt = now - lastTick;
  lastTick = now;

  checkLogicDay(now);

  // 期 3：不做本地权威地块推进 / NPC 假好友写；镜像仅由 snapshot/Rsp/Delta 更新

  // 玩家狗粮消耗（展示用；写权威在后期服务端）
  if (state.dog && state.dogBowl > 0) {
    const dogDef = DOGS.find(d => d.id === state.dog.id);
    state.dogBowl = Math.max(0, state.dogBowl - (dogDef.consumption / hourMs()) * dt);
    if (state.dogBowl <= 0) ui.toast('🐶 狗粮吃完了，看家狗罢工了！', 'err');
  }

  // 日夜循环（跟随逻辑日）
  const dayMs = logicDayMs(state.timeScale);
  const phase = ((now - state.daily.dayStart) % dayMs) / dayMs;
  scene.setDayPhase(phase);
  const icon = CLOCK_ICONS.find(([t]) => phase < t)?.[1] || '☀️';
  ui.setClock(icon);

  // 狗模型（仅自己农场显示）
  if (!isVisitingFriend()) {
    scene.setDog(state.dog ? DOGS.find(d => d.id === state.dog.id) : null, state.dog ? state.dogBowl <= 0 : false);
  } else {
    scene.setDog(null, false);
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
}

const lastMouse = [0, 0];
addEventListener('pointermove', (e) => { lastMouse[0] = e.clientX; lastMouse[1] = e.clientY; });

// ---------------- 启动 ----------------
// 登录页 authFlow / DEV 诊断：暴露 online 切入与状态
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

// 未 online 时不提示本地开局指引（须登录）；online 后由 enterOnlineFromNet toast
