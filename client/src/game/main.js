// ============================================================
// QQ农场 3D —— 游戏主逻辑
// 规则实现严格对照 docs/design/game-design-full.md
// ============================================================
import {
  TIME_SCALES, CROP_MAP, FERTILIZERS, DOGS, EXPANSION,
  YIELD_FLOOR, WITHER_SPAN, STEAL_CAP_RATIO, DOG_BOWL_CAP, DOG_FOOD_SHOP_ITEM_ID,
  stageCount, STAGE_NAMES_3, STAGE_NAMES_4, logicDayPhase,
} from './config.js';
import { PLOT, defaultState, applyMailClaimReceipt } from './state.js';
import { FarmScene } from './farm3d.js';
import { UI, badgeHTML, fmtTime } from './ui.js';
import { SFX } from './audio.js';
import { enterOnline, isOnline, leaveOnline, logout, session, setFarmView } from '../net/session.js';
import { CMD_FERTILIZE, CMD_HARVEST, CMD_PLANT, CMD_STEAL } from '../net/client.js';
import { errText } from '../net/errors.js';
import { applyCodexProgress, applyPatch, cropIdToKey, cropKeyToId } from './applyPatch.js';
import { createFarmMirror } from './farmMirror.js';
import { plotCmdForTool, isVisitTool } from './onlineActions.js';
import { shouldApplyPatchFromError } from './onlineResponse.js';
import {
  applyAuthoritativeFarmEnter,
  bindFarmReconnectRestore,
} from './reconnectRestore.js';
import { cropOf, stageOf, computePlotInfo } from './plotInfo.js';
import { bindPageUnload } from './pageLifecycle.js';
import {
  applyTaskListSchedule,
  createTaskResetScheduler,
  taskRefreshResultFromOutcome,
} from './taskResetTimer.js';
import { createTaskSession } from './taskSession.js';
import { openTasksPanel } from './taskPanel.js';
import { applyPetStatus } from './petStatus.js';

// Vite 热更新会重新执行本模块，但不一定触发 pagehide。保留状态和连接，
// 由新模块接管；否则旧实例与新实例会同时更新同一组 HUD。
const previousActiveRuntime = window.__farmActiveRuntime;
const hmrRuntime = import.meta.hot?.data?.runtime || previousActiveRuntime?.snapshot?.() || null;
const runtimeHandle = {};
// Vite 未能调用旧模块 dispose 时，也由全局槽位兜底释放上一实例。
// 这防止旧 tick 在后续热更新中继续写入 HUD。
previousActiveRuntime?.dispose?.();

// ---------------- 初始化（期 3：无本地权威存档；状态由 snapshot/Rsp 驱动） ----------------
let state = hmrRuntime?.state || defaultState();
state.settings = state.settings || { sound: true };

const sfx = new SFX();
sfx.enabled = state.settings.sound;

const scene = new FarmScene(document.getElementById('scene-container'));

let activeTool = null;
let selectedSeed = null;
let selectedFert = null;
let hoverPlot = null;

/** @type {import('../net/client.js').NetClient|null} */
let netClient = hmrRuntime?.netClient || null;
let farmMirror = null;
let stopDeltaSubscription = null;
let stopPlayerDeltaSubscription = null;
let stopMailNotifySubscription = null;
let stopTaskNotifySubscription = null;
/** @type {{ dispose: () => void }|null} */
let reconnectBinding = null;
let removePageUnload = null;
/** 等 Rsp 期间防连点 */
let onlineBusy = false;
let runtimeDisposed = false;
/** 服务端下次自然日 00:00（Unix 毫秒） */
let taskResetAt = 0;

/** @type {ReturnType<typeof createTaskResetScheduler>|null} */
let taskResetScheduler = null;
/** @type {ReturnType<typeof createTaskSession>|null} */
let taskSession = null;

function createLiveTaskResetScheduler() {
  return createTaskResetScheduler({
    setTimeout,
    clearTimeout,
    // timer 回调只拉取/应用；stale 不改 timer，真实失败才 retry
    refresh: async () => taskRefreshResultFromOutcome(await pullAndApplyTasks()),
  });
}

function ensureTaskSession() {
  if (!taskSession || taskSession.disposed) {
    taskSession = createTaskSession();
  }
  return taskSession;
}

/** 登录绑定 / 重连恢复：使旧 task 请求上下文失效（即使复用 client）。 */
function invalidateTaskSession() {
  ensureTaskSession().invalidate();
}

