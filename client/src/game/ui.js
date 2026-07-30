// ============================================================
// DOM UI：顶栏、工具栏、模态面板、tooltip、toast
// ============================================================
import { CROP_MAP, CROPS, FERTILIZERS, DOGS, CODEX_MILESTONES, EXPANSION, TIME_SCALES, DOG_BOWL_CAP, MAX_PLOTS } from './config.js';
import { levelOf, expProgress } from './state.js';
import { EXP_PER_LEVEL } from './config.js';
import { mailDotVisible, taskDotVisible } from './sideDots.js';
import { taskCardViewModel } from './taskCardView.js';

const $ = (s) => document.querySelector(s);

// 颜色加深，用于徽章渐变
function shade(hex, amt) {
  const n = parseInt(hex.slice(1), 16);
  const f = (v) => Math.max(0, Math.min(255, v + amt));
  return `#${((f(n >> 16) << 16) | (f((n >> 8) & 255) << 8) | f(n & 255)).toString(16).padStart(6, '0')}`;
}

export function badgeHTML(def, sm = false) {
  return `<span class="crop-badge ${sm ? 'sm' : ''}" style="background:linear-gradient(135deg, ${shade(def.color, 22)}, ${shade(def.color, -34)})">${def.name[0]}</span>`;
}

export const fmtTime = (ms) => {
  if (ms <= 0) return '00:00';
  const s = Math.ceil(ms / 1000);
  const m = Math.floor(s / 60), r = s % 60;
  if (m >= 60) { const h = Math.floor(m / 60); return `${h}:${String(m % 60).padStart(2, '0')}:${String(r).padStart(2, '0')}`; }
  return `${String(m).padStart(2, '0')}:${String(r).padStart(2, '0')}`;
};

/** 返回影响 HUD 的可见状态，避免游戏 tick 重复写入相同 DOM。 */
export function hudSignature(state, readOnly) {
  const gold = Math.floor(Number(state.gold) || 0);
  const exp = Number(state.exp) || 0;
  const dogBowl = Math.floor(Number(state.dogBowl) || 0);
  const mailDot = mailDotVisible(state.mails, state.friendRequests);
  const taskDot = taskDotVisible(state.tasks);
  const lv = levelOf(exp);
  const next = EXPANSION.find(e => e[0] === state.unlockedPlots + 1);
  const canExpand = !readOnly && !!next && lv >= next[1] && gold >= next[2];
  return [gold, exp, state.dog?.id || '', dogBowl, mailDot, taskDot, canExpand].join('|');
}

