// ============================================================
// DOM UI：顶栏、工具栏、模态面板、tooltip、toast
// ============================================================
import { CROP_MAP, CROPS, FERTILIZERS, DOGS, TASK_POOL, CODEX_MILESTONES, EXPANSION, TIME_SCALES, DOG_BOWL_CAP, MAX_PLOTS } from './config.js';
import { levelOf, expProgress } from './state.js';
import { EXP_PER_LEVEL } from './config.js';

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

export class UI {
  constructor(cb) {
    this.cb = cb;               // { onTool, onSubSelect, onPanel, onBuy..., onVisit, onBackHome, onExpand, onSetting }
    this.activeTool = null;
    this.readOnly = false;
    this.toastWrap = $('#toast-wrap');
    this.tooltip = $('#tooltip');

    $('#modal-close').onclick = () => this.closeModal();
    $('#modal-mask').addEventListener('click', (e) => { if (e.target.id === 'modal-mask') this.closeModal(); });
    $('#btn-settings').onclick = () => this.renderSettings();
    $('#btn-back-home').onclick = () => cb.onBackHome();
    $('#btn-expand').onclick = () => cb.onExpand();

    document.querySelectorAll('#side-menu button').forEach(b => {
      b.onclick = () => { this.cb.onPanel(b.dataset.panel); };
    });
  }

  // ---------------- HUD ----------------
  updateHUD(state) {
    $('#gold-num').textContent = Math.floor(state.gold).toLocaleString();
    const lv = levelOf(state.exp);
    $('#level-badge').textContent = `Lv.${lv}`;
    $('#exp-fill').style.width = `${expProgress(state.exp) * 100}%`;
    $('#exp-text').textContent = `${state.exp % EXP_PER_LEVEL}/${EXP_PER_LEVEL}`;
    const dogChip = $('#dog-status');
    if (state.dog) {
      dogChip.classList.remove('hidden');
      $('#dog-food-num').textContent = `${Math.floor(state.dogBowl)}g`;
      dogChip.style.opacity = state.dogBowl > 0 ? 1 : 0.5;
    } else dogChip.classList.add('hidden');
    $('#dot-mail').classList.toggle('hidden', !state.mails.some(m => !m.read));
    $('#dot-tasks').classList.toggle('hidden', !state.tasks.some(t => t.done && !t.seen));
    // 扩地按钮
    const next = EXPANSION.find(e => e[0] === state.unlockedPlots + 1);
    const canExpand = !this.readOnly && !!next && lv >= next[1] && state.gold >= next[2];
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

  /** 访客农场只保留浏览入口，隐藏一切写入农场的控件。 */
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
  openModal(title) {
    $('#modal-title').textContent = title;
    $('#modal-body').innerHTML = '';
    this.showTooltip(null);
    $('#modal-mask').classList.remove('hidden');
    return $('#modal-body');
  }
  closeModal() { $('#modal-mask').classList.add('hidden'); }
  get modalOpen() { return !$('#modal-mask').classList.contains('hidden'); }

  // ---------------- 商店 ----------------
  renderShop(state, tab = 'seeds') {
    const body = this.openModal('🛒 商店');
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
      card.innerHTML = `<span style="font-size:34px">🦴</span><div class="name">狗粮</div>
        <div class="meta">1 金币 / g · 狗盆容量 ${DOG_BOWL_CAP}g<br>当前余量 ${Math.floor(state.dogBowl)}g · 狗盆空了看家狗将罢工</div>`;
      const row = document.createElement('div');
      row.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;justify-content:center';
      for (const [label, g] of [['+50g', 50], ['+120g', 120], ['填满狗盆', -1]]) {
        const btn = document.createElement('button');
        btn.className = 'buy-btn'; btn.textContent = label;
        btn.onclick = () => { this.cb.onBuyFood(g); this.renderShop(state, tab); };
        row.appendChild(btn);
      }
      card.appendChild(row);
      grid.appendChild(card);
    } else if (tab === 'dog') {
      for (const d of DOGS) {
        const owned = state.dog && state.dog.id === d.id;
        const locked = lv < d.unlock;
        const card = document.createElement('div');
        card.className = 'shop-card' + (locked ? ' locked' : '');
        card.innerHTML = `<span style="font-size:34px">🐕</span><div class="name">${d.name}</div>
          <div class="meta">拦截率 ${Math.round(d.intercept * 100)}%<br>粮耗 ${d.consumption}g/小时</div>
          <div class="price">💰 ${d.price}</div>`;
        const btn = document.createElement('button');
        btn.className = 'buy-btn';
        btn.textContent = owned ? '看家中' : locked ? `Lv.${d.unlock} 解锁` : state.dog ? '替换当前狗' : '购买';
        btn.disabled = owned || locked || state.gold < d.price;
        btn.onclick = () => { this.cb.onBuyDog(d.id); this.renderShop(state, tab); };
        card.appendChild(btn);
        grid.appendChild(card);
      }
    }
  }