/** 卸载/登出/pagehide/HMR：dispose session + scheduler，在途结果不得写 state/排程。 */
function disposeTaskRuntime() {
  taskSession?.dispose();
  taskSession = null;
  taskResetScheduler?.dispose();
  taskResetScheduler = null;
  taskResetAt = 0;
}

/** 登录/重连/HMR 接管：需要时创建新 scheduler。 */
function ensureTaskResetScheduler() {
  if (!taskResetScheduler || taskResetScheduler.disposed) {
    taskResetScheduler = createLiveTaskResetScheduler();
  }
  return taskResetScheduler;
}

function taskUiSinks({ renderOpenTasks = true } = {}) {
  return {
    getTasks: () => state.tasks,
    setTasks: (tasks) => { state.tasks = tasks; },
    setResetAt: (v) => { taskResetAt = v; },
    afterApply: () => {
      ui.updateHUD(state);
      if (renderOpenTasks && isTasksModalOpen()) ui.renderTasks(state, taskRemainMs());
    },
  };
}

function taskRemainMs() {
  if (!taskResetAt) return null;
  return Math.max(0, taskResetAt - Date.now());
}

function isTasksModalOpen() {
  return ui.isPanelOpen('tasks');
}

const hourMs = () => TIME_SCALES[state.timeScale].hourMs;
const fertilizerKeyToId = (key) => ({ normal: 1, fast: 2, super: 3 }[key] || 0);

const TOOLS_HOME = [
  { id: 'plant', name: '播种', icon: '🌱' },
  { id: 'harvest', name: '收获', icon: '🧺' },
  { id: 'till', name: '锄地', icon: '⛏️' },
  { id: 'water', name: '浇水', icon: '💧' },
  { id: 'fert', name: '施肥', icon: '🧪' },
  { id: 'weed', name: '除草', icon: '🌿' },
  { id: 'pest', name: '除虫', icon: '🐛' },
];
const TOOLS_VISIT = [
  { id: 'water', name: '浇水', icon: '💧' },
  { id: 'weed', name: '除草', icon: '🌿' },
  { id: 'pest', name: '除虫', icon: '🐛' },
  { id: 'steal', name: '偷菜', icon: '🥷' },
];
// ---------------- 通用辅助 ----------------
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

const CODEX_TIER_LABEL = Object.freeze({
  bronze: '铜牌',
  silver: '银牌',
  gold: '金牌',
});

function harvestSuccessText(payload) {
  const progress = payload?.patch?.codex_progress;
  const cropKey = cropIdToKey(progress?.crop_id);
  const cropName = CROP_MAP[cropKey]?.name || '作物';
  const rewards = Array.isArray(payload?.codex_rewards) ? payload.codex_rewards : [];
  if (rewards.length > 0) {
    const latest = rewards[rewards.length - 1];
    const tier = CODEX_TIER_LABEL[latest?.tier] || '新阶段';
    const reward = Number(latest?.reward_coin) || 0;
    return `${cropName}图鉴升级为${tier}，💰${reward} 奖励已发邮箱`;
  }
  if (Number(progress?.harvest_count) === 1) {
    return `📖 图鉴解锁：${cropName}`;
  }
  return '收获成功';
}

function refreshFarmMirror() {
  const visiting = isVisitingFriend();
  if (visiting && activeTool && !isVisitTool(activeTool)) activeTool = null;
  if (!visiting && activeTool === 'steal') activeTool = null;
  ui.setReadOnly?.(visiting);
  ui.setVisitor?.(visiting
    ? (session.viewingOwnerName || `UID ${session.viewingOwnerUid}`)
    : null);
  refreshToolbar();
  refreshSubBar();
  syncAllPlots();
  ui.updateHUD(state);
  if (!visiting) void refreshPetStatus();
}

const healthOf = (plot) => Math.max(0, Math.min(100, 100 - plot.penalty));
const yieldFactor = (h) => YIELD_FLOOR + (1 - YIELD_FLOOR) * (h / 100);
const actualYield = (crop, h) => Math.floor(crop.yield * yieldFactor(h));

function stageName(crop, stage) {
  return (stageCount(crop) === 3 ? STAGE_NAMES_3 : STAGE_NAMES_4)[stage] || '';
}

// ---------------- 农事反馈 ----------------
function fail(msg) { sfx.error(); ui.toast(msg, 'err'); }
function ok(msg, type = 'ok') { ui.toast(msg, type); }