export class UI {
  constructor(cb) {
    this.cb = cb;               // { onTool, onSubSelect, onPanel, onBuy..., onVisit, onBackHome, onExpand, onSetting }
    this.activeTool = null;
    this.activePanel = null;
    this.readOnly = false;
    this.lastHUDSignature = null;
    this.toastWrap = $('#toast-wrap');
    this.tooltip = $('#tooltip');

    $('#modal-close').onclick = () => this.closeModal();
    $('#modal-mask').addEventListener('click', (e) => { if (e.target.id === 'modal-mask') this.closeModal(); });
    // Esc 关闭当前模态（商店/邻里簿/邮箱等共用 #modal-mask）
    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' && e.code !== 'Escape') return;
      if (!this.modalOpen) return;
      e.preventDefault();
      this.closeModal();
    });
    $('#btn-settings').onclick = () => this.renderSettings();
    $('#btn-back-home').onclick = () => cb.onBackHome();
    $('#btn-expand').onclick = () => cb.onExpand();
    $('#dog-status')?.addEventListener('click', () => { this.cb.onPanel?.('pet'); });

    document.querySelectorAll('#side-menu button').forEach(b => {
      b.onclick = () => { this.cb.onPanel(b.dataset.panel); };
    });
  }

  // ---------------- HUD ----------------
  updateHUD(state) {
    const signature = hudSignature(state, this.readOnly);
    if (signature === this.lastHUDSignature) return;
    this.lastHUDSignature = signature;

    const gold = Math.floor(Number(state.gold) || 0);
    const exp = Number(state.exp) || 0;
    $('#gold-num').textContent = gold.toLocaleString();
    const lv = levelOf(exp);
    $('#level-badge').textContent = `Lv.${lv}`;
    $('#exp-fill').style.width = `${expProgress(exp) * 100}%`;
    $('#exp-text').textContent = `${exp % EXP_PER_LEVEL}/${EXP_PER_LEVEL}`;
    const dogChip = $('#dog-status');
    if (state.dog) {
      dogChip.classList.remove('hidden');
      $('#dog-food-num').textContent = `${Math.floor(state.dogBowl)}g`;
      dogChip.style.opacity = state.dogBowl > 0 ? 1 : 0.5;
      dogChip.style.cursor = 'pointer';
      dogChip.title = '点击管理看家狗';
    } else dogChip.classList.add('hidden');
    $('#dot-mail').classList.toggle('hidden', !mailDotVisible(state.mails, state.friendRequests));
    $('#dot-tasks').classList.toggle('hidden', !taskDotVisible(state.tasks));
    // 扩地按钮
    const next = EXPANSION.find(e => e[0] === state.unlockedPlots + 1);
    const canExpand = !this.readOnly && !!next && lv >= next[1] && gold >= next[2];
    $('#btn-expand').classList.toggle('hidden', !canExpand);
  }

  setClock(icon) { $('#clock-chip').textContent = icon; }

  // ---------------- 工具栏 ----------------
  renderToolbar(tools, active) {
    this.activeTool = active;
    const bar = $('#toolbar');
    bar.innerHTML = '';
    for (const t of tools) {
      const b = document.createElement('button');
      b.className = 'tool-btn' + (active === t.id ? ' active' : '');
      b.innerHTML = `<i>${t.icon}</i><span>${t.name}</span>`;
      b.onclick = () => this.cb.onTool(t.id);
      bar.appendChild(b);
    }
  }

  /** 访客农场隐藏主人专属写入入口；互助/偷菜工具仍由 toolbar 渲染。 */
  setReadOnly(readOnly) {
    this.readOnly = readOnly;
    $('#side-menu button[data-panel="shop"]').classList.toggle('hidden', readOnly);
    $('#btn-expand').classList.toggle('hidden', readOnly);
  }

  showSubBar(items, activeId, onPick) {
    const bar = $('#sub-bar');
    if (!items) { bar.classList.add('hidden'); bar.innerHTML = ''; return; }
    bar.classList.remove('hidden');
    bar.innerHTML = '';
    if (!items.length) {
      bar.innerHTML = `<div style="padding:4px 12px;font-size:13px;color:var(--ink-soft);font-weight:700;">暂无可用道具，请先去商店购买</div>`;
      return;
    }
    for (const it of items) {
      const el = document.createElement('div');
      el.className = 'sub-item' + (it.id === activeId ? ' active' : '') + (it.count <= 0 ? ' disabled' : '');
      el.innerHTML = `${it.badge || `<span style="font-size:22px">${it.icon || ''}</span>`}<div><div style="font-weight:800;font-size:13px">${it.name}</div><div class="cnt">×${it.count}</div></div>`;
      if (it.count > 0) el.onclick = () => onPick(it.id);
      bar.appendChild(el);
    }
  }

  // ---------------- 提示 ----------------
  toast(msg, type = 'ok') {
    const el = document.createElement('div');
    el.className = `toast ${type}`;
    el.textContent = msg;
    this.toastWrap.appendChild(el);
    setTimeout(() => el.remove(), 2500);
    while (this.toastWrap.children.length > 4) this.toastWrap.firstChild.remove();
  }

  showTooltip(html, x, y) {
    const t = this.tooltip;
    if (!html) { t.classList.add('hidden'); return; }
    t.innerHTML = html;
    t.classList.remove('hidden');
    const w = t.offsetWidth, h = t.offsetHeight;
    let px = x + 18, py = y + 14;
    if (px + w > innerWidth - 12) px = x - w - 14;
    if (py + h > innerHeight - 12) py = y - h - 12;
    t.style.left = px + 'px'; t.style.top = py + 'px';
  }

  // ---------------- 模态 ----------------
  openModal(title, panel) {
    this.activePanel = panel || null;
    $('#modal-title').textContent = title;
    const body = $('#modal-body');
    body.innerHTML = '';
    body.className = '';
    $('#modal')?.classList.toggle('modal--neighbors', /邻里/.test(title));
    this.showTooltip(null);
    $('#modal-mask').classList.remove('hidden');
    return body;
  }
  closeModal() {
    this.activePanel = null;
    $('#modal-mask').classList.add('hidden');
  }
  get modalOpen() { return !$('#modal-mask').classList.contains('hidden'); }
  isPanelOpen(panel) { return this.modalOpen && this.activePanel === panel; }

  // ---------------- 商店 ----------------
  renderShop(state, tab = 'seeds') {
    const body = this.openModal('🛒 商店', 'shop');
    const tabs = document.createElement('div');
    tabs.className = 'tabs';
    const defs = [['seeds', '种子'], ['fert', '化肥'], ['food', '狗粮'], ['dog', '看家狗']];
    for (const [id, name] of defs) {
      const b = document.createElement('button');
      b.textContent = name;
      b.className = id === tab ? 'active' : '';
      b.onclick = () => this.renderShop(state, id);
      tabs.appendChild(b);
    }
    body.appendChild(tabs);
    const grid = document.createElement('div');
    grid.className = 'shop-grid';
    body.appendChild(grid);
    const lv = levelOf(state.exp);

    if (tab === 'seeds') {
      for (const c of CROPS.filter(c => !c.hidden)) {
        const locked = lv < c.unlock;
        const card = document.createElement('div');
        card.className = 'shop-card' + (locked ? ' locked' : '');
        card.innerHTML = `${badgeHTML(c)}<div class="name">${c.name}</div>
          <div class="meta">${c.seasons > 1 ? c.seasons + '季作物' : '单季作物'} · ${c.cycleH}h 周期<br>产量 ${c.yield}/季 · 果价 ${c.fruitPrice}</div>
          <div class="price">💰 ${c.seedPrice}</div>`;
        const btn = document.createElement('button');
        btn.className = 'buy-btn';
        btn.textContent = locked ? `Lv.${c.unlock} 解锁` : '购买';
        btn.disabled = locked || state.gold < c.seedPrice;
        btn.onclick = async () => { await this.cb.onBuySeed(c.id); this.renderShop(this.cb.getState(), tab); };
        card.appendChild(btn);
        grid.appendChild(card);
      }
    } else if (tab === 'fert') {
      for (const f of FERTILIZERS) {
        const card = document.createElement('div');
        card.className = 'shop-card';
        card.innerHTML = `<span style="font-size:34px">${f.icon}</span><div class="name">${f.name}</div>
          <div class="meta">当前阶段提速 ${f.reduceH} 小时<br>每阶段限用一次</div>
          <div class="price">💰 ${f.price}</div>`;
        const btn = document.createElement('button');
        btn.className = 'buy-btn'; btn.textContent = '购买';
        btn.disabled = state.gold < f.price;
        btn.onclick = () => { this.cb.onBuyFert(f.id); this.renderShop(state, tab); };
        card.appendChild(btn);
        grid.appendChild(card);
      }
    } else if (tab === 'food') {
      const card = document.createElement('div');
      card.className = 'shop-card';
      card.style.gridColumn = '1 / -1';
      const bagFood = state.inventory?.dogFood || 0;
      card.innerHTML = `<span style="font-size:34px">🦴</span><div class="name">狗粮</div>
        <div class="meta">1 金币 / g · 狗盆容量 ${DOG_BOWL_CAP}g<br>背包狗粮 ${bagFood}g · 狗盆 ${Math.floor(state.dogBowl)}g</div>`;
      const row = document.createElement('div');
      row.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;justify-content:center';
      for (const [label, g] of [['+50g', 50], ['+120g', 120], ['填满狗盆', -1]]) {
        const btn = document.createElement('button');
        btn.className = 'buy-btn'; btn.textContent = label;
        btn.onclick = async () => { await this.cb.onBuyFood(g); this.renderShop(this.cb.getState(), tab); };
        row.appendChild(btn);
      }
      card.appendChild(row);
      grid.appendChild(card);
    } else if (tab === 'dog') {
      for (const d of DOGS) {
        const owned = state.dog && state.dog.id === d.id;
        const locked = lv < d.unlock;
        const onlineReady = !!d.shopItemId;
        const card = document.createElement('div');
        card.className = 'shop-card' + (locked || !onlineReady ? ' locked' : '');
        card.innerHTML = `<span style="font-size:34px">🐕</span><div class="name">${d.name}</div>
          <div class="meta">拦截率 ${Math.round(d.intercept * 100)}%<br>粮耗 ${d.consumption}g/小时${!onlineReady ? '<br>期 4 暂未上架' : ''}</div>
          <div class="price">💰 ${d.price}</div>`;
        const btn = document.createElement('button');
        btn.className = 'buy-btn';
        btn.textContent = owned ? '看家中' : !onlineReady ? '暂未开放' : locked ? `Lv.${d.unlock} 解锁` : state.dog ? '替换当前狗' : '购买并启用';
        btn.disabled = owned || locked || !onlineReady || state.gold < d.price;
        btn.onclick = async () => { await this.cb.onBuyDog(d.id); this.renderShop(this.cb.getState(), tab); };
        card.appendChild(btn);
        grid.appendChild(card);
      }
    }
  }

  // ---------------- 背包 ----------------
  renderBag(state) {
    const body = this.openModal('🎒 背包', 'bag');
    const seeds = Object.entries(state.inventory.seeds).filter(([, n]) => n > 0);
    const ferts = Object.entries(state.inventory.fertilizers).filter(([, n]) => n > 0);
    const dogFood = state.inventory?.dogFood || 0;
    if (!seeds.length && !ferts.length && dogFood <= 0) {
      body.innerHTML = `<div class="empty-tip">背包空空如也<br><br>去商店买些种子吧，锄地时也可能意外发现隐藏种子 ✨</div>`;
      return;
    }
    if (seeds.length) {
      body.insertAdjacentHTML('beforeend', `<div style="font-weight:800;color:var(--brown);margin:4px 0 10px">🌱 种子</div>`);
      for (const [id, n] of seeds) {
        const c = CROP_MAP[id];
        body.insertAdjacentHTML('beforeend', `<div class="list-row">${badgeHTML(c)}
          <div class="grow"><div class="title">${c.name}${c.hidden ? ' <span style="color:#9b5de5;font-size:11px">✨隐藏</span>' : ''}</div>
          <div class="sub">${c.seasons > 1 ? c.seasons + '季' : '单季'} · 周期 ${c.cycleH}h · 产量 ${c.yield}/季</div></div>
          <b>×${n}</b></div>`);
      }
    }
    if (ferts.length) {
      body.insertAdjacentHTML('beforeend', `<div style="font-weight:800;color:var(--brown);margin:14px 0 10px">🧪 化肥</div>`);
      for (const [id, n] of ferts) {
        const f = FERTILIZERS.find(f => f.id === id);
        body.insertAdjacentHTML('beforeend', `<div class="list-row"><span style="font-size:26px">${f.icon}</span>
          <div class="grow"><div class="title">${f.name}</div><div class="sub">当前阶段提速 ${f.reduceH} 小时</div></div>
          <b>×${n}</b></div>`);
      }
    }
    if (dogFood > 0) {
      body.insertAdjacentHTML('beforeend', `<div style="font-weight:800;color:var(--brown);margin:14px 0 10px">🦴 狗粮</div>`);
      body.insertAdjacentHTML('beforeend', `<div class="list-row"><span style="font-size:26px">🦴</span>
        <div class="grow"><div class="title">狗粮</div><div class="sub">喂食看家狗</div></div>
        <b>${dogFood}g</b></div>`);
    }
  }

  // ---------------- 仓库 ----------------
  renderBarn(state) {
    const body = this.openModal('🏠 仓库', 'barn');
    const items = Object.entries(state.warehouse).filter(([, n]) => n > 0);
    const total = items.reduce((s, [id, n]) => s + n * CROP_MAP[id].fruitPrice, 0);
    if (!items.length) {
      body.innerHTML = `<div class="empty-tip">仓库里还没有果实<br><br>收获的果实会存放在这里，出售后才能换成金币 💰</div>`;
      return;
    }
    const head = document.createElement('div');
    head.style.cssText = 'display:flex;justify-content:space-between;align-items:center;margin-bottom:12px';
    head.innerHTML = `<span style="font-weight:800;color:var(--brown)">总估值 💰 ${total.toLocaleString()}</span>`;
    const sellAll = document.createElement('button');
    sellAll.className = 'act-btn green'; sellAll.textContent = '全部出售';
    sellAll.onclick = async () => { await this.cb.onSellAll(); this.renderBarn(this.cb.getState()); };
    head.appendChild(sellAll);
    body.appendChild(head);
    for (const [id, n] of items) {
      const c = CROP_MAP[id];
      const row = document.createElement('div');
      row.className = 'list-row';
      row.innerHTML = `${badgeHTML(c)}<div class="grow"><div class="title">${c.name}</div>
        <div class="sub">单价 💰${c.fruitPrice} · 小计 💰${(n * c.fruitPrice).toLocaleString()}</div></div><b>×${n}</b>`;
      const btn = document.createElement('button');
      btn.className = 'act-btn green'; btn.textContent = '出售';
      btn.onclick = async () => { await this.cb.onSell(id, n); this.renderBarn(this.cb.getState()); };
      row.appendChild(btn);
      body.appendChild(row);
    }
  }

  // ---------------- 任务 ----------------
  renderTasks(state, dayRemainMs) {
    const body = this.openModal('📋 日常任务', 'tasks');
    const head = document.createElement('div');
    head.style.cssText = 'display:flex;justify-content:space-between;align-items:center;gap:10px;margin-bottom:12px;flex-wrap:wrap';
    head.innerHTML = `<div style="font-size:12.5px;color:var(--ink-soft)">每日任务 · 完成后可直接领取奖励 💰${dayRemainMs != null ? ` · <b>${fmtTime(dayRemainMs)}</b> 后刷新` : ''}</div>`;
    body.appendChild(head);

    if (!state.tasks?.length) {
      body.insertAdjacentHTML('beforeend', `<div class="empty-tip">暂无任务</div>`);
      return;
    }
    for (const t of state.tasks) {
      const vm = taskCardViewModel(t);
      const card = document.createElement('div');
      card.className = 'task-card' + (vm.done ? ' done' : '');
      card.innerHTML = `<div class="head"><span class="name">${vm.name}</span>
        ${vm.statusTag === 'claimed' ? '<span class="done-tag">✓ 已领取</span>' : vm.statusTag === 'claimable' ? '' : `<span class="reward">💰${vm.reward}</span>`}</div>
        <div class="pbar"><i style="width:${vm.pct * 100}%"></i></div>
        <div class="ptext">${vm.progress} / ${vm.target}</div>`;
      if (vm.claimAction?.type === 'claimTask') {
        const btn = document.createElement('button');
        btn.className = 'act-btn';
        btn.textContent = '领取奖励';
        btn.style.marginTop = '8px';
        btn.onclick = async () => {
          await this.cb.onClaimTask?.(vm.claimAction.taskId);
          if (this.modalOpen) this.cb.onPanel?.('tasks');
        };
        card.appendChild(btn);
      }
      body.appendChild(card);
    }
    this.updateHUD(state);
  }

  // ---------------- 图鉴 ----------------
  renderCodex(state) {
    const body = this.openModal('📖 作物图鉴', 'codex');
    const unlocked = new Set(state.codex);
    body.insertAdjacentHTML('beforeend', `<div style="font-size:12.5px;color:var(--ink-soft);margin-bottom:12px">首次收获即解锁 · 已收集 <b style="color:var(--green-dark)">${unlocked.size}</b> / ${CROPS.length}</div>`);
    const grid = document.createElement('div');
    grid.className = 'codex-grid';
    for (const c of CROPS) {
      const has = unlocked.has(c.id);
      const cell = document.createElement('div');
      cell.className = 'codex-cell' + (has ? '' : ' locked');
      cell.innerHTML = `${badgeHTML(c)}<div class="cname">${has ? c.name : '？？？'}</div>
        <div class="clock">${has ? (c.seasons > 1 ? `${c.seasons}季 · 💰${c.fruitPrice}` : `单季 · 💰${c.fruitPrice}`) : (c.hidden ? '锄地掉落' : `Lv.${c.unlock} 解锁`)}</div>`;
      grid.appendChild(cell);
    }
    body.appendChild(grid);
    const ms = document.createElement('div');
    ms.className = 'milestone';
    ms.innerHTML = `<div class="mrow"><span>🏆 收集里程碑</span><span>奖励通过邮件发放</span></div>` +
      CODEX_MILESTONES.map(([n, g]) => {
        const got = state.codexMilestones.includes(n);
        const can = unlocked.size >= n;
        return `<div class="mrow" style="font-weight:${can ? 800 : 400};color:${got ? 'var(--green-dark)' : can ? '#e0a92e' : 'var(--ink-soft)'}">
          <span>${got ? '✓' : can ? '🎁' : '○'} 收集 ${n} 种</span><span>💰 ${g.toLocaleString()}</span></div>`;
      }).join('');
    body.appendChild(ms);
  }

  // ---------------- 邮件（含邻里申请待办） ----------------
  renderMail(state) {
    const body = this.openModal('📮 邮箱', 'mail');
    const requests = Array.isArray(state.friendRequests) ? state.friendRequests : [];
    const mails = Array.isArray(state.mails) ? state.mails : [];
    if (!requests.length && !mails.length) {
      body.innerHTML = `<div class="empty-tip">暂无邮件</div>`;
      return;
    }

    if (requests.length) {
      const sec = document.createElement('div');
      sec.className = 'mail-section';
      sec.innerHTML = `<div class="mail-section__title">邻里申请</div>`;
      requests.forEach((r) => {
        const name = r.nickname || `UID ${r.from_uid}`;
        const row = document.createElement('div');
        row.className = 'list-row mail-row unread mail-row--friend-req';
        row.innerHTML = `<span class="unread-dot"></span>
          <div class="grow"><div class="title">${name} 申请加你为邻里</div>
          <div class="sub">同意后即可互相串门</div></div>`;
        const actions = document.createElement('div');
        actions.className = 'mail-req-actions';
        const accept = document.createElement('button');
        accept.type = 'button';
        accept.className = 'act-btn';
        accept.textContent = '同意';
        accept.onclick = async (e) => {
          e.stopPropagation();
          if (await this.cb.onAcceptFriendRequest?.(r.from_uid)) {
            if (this.modalOpen) this.cb.onPanel?.('mail');
          }
        };
        const reject = document.createElement('button');
        reject.type = 'button';
        reject.className = 'act-btn ghost';
        reject.textContent = '拒绝';
        reject.onclick = async (e) => {
          e.stopPropagation();
          if (await this.cb.onRejectFriendRequest?.(r.from_uid)) {
            if (this.modalOpen) this.cb.onPanel?.('mail');
          }
        };
        actions.append(accept, reject);
        row.appendChild(actions);
        sec.appendChild(row);
      });
      body.appendChild(sec);
    }

    const list = [...mails].reverse();
    for (const m of list) {
      const row = document.createElement('div');
      row.className = 'list-row mail-row' + (m.read ? '' : ' unread');
      const gold = m.gold || m.attachmentCoin || 0;
      const attach = [];
      if (gold) attach.push(`<span class="chip">💰 ${gold}</span>`);
      if (m.exp) attach.push(`<span class="chip">✨ ${m.exp} 经验</span>`);
      row.innerHTML = `${m.read ? '' : '<span class="unread-dot"></span>'}
        <div class="grow"><div class="title">${m.title}</div>
        <div class="sub">${m.content || ''}</div>
        ${attach.length ? `<div class="mail-attach">${attach.join('')}</div>` : ''}</div>`;
      if (gold && !m.claimed) {
        const btn = document.createElement('button');
        btn.className = 'act-btn'; btn.textContent = '领取';
        btn.onclick = async (e) => {
          e.stopPropagation();
          await this.cb.onClaimMail(m.id);
          if (this.modalOpen) this.cb.onPanel?.('mail');
        };
        row.appendChild(btn);
      } else if (m.claimed) {
        row.insertAdjacentHTML('beforeend', `<span style="font-size:11.5px;color:var(--ink-soft);font-weight:700">已领取</span>`);
      }
      row.onclick = () => { m.read = true; row.classList.remove('unread'); row.querySelector('.unread-dot')?.remove(); this.updateHUD(state); };
      body.appendChild(row);
    }
  }

  // ---------------- 看家狗 ----------------
  renderPet(state) {
    const body = this.openModal('🐶 看家狗', 'pet');
    const dog = state.dog;
    const bagFood = state.inventory?.dogFood || 0;
    if (!dog) {
      body.innerHTML = `<div class="empty-tip">还没有看家狗<br><br>去商店购买土狗，启用后记得喂狗粮</div>`;
      const go = document.createElement('button');
      go.className = 'act-btn blue';
      go.textContent = '去商店';
      go.onclick = () => this.renderShop(state, 'dog');
      body.appendChild(go);
      return;
    }
    const def = DOGS.find(d => d.id === dog.id);
    body.insertAdjacentHTML('beforeend', `<div class="list-row">
      <span style="font-size:34px">🐕</span>
      <div class="grow"><div class="title">${def?.name || dog.id}</div>
      <div class="sub">等级 ${dog.level || 0} · 拦截 ${dog.intercepts || 0} 次 · 拦截率 ${dog.interceptionPct ?? Math.round((def?.intercept || 0) * 100)}%</div></div>
    </div>
    <div style="font-size:12.5px;color:var(--ink-soft);margin:8px 0 14px">狗盆 ${Math.floor(state.dogBowl)}g / ${DOG_BOWL_CAP}g · 背包狗粮 ${bagFood}g</div>`);

    const row = document.createElement('div');
    row.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap';
    for (const grams of [10, 50, 120]) {
      const btn = document.createElement('button');
      btn.className = 'act-btn';
      btn.textContent = `喂 ${grams}g`;
      btn.disabled = bagFood < 1;
      btn.onclick = async () => {
        await this.cb.onFeedPet?.(grams);
        if (this.modalOpen) this.renderPet(this.cb.getState());
      };
      row.appendChild(btn);
    }
    const activate = document.createElement('button');
    activate.className = 'act-btn blue';
    activate.textContent = '确认启用';
    activate.onclick = async () => {
      await this.cb.onActivatePet?.(dog.id);
      if (this.modalOpen) this.renderPet(this.cb.getState());
    };
    row.appendChild(activate);
    body.appendChild(row);
  }

  // ---------------- 邻里簿（好友） ----------------
  renderFriends(state) {
    const body = this.openModal('🏡 邻里簿', 'friends');
    body.classList.add('neighbors-book');
    const { uid } = this.cb.getSession?.() || {};
    const myName = state.nickname || '我的农场';

    // 顶栏身份卡：昵称主位，UID 可复制
    const identity = document.createElement('section');
    identity.className = 'nb-identity';
    const meAvatar = document.createElement('span');
    meAvatar.className = 'nb-plate';
    meAvatar.textContent = myName[0] || '我';
    const meDetail = document.createElement('div');
    meDetail.className = 'nb-identity__text';
    const meName = document.createElement('div');
    meName.className = 'nb-identity__name';
    meName.textContent = myName;
    const meLabel = document.createElement('div');
    meLabel.className = 'nb-identity__label';
    meLabel.textContent = '我的门牌';
    meDetail.append(meName, meLabel);
    const copyUid = document.createElement('button');
    copyUid.type = 'button';
    copyUid.className = 'nb-uid-chip';
    copyUid.title = '点击复制 UID';
    copyUid.textContent = uid != null ? `UID ${uid}` : 'UID —';
    copyUid.onclick = async () => {
      if (uid == null) return;
      try {
        await navigator.clipboard?.writeText(String(uid));
        this.toast('已复制我的 UID', 'ok');
      } catch {
        this.toast('复制失败，请手动选择', 'info');
      }
    };
    identity.append(meAvatar, meDetail, copyUid);
    body.appendChild(identity);

    // 邀请邻里：主按钮；链接默认收起
    const invite = document.createElement('section');
    invite.className = 'nb-section';
    invite.innerHTML = `<h3 class="nb-section__title">邀请邻里</h3>`;
    const inviteBtn = document.createElement('button');
    inviteBtn.type = 'button';
    inviteBtn.className = 'nb-btn nb-btn--invite';
    inviteBtn.textContent = '生成并复制邀请';
    const linkWrap = document.createElement('div');
    linkWrap.className = 'nb-link-wrap hidden';
    const linkInput = document.createElement('input');
    linkInput.readOnly = true;
    linkInput.className = 'nb-input';
    linkInput.placeholder = '邀请链接会出现在这里';
    linkWrap.appendChild(linkInput);
    inviteBtn.onclick = async () => {
      const link = await this.cb.onGenShareLink?.();
      if (!link) return;
      linkInput.value = link;
      linkWrap.classList.remove('hidden');
      try {
        await navigator.clipboard?.writeText(link);
        this.toast('邀请链接已复制', 'ok');
      } catch {
        this.toast('请手动复制邀请链接', 'info');
      }
    };
    invite.append(inviteBtn, linkWrap);
    body.appendChild(invite);

    // 搜索邻里：精确用户名 → 结果列表 → 申请；邀请链接仍一键成好友
    const addSec = document.createElement('section');
    addSec.className = 'nb-section';
    addSec.innerHTML = `<h3 class="nb-section__title">搜索邻里</h3>`;
    const addRow = document.createElement('div');
    addRow.className = 'nb-add-row';
    const addInput = document.createElement('input');
    addInput.className = 'nb-input';
    addInput.placeholder = '输入用户名，或粘贴邀请链接';
    const searchBtn = document.createElement('button');
    searchBtn.type = 'button';
    searchBtn.className = 'nb-btn nb-btn--add';
    searchBtn.textContent = '搜索';
    const resultBox = document.createElement('div');
    resultBox.className = 'nb-search-results';
    const runSearch = async () => {
      resultBox.innerHTML = '';
      const result = await this.cb.onSearchNeighbors?.(addInput.value);
      if (!result) return;
      if (result.invited) {
        this.renderFriends(this.cb.getState());
        return;
      }
      if (!result.ok) return;
      const users = result.users || [];
      if (!users.length) {
        resultBox.innerHTML = `<div class="nb-search-empty">未找到该用户</div>`;
        return;
      }
      users.forEach((u) => {
        const card = document.createElement('div');
        card.className = 'nb-search-card';
        const plate = document.createElement('span');
        plate.className = 'nb-plate nb-plate--sm';
        const name = u.nickname || `UID ${u.uid}`;
        plate.textContent = name[0] || '?';
        const info = document.createElement('div');
        info.className = 'nb-card__info';
        const title = document.createElement('div');
        title.className = 'nb-card__name';
        title.textContent = name;
        const sub = document.createElement('div');
        sub.className = 'nb-search-uid';
        sub.textContent = `UID ${u.uid}`;
        info.append(title, sub);
        const apply = document.createElement('button');
        apply.type = 'button';
        apply.className = 'nb-btn nb-btn--visit';
        apply.textContent = '申请';
        apply.onclick = async () => {
          if (await this.cb.onRequestFriend?.(u.uid)) {
            this.renderFriends(this.cb.getState());
          }
        };
        card.append(plate, info, apply);
        resultBox.appendChild(card);
      });
    };
    searchBtn.onclick = () => { void runSearch(); };
    addInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        void runSearch();
      }
    });
    addRow.append(addInput, searchBtn);
    addSec.append(addRow, resultBox);
    body.appendChild(addSec);

    // 邻里列表
    const listSec = document.createElement('section');
    listSec.className = 'nb-section nb-section--list';
    const count = state.friends?.length || 0;
    listSec.innerHTML = `<h3 class="nb-section__title">邻里 <span class="nb-count">${count}</span></h3>`;

    if (!count) {
      const empty = document.createElement('div');
      empty.className = 'nb-empty';
      empty.innerHTML = `<span class="nb-empty__icon" aria-hidden="true">🌾</span>
        <p>还没有邻里</p>
        <p class="nb-empty__hint">生成邀请，或按用户名添加</p>`;
      listSec.appendChild(empty);
      body.appendChild(listSec);
      return;
    }

    const list = document.createElement('div');
    list.className = 'nb-list';
    state.friends.forEach((f, i) => {
      const name = f.nickname || `农场主 ${f.uid}`;
      const card = document.createElement('article');
      card.className = 'nb-card' + (f.has_stealable ? ' nb-card--ripe' : '');
      card.style.setProperty('--nb-i', String(i));

      const plate = document.createElement('span');
      plate.className = 'nb-plate';
      plate.textContent = name[0] || '?';

      const info = document.createElement('div');
      info.className = 'nb-card__info';
      const title = document.createElement('div');
      title.className = 'nb-card__name';
      title.textContent = name;
      info.appendChild(title);
      if (f.has_stealable) {
        const ripe = document.createElement('span');
        ripe.className = 'nb-ripe';
        ripe.textContent = '有菜可偷';
        info.appendChild(ripe);
      }

      const actions = document.createElement('div');
      actions.className = 'nb-card__actions';
      const visit = document.createElement('button');
      visit.type = 'button';
      visit.className = 'nb-btn nb-btn--visit';
      visit.textContent = '串门';
      visit.onclick = () => {
        this.closeModal();
        this.cb.onVisit(f.uid, f.nickname);
      };
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'nb-btn nb-btn--ghost';
      remove.textContent = '删除';
      remove.onclick = async () => {
        if (remove.dataset.confirm !== '1') {
          remove.dataset.confirm = '1';
          remove.textContent = '确认？';
          remove.classList.add('nb-btn--warn');
          window.setTimeout(() => {
            if (remove.dataset.confirm === '1') {
              remove.dataset.confirm = '';
              remove.textContent = '删除';
              remove.classList.remove('nb-btn--warn');
            }
          }, 2600);
          return;
        }
        if (await this.cb.onRemoveFriend?.(f.uid)) this.renderFriends(this.cb.getState());
      };
      actions.append(visit, remove);
      card.append(plate, info, actions);
      list.appendChild(card);
    });
    listSec.appendChild(list);
    body.appendChild(listSec);
  }

  // ---------------- 设置 ----------------
  renderSettings() {
    const s = this.cb.getState();
    const body = this.openModal('⚙️ 设置', 'settings');
    const tsRow = document.createElement('div');
    tsRow.className = 'set-row';
    tsRow.innerHTML = `<div><div class="lab">⏳ 时间档</div><div class="desc">新播种的作物按当前时间档折算（在途作物不受影响）</div></div>`;
    const seg = document.createElement('div');
    seg.className = 'seg';
    for (const [id, def] of Object.entries(TIME_SCALES)) {
      const b = document.createElement('button');
      b.textContent = def.label.split(' ')[0];
      b.title = def.label;
      b.className = s.timeScale === id ? 'active' : '';
      b.onclick = () => { this.cb.onSetTimeScale(id); this.renderSettings(); };
      seg.appendChild(b);
    }
    tsRow.appendChild(seg);
    body.appendChild(tsRow);

    const sndRow = document.createElement('div');
    sndRow.className = 'set-row';
    sndRow.innerHTML = `<div><div class="lab">🔊 音效</div></div>`;
    const sndSeg = document.createElement('div');
    sndSeg.className = 'seg';
    for (const [v, label] of [[true, '开'], [false, '关']]) {
      const b = document.createElement('button');
      b.textContent = label;
      b.className = s.settings.sound === v ? 'active' : '';
      b.onclick = () => { this.cb.onSetSound(v); this.renderSettings(); };
      sndSeg.appendChild(b);
    }
    sndRow.appendChild(sndSeg);
    body.appendChild(sndRow);

    const logoutRow = document.createElement('div');
    logoutRow.className = 'set-row';
    logoutRow.style.borderBottom = 'none';
    logoutRow.innerHTML = `<div><div class="lab">🚪 退出登录</div><div class="desc">断开当前连接并返回登录页</div></div>`;
    const logoutBtn = document.createElement('button');
    logoutBtn.className = 'danger-btn'; logoutBtn.textContent = '退出登录';
    logoutBtn.onclick = () => {
      if (confirm('确定退出当前账号？')) { this.cb.onLogout?.(); this.closeModal(); }
    };
    logoutRow.appendChild(logoutBtn);
    body.appendChild(logoutRow);
  }

  setVisitor(name) {
    const banner = $('#visitor-banner');
    if (name) { $('#visitor-name').textContent = name; banner.classList.remove('hidden'); }
    else banner.classList.add('hidden');
  }
}