  // ---------------- 背包 ----------------
  renderBag(state) {
    const body = this.openModal('🎒 背包');
    const seeds = Object.entries(state.inventory.seeds).filter(([, n]) => n > 0);
    const ferts = Object.entries(state.inventory.fertilizers).filter(([, n]) => n > 0);
    if (!seeds.length && !ferts.length) {
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
  }

  // ---------------- 仓库 ----------------
  renderBarn(state) {
    const body = this.openModal('🏠 仓库');
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
    const body = this.openModal('📋 日常任务');
    body.insertAdjacentHTML('beforeend', `<div style="font-size:12.5px;color:var(--ink-soft);margin-bottom:12px">每日随机 3 条 · 完成后奖励发送至邮箱 📮 · <b>${fmtTime(dayRemainMs)}</b> 后刷新</div>`);
    for (const t of state.tasks) {
      const def = TASK_POOL.find(d => d.id === t.taskId);
      const pct = Math.min(1, t.progress / def.target);
      const card = document.createElement('div');
      card.className = 'task-card' + (t.done ? ' done' : '');
      card.innerHTML = `<div class="head"><span class="name">${def.name}</span>
        ${t.done ? '<span class="done-tag">✓ 已完成</span>' : `<span class="reward">💰${def.gold} · ✨${def.exp}</span>`}</div>
        <div class="pbar"><i style="width:${pct * 100}%"></i></div>
        <div class="ptext">${Math.min(t.progress, def.target)} / ${def.target}</div>`;
      body.appendChild(card);
      t.seen = true;
    }
    this.updateHUD(state);
  }

  // ---------------- 图鉴 ----------------
  renderCodex(state) {
    const body = this.openModal('📖 作物图鉴');
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

  // ---------------- 邮件 ----------------
  renderMail(state) {
    const body = this.openModal('📮 邮箱');
    if (!state.mails.length) {
      body.innerHTML = `<div class="empty-tip">暂无邮件</div>`;
      return;
    }
    const list = [...state.mails].reverse();
    for (const m of list) {
      const row = document.createElement('div');
      row.className = 'list-row mail-row' + (m.read ? '' : ' unread');
      const attach = [];
      if (m.gold) attach.push(`<span class="chip">💰 ${m.gold}</span>`);
      if (m.exp) attach.push(`<span class="chip">✨ ${m.exp} 经验</span>`);
      row.innerHTML = `${m.read ? '' : '<span class="unread-dot"></span>'}
        <div class="grow"><div class="title">${m.title}</div>
        <div class="sub">${m.content}</div>
        ${attach.length ? `<div class="mail-attach">${attach.join('')}</div>` : ''}</div>`;
      if ((m.gold || m.exp) && !m.claimed) {
        const btn = document.createElement('button');
        btn.className = 'act-btn'; btn.textContent = '领取';
        btn.onclick = () => { this.cb.onClaimMail(m.id); this.renderMail(state); };
        row.appendChild(btn);
      } else if (m.claimed) {
        row.insertAdjacentHTML('beforeend', `<span style="font-size:11.5px;color:var(--ink-soft);font-weight:700">已领取</span>`);
      }
      row.onclick = () => { m.read = true; row.classList.remove('unread'); row.querySelector('.unread-dot')?.remove(); this.updateHUD(state); };
      body.appendChild(row);
    }
  }

  // ---------------- 好友 ----------------
  renderFriends(state) {
    const body = this.openModal('👥 好友');
    const { uid } = this.cb.getSession?.() || {};
    const info = document.createElement('div');
    info.style.cssText = 'font-size:12.5px;color:var(--ink-soft);margin-bottom:12px';
    info.textContent = `我的 UID：${uid ?? '—'}`;
    body.appendChild(info);

    const shareRow = document.createElement('div');
    shareRow.className = 'set-row';
    shareRow.style.marginBottom = '10px';
    const shareButton = document.createElement('button');
    shareButton.className = 'act-btn blue';
    shareButton.textContent = '复制分享链接';
    shareButton.onclick = async () => {
      const link = await this.cb.onGenShareLink?.();
      if (!link) return;
      try {
        await navigator.clipboard?.writeText(link);
        this.toast('分享链接已复制', 'ok');
      } catch {
        this.toast('请手动复制分享链接', 'info');
      }
      shareInput.value = link;
    };
    const shareInput = document.createElement('input');
    shareInput.readOnly = true;
    shareInput.placeholder = '生成后可分享给好友';
    shareInput.style.cssText = 'flex:1;min-width:0';
    shareRow.append(shareInput, shareButton);
    body.appendChild(shareRow);

    const addRow = document.createElement('div');
    addRow.className = 'set-row';
    addRow.style.marginBottom = '10px';
    const addInput = document.createElement('input');
    addInput.placeholder = '输入用户名、UID 或粘贴分享链接';
    addInput.style.cssText = 'flex:1;min-width:0';
    const addButton = document.createElement('button');
    addButton.className = 'act-btn';
    addButton.textContent = '搜索并添加';
    addButton.onclick = async () => {
      if (await this.cb.onAddFriend?.(addInput.value)) this.renderFriends(state);
    };
    addRow.append(addInput, addButton);
    body.appendChild(addRow);

    if (!state.friends?.length) {
      body.insertAdjacentHTML('beforeend', `<div style="font-size:13px;color:var(--ink-soft);padding:18px 4px">暂无好友</div>`);
      return;
    }
    for (const f of state.friends) {
      const row = document.createElement('div');
      row.className = 'list-row';
      const avatar = document.createElement('span');
      avatar.className = 'crop-badge';
      avatar.style.background = 'linear-gradient(135deg,#a8d5a2,#5a8f54)';
      avatar.textContent = (f.nickname || String(f.uid))[0];
      const detail = document.createElement('div');
      detail.className = 'grow';
      const title = document.createElement('div');
      title.className = 'title';
      title.textContent = f.nickname || `农场主 ${f.uid}`;
      const sub = document.createElement('div');
      sub.className = 'sub';
      sub.textContent = `UID：${f.uid}`;
      detail.append(title, sub);
      const visit = document.createElement('button');
      visit.className = 'act-btn blue';
      visit.textContent = '访问农场';
      visit.onclick = () => { this.closeModal(); this.cb.onVisit(f.uid, f.nickname); };
      const remove = document.createElement('button');
      remove.className = 'danger-btn';
      remove.style.marginLeft = '6px';
      remove.textContent = '删除';
      remove.onclick = async () => {
        if (await this.cb.onRemoveFriend?.(f.uid)) this.renderFriends(state);
      };
      row.append(avatar, detail, visit, remove);
      body.appendChild(row);
    }
  }

  // ---------------- 设置 ----------------
  renderSettings() {
    const s = this.cb.getState();
    const body = this.openModal('⚙️ 设置');
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

    const resetRow = document.createElement('div');
    resetRow.className = 'set-row';
    resetRow.style.borderBottom = 'none';
    resetRow.innerHTML = `<div><div class="lab">🗑️ 清理本地残留</div><div class="desc">清除旧版 localStorage 键（进度以服务器为准）</div></div>`;
    const resetBtn = document.createElement('button');
    resetBtn.className = 'danger-btn'; resetBtn.textContent = '清理并刷新';
    resetBtn.onclick = () => {
      if (confirm('确定清理本地残留并刷新？服务器进度不受影响。')) { this.cb.onReset(); this.closeModal(); }
    };
    resetRow.appendChild(resetBtn);
    body.appendChild(resetRow);
  }

  setVisitor(name) {
    const banner = $('#visitor-banner');
    if (name) { $('#visitor-name').textContent = name; banner.classList.remove('hidden'); }
    else banner.classList.add('hidden');
  }
}