// ---------------- online 入口（登录页 authFlow / DEV 诊断） ----------------
function reconnectRestoreDeps(client) {
  return {
    client,
    session,
    state,
    applyPatch,
    setFarmView,
    leaveOnline,
    setOnlineBusy: (busy) => {
      onlineBusy = busy;
    },
    getSelfUid: () => client?.uid ?? netClient?.uid,
    refreshUI: () => {
      refreshFarmMirror();
      if (!isVisitingFriend()) void refreshPetStatus();
    },
    onRestored: () => {
      // 重连成功：即使复用 client 也使旧 task 请求失效，再重建列表/timer
      invalidateTaskSession();
      void refreshTasks();
    },
    toast: (msg, type) => ui.toast(msg, type),
    fail,
    errText,
    onOfflineCleanup: (reason) => {
      stopDeltaSubscription?.();
      stopDeltaSubscription = null;
      stopPlayerDeltaSubscription?.();
      stopPlayerDeltaSubscription = null;
      stopMailNotifySubscription?.();
      stopMailNotifySubscription = null;
      stopTaskNotifySubscription?.();
      stopTaskNotifySubscription = null;
      disposeTaskRuntime();
      farmMirror = null;
      netClient = null;
      reconnectBinding = null;
      if (Number(reason?.err) === 1105) {
        logout();
        location.assign('/login?error=1105');
      }
    },
  };
}

/**
 * 让当前模块接管已有的在线客户端。
 * @param {import('../net/client.js').NetClient} client
 */
function bindOnlineClient(client) {
  netClient = client;
  // 新登录绑定：使旧 task 请求/会话失效
  invalidateTaskSession();
  reconnectBinding = bindFarmReconnectRestore(reconnectRestoreDeps(client));
  farmMirror?.dispose?.();
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
    if (deltaEnv.payload?.pet) applyAuthoritativePetStatus(deltaEnv.payload.pet);
    ui.updateHUD(state);
    refreshSubBar();
  });
  stopMailNotifySubscription?.();
  stopMailNotifySubscription = client.onMailNotify(() => {
    // 申请/同意/拒绝到达：按需拉列表，点亮或熄灭侧栏红点
    void refreshFriendRequests();
    void refreshMails();
  });
  stopTaskNotifySubscription?.();
  stopTaskNotifySubscription = client.onTaskNotify((env) => {
    // 只合并任务权威状态；提升 push epoch，避免晚到 TaskList 覆盖
    ensureTaskSession().applyTaskNotify(env.payload, taskUiSinks());
  });
}

function enterOnlineFromNet(client, enterEnv) {
  if (!client?.uid || !client?.token) {
    throw new Error('enterOnlineFromNet: missing uid/token');
  }
  if (!enterEnv || enterEnv.err !== 0) {
    throw new Error(`applyAuthoritativeFarmEnter: enterFarm err=${enterEnv?.err}`);
  }
  enterOnline({ uid: client.uid, token: client.token });
  bindOnlineClient(client);
  applyAuthoritativeFarmEnter(reconnectRestoreDeps(client), enterEnv, {
    toast: '已进入 online 模式：操作将发往服务端',
  });
  // 清掉登录前本地灌入的假邮件/任务，避免右侧红点误报；真实列表按需拉取
  state.mails = [];
  state.tasks = [];
  state.friendRequests = [];
  ui.updateHUD(state);
  void refreshMails();
  void refreshTasks();
  void refreshFriendRequests();
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
    const snapshot = response.payload?.snapshot || {};
    const friend = Array.isArray(state.friends)
      ? state.friends.find((f) => Number(f.uid) === Number(ownerUid || snapshot.owner_uid))
      : null;
    applyAuthoritativeFarmEnter(reconnectRestoreDeps(netClient), response, {
      fallbackOwnerUid: ownerUid || netClient.uid,
      ownerName: nickname || snapshot.nickname || friend?.nickname || '',
    });
    if (isVisitingFriend()) {
      ui.toast('已进入好友农场，可浇水/除草/除虫/偷菜', 'info');
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
    case 'steal':
      sfx.steal();
      scene.harvestAnim(plotId);
      break;
    default:
      break;
  }
}

/** online：发意图 → 等 Rsp → applyPatch；失败 toast 不改地块；偷菜不做数量乐观预测 */
async function onPlotClickOnline(plotId) {
  if (onlineBusy || !netClient) return;
  const farm = currentFarm();
  const plot = farm.plots[plotId];
  if (!plot || plotId >= farm.unlocked) return;

  if (!activeTool) return showPlotTip(plotId);
  if (isVisitingFriend() && !isVisitTool(activeTool)) {
    return fail('好友农场仅可浇水、除草、除虫或偷菜');
  }

  const cmd = plotCmdForTool(activeTool, plot.state);
  if (cmd == null) {
    if (activeTool === 'till') return fail('这块地不需要锄地');
    if (activeTool === 'steal') return fail('这块地没有可偷的成熟作物');
    return showPlotTip(plotId);
  }

  const ownerUid = isVisitingFriend() ? Number(session.viewingOwnerUid) || 0 : 0;

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
    let rsp;
    if (cmd === CMD_STEAL) {
      const cropId = cropKeyToId(plot.cropId);
      if (!cropId) return fail('作物未知，无法偷菜');
      rsp = await netClient.steal(ownerUid, plotId, cropId);
    } else {
      rsp = await netClient.plotAction(cmd, plotId, arg, ownerUid);
    }

    if (rsp.err === 1411) {
      // 被狗拦截：有副作用（赔付），展示赔付金额；金币由 PlayerDelta 刷新
      const compensation = Number(rsp.payload?.compensation) || 0;
      sfx.dog();
      fail(compensation > 0
        ? `被看家狗抓住了，赔付 💰${compensation}`
        : errText(rsp.err));
      ui.updateHUD(state);
      return;
    }
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
    if (cmd === CMD_STEAL) {
      const amount = Number(rsp.payload?.amount) || 0;
      ok(amount > 0 ? `偷菜成功 ×${amount}` : '偷菜成功', 'gold');
    } else if (isVisitingFriend()) {
      const exp = Number(rsp.payload?.exp_gained) || 0;
      const coin = Number(rsp.payload?.coin_gained) || 0;
      const bits = [];
      if (exp) bits.push(`+${exp} 经验`);
      if (coin) bits.push(`+${coin} 金币`);
      ok(bits.length ? `互助成功 ${bits.join(' · ')}` : '互助成功');
    } else {
      ok(cmd === CMD_HARVEST ? harvestSuccessText(rsp.payload) : '操作成功');
    }
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
      const name = c?.name ?? '作物';
      return `<h4>🥀 ${name}（枯萎）</h4><div class="row"><span>状态</span><b>产量全失，需清理</b></div>`;
    }
    case PLOT.GROWING:
    case PLOT.MATURE: {
      const c = cropOf(plot);
      const { stage, total } = stageOf(plot, now);
      const h = Math.round(healthOf(plot));
      const dry = now > plot.waterUntil;
      const mature = plot.state === PLOT.MATURE;
      const yieldEst = plot.finalYield > 0 ? plot.finalYield : actualYield(c, h);
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
    const plot = farm.plots[i];
    const info = computePlotInfo(plot, { unlocked: i < farm.unlocked, index: i, now });
    scene.updatePlot(g, info);
  });
}

// ---------------- 工具选择 ----------------
function refreshToolbar() {
  const tools = isVisitingFriend() ? TOOLS_VISIT : TOOLS_HOME;
  ui.renderToolbar(tools, activeTool);
}

function applyAuthoritativePetStatus(status) {
  if (!applyPetStatus(state, status)) return;
  ui.updateHUD(state);
}

async function refreshPetStatus() {
  if (!netClient || isVisitingFriend()) return false;
  try {
    const response = await netClient.petStatus();
    if (response.err !== 0) return false;
    applyAuthoritativePetStatus(response.payload);
    return true;
  } catch {
    return false;
  }
}

function mapServerMails(mails) {
  return (Array.isArray(mails) ? mails : []).map((m) => {
    const gold = Number(m.attachment_coin) || 0;
    const claimed = !!m.claimed;
    return {
      id: Number(m.id),
      title: m.title || '系统邮件',
      content: '',
      gold,
      attachmentCoin: gold,
      exp: 0,
      claimed,
      read: m.read === true,
      time: Number(m.created_at) || Date.now(),
    };
  });
}

/**
 * 仅拉取并在上下文有效时应用；不排程、不 toast。
 * @returns {Promise<import('./taskSession.js').TaskListApplyOutcome>}
 */
async function pullAndApplyTasks({ renderOpenTasks = true, force = false } = {}) {
  const client = netClient;
  if (!client) {
    return { applied: false, ok: false, resetAt: 0, contextValid: false, reason: 'no_client' };
  }
  const session = ensureTaskSession();
  const sinks = taskUiSinks({ renderOpenTasks });
  return session.refreshTaskList({
    client,
    force,
    getCurrentClient: () => netClient,
    fetch: async (c) => {
      const response = await c.taskList();
      if (response.err !== 0) return { ok: false, err: response.err };
      return {
        ok: true,
        tasks: response.payload?.tasks,
        resetAt: Number(response.payload?.reset_at) || 0,
      };
    },
    getTasks: sinks.getTasks,
    setTasks: sinks.setTasks,
    setResetAt: sinks.setResetAt,
    afterApply: sinks.afterApply,
  });
}

/**
 * 拉取 TaskList：网络结果先不落地；仅 success/failure 改 timer，stale 不动。
 * @returns {Promise<boolean>}
 */
async function refreshTasks({ renderOpenTasks = true, force = false } = {}) {
  const scheduler = ensureTaskResetScheduler();
  const outcome = await pullAndApplyTasks({ renderOpenTasks, force });
  const result = taskRefreshResultFromOutcome(outcome);

  if (result.status === 'failure') {
    if (result.err != null) fail(errText(result.err));
    else if (result.error) fail(result.error instanceof Error ? result.error.message : String(result.error));
  }

  if (taskResetScheduler === scheduler && !scheduler.disposed) {
    applyTaskListSchedule(scheduler, result);
  }
  return result.status === 'success';
}

async function refreshMails() {
  if (!netClient) return false;
  try {
    const response = await netClient.mailList();
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    state.mails = mapServerMails(response.payload?.mails);
    ui.updateHUD(state);
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function refreshCodex() {
  if (!netClient) return false;
  try {
    const response = await netClient.codexList();
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    state.codex = [];
    state.codexProgress = {};
    for (const entry of Array.isArray(response.payload?.entries) ? response.payload.entries : []) {
      applyCodexProgress(state, entry);
    }
    if (ui.isPanelOpen('codex')) ui.renderCodex(state);
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function markAllMailsRead() {
  if (!netClient || !state.mails.some((mail) => !mail.read)) return true;
  try {
    const response = await netClient.mailReadAll();
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    state.mails.forEach((mail) => { mail.read = true; });
    ui.updateHUD(state);
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function openAndRefreshMails() {
  const [mailOK] = await Promise.all([refreshMails(), refreshFriendRequests()]);
  if (mailOK) await markAllMailsRead();
  if (ui.isPanelOpen('mail')) ui.renderMail(state);
}

async function clearAllMails() {
  if (!netClient) return false;
  try {
    const response = await netClient.mailDeleteAll();
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    const affected = Math.max(0, Number(response.payload?.affected) || 0);
    await refreshMails();
    const protectedCount = state.mails.filter((mail) =>
      !mail.claimed && Number(mail.gold || mail.attachmentCoin) > 0
    ).length;
    ui.updateHUD(state);
    if (ui.isPanelOpen('mail')) ui.renderMail(state);
    ui.toast(protectedCount > 0
      ? `已清理 ${affected} 封邮件，${protectedCount} 封未领取奖励已保留`
      : '邮件已全部清空', 'ok');
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
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

/** 比较 uid（雪花经 JSON 后可能丢精度，同端两侧通常同为 Number）。 */
function sameUid(a, b) {
  if (a == null || b == null) return false;
  return String(a) === String(b);
}

async function refreshFriendRequests() {
  if (!netClient) return [];
  try {
    const response = await netClient.listFriendRequests();
    if (response.err !== 0) {
      fail(errText(response.err));
      return [];
    }
    const requests = Array.isArray(response.payload?.requests) ? response.payload.requests : [];
    state.friendRequests = requests;
    ui.updateHUD(state);
    return requests;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return [];
  }
}

/** 搜索用户（精确用户名）；邀请链接走 acceptInvite。不返回自己。 */
async function searchNeighbors(value) {
  if (!netClient) return { ok: false, users: [] };
  const selfUid = session.uid ?? netClient.uid;
  const input = value.trim();
  if (!input) {
    fail('请输入用户名');
    return { ok: false, users: [] };
  }
  const token = inviteTokenFromInput(input);
  if (token) {
    try {
      const response = await netClient.acceptInvite(token);
      if (response.err !== 0) {
        fail(errText(response.err));
        return { ok: false, users: [] };
      }
      await refreshFriends();
      ui.toast('已通过邀请成为好友', 'ok');
      return { ok: true, users: [], invited: true };
    } catch (error) {
      fail(error instanceof Error ? error.message : String(error));
      return { ok: false, users: [] };
    }
  }
  try {
    // 纯数字 UID：直接作为搜索结果卡片，发申请（排除自己）
    const peerUid = Number(input);
    if (Number.isSafeInteger(peerUid) && peerUid > 0 && String(peerUid) === input) {
      if (sameUid(peerUid, selfUid)) {
        fail('不能添加自己为好友');
        return { ok: true, users: [] };
      }
      return { ok: true, users: [{ uid: peerUid, nickname: `UID ${peerUid}` }] };
    }
    const search = await netClient.searchUser(input);
    if (search.err !== 0) {
      // 未找到：交给结果区空态，不弹 toast
      if (search.err === 1413) return { ok: true, users: [] };
      fail(errText(search.err));
      return { ok: false, users: [] };
    }
    // 兼容旧 Rsp { uid, nickname } 与新 Rsp { users: [...] }
    let users = Array.isArray(search.payload?.users) ? search.payload.users : [];
    if (!users.length && search.payload?.uid) {
      users = [{ uid: search.payload.uid, nickname: search.payload.nickname || '' }];
    }
    users = users.filter((u) => !sameUid(u.uid, selfUid));
    return { ok: true, users };
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return { ok: false, users: [] };
  }
}

async function requestFriend(peerUid) {
  if (!netClient) return false;
  if (sameUid(peerUid, session.uid ?? netClient.uid)) {
    fail('不能添加自己为好友');
    return false;
  }
  try {
    const response = await netClient.requestFriend(peerUid);
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    await refreshFriends();
    ui.toast('好友申请已发送', 'ok');
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function acceptFriendRequest(fromUid) {
  if (!netClient) return false;
  try {
    const response = await netClient.acceptFriendRequest(fromUid);
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    await Promise.all([refreshFriends(), refreshFriendRequests(), refreshMails()]);
    ui.toast('已同意好友申请', 'ok');
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function rejectFriendRequest(fromUid) {
  if (!netClient) return false;
  try {
    const response = await netClient.rejectFriendRequest(fromUid);
    if (response.err !== 0) {
      fail(errText(response.err));
      return false;
    }
    await Promise.all([refreshFriendRequests(), refreshMails()]);
    ui.toast('已拒绝申请', 'info');
    return true;
  } catch (error) {
    fail(error instanceof Error ? error.message : String(error));
    return false;
  }
}

async function addFriend(value) {
  const result = await searchNeighbors(value);
  if (result.invited) return true;
  if (!result.ok || !result.users?.length) return false;
  return requestFriend(result.users[0].uid);
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
      case 'tasks':
        openTasksPanel({
          render: () => ui.renderTasks(state, taskRemainMs()),
          refresh: () => refreshTasks({ renderOpenTasks: false }),
          isPanelOpen: (panel) => ui.isPanelOpen(panel),
        });
        break;
      case 'codex':
        ui.renderCodex(state);
        void refreshCodex();
        break;
      case 'mail':
        ui.renderMail(state);
        void openAndRefreshMails();
        break;
      case 'pet':
        ui.renderPet(state);
        void refreshPetStatus().then((ok) => {
          if (ok && ui.modalOpen) ui.renderPet(state);
        });
        break;
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

  async onBuyFood(g) {
    if (!isOnline()) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('好友农场无法购买');
    if (onlineBusy || !netClient) return;
    const grams = g < 0 ? Math.max(1, DOG_BOWL_CAP - Math.floor(state.dogBowl)) : g;
    if (grams <= 0) return ok('狗盆已满');
    onlineBusy = true;
    try {
      const rsp = await netClient.buy(DOG_FOOD_SHOP_ITEM_ID, grams);
      if (rsp.err !== 0) return fail(errText(rsp.err));
      applyResponsePatch(rsp.payload);
      sfx.gold();
      ui.toast(`购买狗粮 ${grams}g`, 'ok');
      ui.updateHUD(state);
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    } finally {
      onlineBusy = false;
    }
  },

  async onBuyDog(id) {
    if (!isOnline()) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('好友农场无法购买');
    if (onlineBusy || !netClient) return;
    const dog = DOGS.find((d) => d.id === id);
    if (!dog?.shopItemId || !dog.dogType) return fail('该狗种暂未上架');
    onlineBusy = true;
    try {
      const buy = await netClient.buy(dog.shopItemId, 1);
      if (buy.err !== 0) return fail(errText(buy.err));
      applyResponsePatch(buy.payload);
      const act = await netClient.petActivate(dog.dogType);
      if (act.err !== 0) {
        fail(errText(act.err));
        await refreshPetStatus();
        return;
      }
      applyAuthoritativePetStatus(act.payload);
      sfx.dog();
      ui.toast(`已购买并启用 ${dog.name}`, 'ok');
      ui.updateHUD(state);
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    } finally {
      onlineBusy = false;
    }
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

  async onClaimMail(id) {
    if (!isOnline() || !netClient) return fail('请先登录后再操作');
    if (onlineBusy) return;
    onlineBusy = true;
    try {
      const rsp = await netClient.mailClaim(id);
      if (rsp.err !== 0) return fail(errText(rsp.err));
      const reward = applyMailClaimReceipt(state, id, rsp.payload);
      sfx.gold();
      ui.toast(reward > 0 ? `领取 💰${reward}` : '附件已领取', 'gold');
      ui.updateHUD(state);
      await refreshMails();
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    } finally {
      onlineBusy = false;
    }
  },

  async onClearMails() {
    if (!isOnline() || !netClient) return fail('请先登录后再操作');
    if (onlineBusy) return false;
    onlineBusy = true;
    try {
      return await clearAllMails();
    } finally {
      onlineBusy = false;
    }
  },

  async onClaimTask(taskId) {
    if (!isOnline() || !netClient) return fail('请先登录后再操作');
    if (onlineBusy) return;
    onlineBusy = true;
    try {
      const rsp = await netClient.taskClaim(taskId);
      if (rsp.err !== 0) return fail(errText(rsp.err));
      const rewardCoin = Number(rsp.payload?.coin) || 0;
      ui.toast(rewardCoin > 0 ? `任务奖励已到账 💰${rewardCoin}` : '任务奖励已到账', 'gold');
      await refreshTasks({ force: true });
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    } finally {
      onlineBusy = false;
    }
  },

  async onFeedPet(grams) {
    if (!isOnline() || !netClient) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('只能在自己的农场喂狗');
    if (onlineBusy) return;
    onlineBusy = true;
    try {
      const beforeBowl = Number(state.dogBowl) || 0;
      const rsp = await netClient.petFeed(grams);
      if (rsp.err !== 0) return fail(errText(rsp.err));
      applyAuthoritativePetStatus(rsp.payload);
      const afterBowl = Number(rsp.payload?.bowl_grams) || 0;
      const fed = Math.max(0, afterBowl - beforeBowl);
      // PetFeed Rsp 不含背包；按盆增量本地扣减狗粮展示
      state.inventory.dogFood = Math.max(0, (state.inventory.dogFood || 0) - (fed || grams));
      sfx.click();
      ui.toast(`已喂食 ${fed || grams}g`, 'ok');
      ui.updateHUD(state);
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    } finally {
      onlineBusy = false;
    }
  },

  async onActivatePet(dogKey) {
    if (!isOnline() || !netClient) return fail('请先登录后再操作');
    if (isVisitingFriend()) return fail('只能在自己的农场启用狗');
    const dog = DOGS.find((d) => d.id === dogKey);
    if (!dog?.dogType) return fail('该狗种暂未开放');
    if (onlineBusy) return;
    onlineBusy = true;
    try {
      const rsp = await netClient.petActivate(dog.dogType);
      if (rsp.err !== 0) return fail(errText(rsp.err));
      applyAuthoritativePetStatus(rsp.payload);
      sfx.dog();
      ui.toast(`已启用 ${dog.name}`, 'ok');
    } catch (e) {
      fail(e instanceof Error ? e.message : String(e));
    } finally {
      onlineBusy = false;
    }
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

  onSearchNeighbors(value) {
    return searchNeighbors(value);
  },

  onRequestFriend(peerUid) {
    return requestFriend(peerUid);
  },

  onListFriendRequests() {
    return refreshFriendRequests();
  },

  onAcceptFriendRequest(fromUid) {
    return acceptFriendRequest(fromUid);
  },

  onRejectFriendRequest(fromUid) {
    return rejectFriendRequest(fromUid);
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
  onLogout() {
    disposeTaskRuntime();
    stopTaskNotifySubscription?.();
    stopTaskNotifySubscription = null;
    netClient?.close();
    netClient = null;
    logout();
    location.assign('/login');
  },
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

  // 期 3：不做本地权威地块推进 / NPC 假好友写；镜像仅由 snapshot/Rsp/Delta 更新

  // 在线仅根据权威到期时间插值显示，不产生库存或服务端状态写入。
  if (isOnline() && state.dog && state.dogBowlEmptyAt > 0 && state.dogMsPerGram > 0) {
    const previous = state.dogBowl;
    state.dogBowl = Math.max(0, Math.floor((state.dogBowlEmptyAt - now) / state.dogMsPerGram));
    if (state.dogBowl !== previous) ui.updateHUD(state);
    if (previous > 0 && state.dogBowl <= 0) ui.toast('🐶 狗粮吃完了，看家狗罢工了！', 'err');
  } else if (!isOnline() && state.dog && state.dogBowl > 0) {
    const dogDef = DOGS.find(d => d.id === state.dog.id);
    state.dogBowl = Math.max(0, state.dogBowl - (dogDef.consumption / hourMs()) * dt);
    if (state.dogBowl <= 0) ui.toast('🐶 狗粮吃完了，看家狗罢工了！', 'err');
  }

  // 日夜循环：跟全局逻辑日相位，各客户端同刻同天空
  const phase = logicDayPhase(now, state.timeScale);
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
const onGlobalPointerMove = (e) => { lastMouse[0] = e.clientX; lastMouse[1] = e.clientY; };
addEventListener('pointermove', onGlobalPointerMove);

// ---------------- 启动 ----------------
// 登录页 authFlow / DEV 诊断：暴露 online 切入与状态
window.__farm = {
  __runtime: runtimeHandle,
  getState: () => state,
  scene,
  enterOnlineFromNet,
  isOnline,
  getNetClient: () => netClient,
};
refreshToolbar();
syncAllPlots();
scene.start();
const tickIntervalId = setInterval(tick, 300);
tick();

// 页面卸载：清 tick / pointermove / scene，并保留网络关闭与 reconnect cleanup
removePageUnload = bindPageUnload({
  addEventListener,
  removeEventListener,
  clearInterval,
  tickIntervalId,
  onPointerMove: onGlobalPointerMove,
  getReconnectBinding: () => reconnectBinding,
  setReconnectBinding: (v) => { reconnectBinding = v; },
  scene,
  getNetClient: () => netClient,
  onCleanup: () => {
    disposeTaskRuntime();
    stopTaskNotifySubscription?.();
    stopTaskNotifySubscription = null;
  },
});

// 热更新时保留在线状态，避免新模块从默认金币/经验开始渲染。
if (hmrRuntime?.netClient && isOnline()) {
  bindOnlineClient(hmrRuntime.netClient);
  refreshFarmMirror();
  void refreshTasks();
}

function disposeRuntimeForHMR() {
  if (runtimeDisposed) return;
  runtimeDisposed = true;
  // 停掉旧模块的 tick：否则 HMR 后新旧实例都会每 300ms 写同一组 HUD，
  // 旧实例用旧 state、新实例用新 state，金币/经验就会反复跳动。
  clearInterval(tickIntervalId);
  disposeTaskRuntime();
  stopDeltaSubscription?.();
  stopDeltaSubscription = null;
  stopPlayerDeltaSubscription?.();
  stopPlayerDeltaSubscription = null;
  stopMailNotifySubscription?.();
  stopMailNotifySubscription = null;
  stopTaskNotifySubscription?.();
  stopTaskNotifySubscription = null;
  reconnectBinding?.dispose?.();
  reconnectBinding = null;
  farmMirror?.dispose?.();
  farmMirror = null;
  removePageUnload?.();
  removePageUnload = null;
  removeEventListener('pointermove', onGlobalPointerMove);
  scene.dispose();
  // HMR 交接时故意不关闭 netClient：新模块会继续接管它。
  if (window.__farm?.__runtime === runtimeHandle) {
    delete window.__farm;
  }
  if (window.__farmActiveRuntime?.runtime === runtimeHandle) {
    delete window.__farmActiveRuntime;
  }
}

if (import.meta.hot) {
  window.__farmActiveRuntime = {
    runtime: runtimeHandle,
    dispose: disposeRuntimeForHMR,
    snapshot: () => ({ state, netClient }),
  };
  import.meta.hot.dispose(() => {
    import.meta.hot.data.runtime = { state, netClient };
    disposeRuntimeForHMR();
  });
}

// 未 online 时不提示本地开局指引（须登录）；online 后由 enterOnlineFromNet toast
