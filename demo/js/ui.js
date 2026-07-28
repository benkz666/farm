/* 界面渲染与交互 */
window.Farm = window.Farm || {};

(function (Farm) {
  'use strict';

  const cfg = Farm.config;
  const game = Farm.game;
  const sprites = Farm.sprites;
  const audio = Farm.audio;

  const TOOLS = [
    { id: 'hand', icon: '✋', name: '小手', tip: '智能操作：自动翻地 / 照料 / 收获' },
    { id: 'hoe', icon: '⛏️', name: '锄头', tip: '把空地翻松，之后才能播种' },
    { id: 'seed', icon: '🌱', name: '播种', tip: '把当前选中的种子种到翻好的地里' },
    { id: 'water', icon: '💧', name: '水壶', tip: '浇水，缺水的作物长得很慢' },
    { id: 'weed', icon: '🌿', name: '除草', tip: '拔掉杂草，杂草会拖慢生长' },
    { id: 'pest', icon: '🐛', name: '杀虫', tip: '消灭害虫，否则成熟时会减产' },
    { id: 'fert', icon: '🧪', name: '化肥', tip: '立刻推进一段生长进度' },
    { id: 'shovel', icon: '🥄', name: '铲子', tip: '铲除枯萎或不想要的作物' }
  ];

  const ui = {
    tool: 'hand',
    seedId: null,
    tab: 'shop',
    tiles: [],       // 地块 DOM 缓存
    els: {}
  };

  /* ---------------- 工具函数 ---------------- */

  function $(sel, root) { return (root || document).querySelector(sel); }
  function el(tag, cls, html) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (html != null) node.innerHTML = html;
    return node;
  }
  function num(n) { return Math.round(n).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ','); }
  function clock(ms) {
    const total = Math.ceil(ms / 1000);
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = total % 60;
    const pad = function (v) { return v < 10 ? '0' + v : '' + v; };
    return h ? h + ':' + pad(m) + ':' + pad(s) : pad(m) + ':' + pad(s);
  }
  function duration(ms) {
    const min = Math.round(ms / 60000);
    return min >= 60 ? (ms / 3600000).toFixed(1) + ' 小时' : min + ' 分钟';
  }

  /* ---------------- 提示条 / 飘字 / 弹窗 ---------------- */

  function toast(text, type) {
    const wrap = ui.els.toasts;
    const node = el('div', 'toast toast-' + (type || 'info'), text);
    wrap.appendChild(node);
    setTimeout(function () {
      node.classList.add('out');
      setTimeout(function () { node.remove(); }, 320);
    }, 2200);
    while (wrap.children.length > 4) wrap.firstChild.remove();
  }

  function floatText(plotId, text, type) {
    const tile = ui.tiles[plotId] && ui.tiles[plotId].root;
    if (!tile) return;
    const node = el('span', 'float float-' + (type || 'exp'), text);
    tile.appendChild(node);
    setTimeout(function () { node.remove(); }, 1100);
  }

  function showModal(title, bodyHtml) {
    const back = el('div', 'modal-back');
    const box = el('div', 'modal');
    box.innerHTML = '<h3>' + title + '</h3><div class="modal-body">' + bodyHtml + '</div>' +
      '<button class="btn btn-primary modal-ok">知道啦</button>';
    back.appendChild(box);
    document.body.appendChild(back);
    function close() { back.remove(); }
    back.addEventListener('click', function (e) { if (e.target === back) close(); });
    $('.modal-ok', box).addEventListener('click', close);
  }

  function result(r, sfxOk) {
    if (!r) return;
    if (r.ok) {
      toast(r.msg, r.caught ? 'warn' : 'ok');
      audio.play(r.caught ? 'caught' : (sfxOk || 'click'));
    } else {
      toast(r.msg, 'warn');
      audio.play('error');
    }
    render();
    game.persist();
  }

  /* ---------------- 顶部信息栏 ---------------- */

  function buildTopbar() {
    const bar = el('header', 'topbar');
    bar.innerHTML =
      '<div class="player">' +
      '  <div class="avatar">' + sprites.avatar(game.state.player.avatar) + '<span class="lv-badge" id="lvBadge">1</span></div>' +
      '  <div class="player-meta">' +
      '    <div class="player-name"><span id="pName">农场主</span><span class="title-tag" id="pTitle">新手农夫</span></div>' +
      '    <div class="exp-bar"><i id="expFill"></i><span id="expText">0 / 0</span></div>' +
      '  </div>' +
      '</div>' +
      '<div class="wallet">' +
      '  <div class="coin-box"><span class="coin-icon">🪙</span><b id="coinText">0</b></div>' +
      '  <div class="stat-mini" id="statMini"></div>' +
      '</div>' +
      '<div class="top-actions">' +
      '  <button class="btn btn-ghost" id="btnSound">🔊 音效</button>' +
      '  <button class="btn btn-ghost" id="btnHelp">❓ 玩法</button>' +
      '  <button class="btn btn-ghost" id="btnReset">♻️ 重开</button>' +
      '</div>';
    return bar;
  }

  function renderTopbar() {
    const p = game.state.player;
    const need = game.expNeeded(p.level);
    ui.els.lvBadge.textContent = p.level;
    ui.els.pName.textContent = p.name;
    ui.els.pTitle.textContent = titleOf(p.level);
    ui.els.expFill.style.width = Math.min(100, (p.exp / need) * 100) + '%';
    ui.els.expText.textContent = p.exp + ' / ' + need;
    ui.els.coinText.textContent = num(p.coins);
    const st = game.state.stats;
    ui.els.statMini.textContent = '收获 ' + num(st.harvested) + ' · 偷菜 ' + num(st.stolen);
    ui.els.btnSound.textContent = (game.state.settings.sound ? '🔊' : '🔇') + ' 音效';
  }

  function titleOf(level) {
    if (level >= 20) return '农场大亨';
    if (level >= 14) return '种田高手';
    if (level >= 9) return '资深农夫';
    if (level >= 5) return '熟练农夫';
    if (level >= 3) return '见习农夫';
    return '新手农夫';
  }

  /* ---------------- 农场网格 ---------------- */

  function buildGrid() {
    const grid = el('div', 'farm-grid');
    game.state.plots.forEach(function (plot) {
      const root = el('button', 'tile');
      root.type = 'button';
      root.dataset.id = plot.id;
      root.innerHTML =
        '<span class="soil">' +
        '  <span class="weeds"></span>' +
        '  <span class="plant"></span>' +
        '  <span class="overlay"></span>' +
        '  <span class="lock"></span>' +
        '</span>' +
        '<span class="tile-hud">' +
        '  <span class="tile-name"></span>' +
        '  <span class="bar"><i></i></span>' +
        '</span>' +
        '<span class="tile-flags"></span>';
      grid.appendChild(root);
      ui.tiles[plot.id] = {
        root: root,
        plant: $('.plant', root),
        weeds: $('.weeds', root),
        overlay: $('.overlay', root),
        lock: $('.lock', root),
        name: $('.tile-name', root),
        bar: $('.bar i', root),
        flags: $('.tile-flags', root),
        sig: ''
      };
    });
    grid.addEventListener('click', function (e) {
      const tile = e.target.closest('.tile');
      if (tile) handleTileClick(Number(tile.dataset.id));
    });
    return grid;
  }

  function renderGrid() {
    // 只展示已开垦的地块与紧接着的下一块，避免一屏灰格子
    let nextLocked = game.state.plots.length;
    for (let i = 0; i < game.state.plots.length; i++) {
      if (!game.state.plots[i].unlocked) { nextLocked = i; break; }
    }

    game.state.plots.forEach(function (plot) {
      const t = ui.tiles[plot.id];
      const crop = game.cropOf(plot);
      const stage = game.stageOf(plot);
      const cls = ['tile'];

      if (plot.id > nextLocked) cls.push('is-hidden');
      if (!plot.unlocked) cls.push('is-locked');
      else if (!plot.cropId) cls.push(plot.tilled ? 'is-tilled' : 'is-raw');
      else {
        cls.push('is-planted');
        if (plot.ripe) cls.push('is-ripe');
        if (plot.withered) cls.push('is-withered');
        if (plot.water <= 0 && !plot.ripe) cls.push('is-dry');
      }
      t.root.className = cls.join(' ');

      const sig = [plot.unlocked, plot.tilled, plot.cropId, stage, plot.weeds, plot.pests].join('|');
      if (sig !== t.sig) {
        t.sig = sig;
        t.plant.innerHTML = crop ? sprites.plant(crop, stage, plot.id + 1) : '';
        t.weeds.innerHTML = plot.weeds ? sprites.weeds(plot.id + 5) : '';
        t.overlay.innerHTML = plot.pests ? sprites.pests(plot.id + 11) : '';
        if (!plot.unlocked) {
          const cost = cfg.plotUnlockCost(plot.id);
          const need = cfg.plotUnlockLevel(plot.id);
          t.lock.innerHTML = '<b>🔒 Lv.' + need + '</b><span>🪙 ' + num(cost) + '</span>';
        } else {
          t.lock.innerHTML = '';
        }
      }

      // 文字与进度每秒刷新
      if (!plot.unlocked) {
        t.name.textContent = '待开垦';
        t.bar.style.width = '0%';
      } else if (!plot.cropId) {
        t.name.textContent = plot.tilled ? '已翻好的地' : '荒地';
        t.bar.style.width = '0%';
      } else if (plot.withered) {
        t.name.textContent = crop.name + ' · 已枯萎';
        t.bar.style.width = '100%';
      } else if (plot.ripe) {
        const left = cfg.RULES.witherAfterRipeMs - plot.ripeElapsed;
        t.name.textContent = left < 5 * 60 * 1000
          ? crop.name + ' · 快枯萎 ' + clock(left)
          : crop.name + ' · 可收获';
        t.root.classList.toggle('is-urgent', left < 5 * 60 * 1000);
        t.bar.style.width = '100%';
      } else {
        t.name.textContent = crop.name + ' · ' + clock(game.remainingMs(plot));
        t.bar.style.width = (game.progressOf(plot) * 100).toFixed(1) + '%';
      }

      let flags = '';
      if (plot.cropId && !plot.withered) {
        if (plot.water <= 0) flags += '<i class="flag flag-dry" title="缺水，生长变慢">🥵</i>';
        else if (plot.water < 35) flags += '<i class="flag flag-thirsty" title="快没水了">💧</i>';
        if (plot.weeds) flags += '<i class="flag flag-weed" title="有杂草">🌿</i>';
        if (plot.pests) flags += '<i class="flag flag-pest" title="有害虫，会减产">🐛</i>';
        if (plot.ripe) flags += '<i class="flag flag-ripe" title="可以收获">✨</i>';
      }
      t.flags.innerHTML = flags;
    });
  }

  /** 小手模式：按优先级自动执行最合适的操作 */
  function smartAction(plot) {
    if (!plot.unlocked) return { fn: 'unlockPlot', sfx: 'coin' };
    if (plot.withered) return { fn: 'clearPlot', sfx: 'till' };
    if (plot.ripe) return { fn: 'harvest', sfx: 'harvest' };
    if (plot.weeds) return { fn: 'removeWeeds', sfx: 'weed' };
    if (plot.pests) return { fn: 'killPests', sfx: 'pest' };
    if (plot.cropId && plot.water <= 85) return { fn: 'water', sfx: 'water' };
    if (!plot.cropId && !plot.tilled) return { fn: 'till', sfx: 'till' };
    if (!plot.cropId && plot.tilled) return { fn: 'plant', sfx: 'plant' };
    return null;
  }

  function handleTileClick(plotId) {
    const plot = game.state.plots[plotId];
    audio.unlock();

    if (ui.tool === 'hand') {
      const act = smartAction(plot);
      if (!act) { toast('这块地暂时不需要打理', 'info'); return; }
      if (act.fn === 'plant') return doPlant(plotId);
      return result(game[act.fn](plotId), act.sfx);
    }

    switch (ui.tool) {
      case 'hoe': return result(plot.unlocked ? game.till(plotId) : game.unlockPlot(plotId), 'till');
      case 'seed': return doPlant(plotId);
      case 'water': return result(game.water(plotId), 'water');
      case 'weed': return result(game.removeWeeds(plotId), 'weed');
      case 'pest': return result(game.killPests(plotId), 'pest');
      case 'fert': return result(game.useFertilizer(plotId), 'plant');
      case 'shovel': return result(game.clearPlot(plotId), 'till');
      default: return null;
    }
  }

  function doPlant(plotId) {
    if (!ui.seedId) {
      toast('先在右边商店里选一种种子', 'info');
      switchTab('shop');
      return;
    }
    result(game.plant(plotId, ui.seedId), 'plant');
  }

  /* ---------------- 工具栏 ---------------- */

  function buildToolbar() {
    const bar = el('div', 'toolbar');
    const tools = el('div', 'tools');
    TOOLS.forEach(function (t) {
      const b = el('button', 'tool', '<span class="tool-icon">' + t.icon + '</span><span class="tool-name">' + t.name + '</span>');
      b.type = 'button';
      b.dataset.tool = t.id;
      b.title = t.tip;
      tools.appendChild(b);
    });
    tools.addEventListener('click', function (e) {
      const b = e.target.closest('.tool');
      if (!b) return;
      ui.tool = b.dataset.tool;
      audio.unlock();
      audio.play('click');
      const t = TOOLS.filter(function (x) { return x.id === ui.tool; })[0];
      toast(t.name + '：' + t.tip, 'info');
      renderToolbar();
    });

    const quick = el('div', 'quick');
    quick.innerHTML =
      '<button class="btn btn-soft" data-act="tillAll">⛏️ 一键翻地</button>' +
      '<button class="btn btn-soft" data-act="plantAll">🌱 一键播种</button>' +
      '<button class="btn btn-soft" data-act="careAll">💧 一键照料</button>' +
      '<button class="btn btn-primary" data-act="harvestAll">🧺 一键收获</button>' +
      '<button class="btn btn-soft" data-act="clearAll">🧹 清理枯地</button>';
    quick.addEventListener('click', function (e) {
      const b = e.target.closest('button');
      if (!b) return;
      audio.unlock();
      switch (b.dataset.act) {
        case 'tillAll': return result(game.tillAll(), 'till');
        case 'plantAll':
          if (!ui.seedId) { toast('先在商店里选一种种子', 'info'); switchTab('shop'); return; }
          return result(game.plantAll(ui.seedId), 'plant');
        case 'careAll': return result(game.careAll(), 'water');
        case 'harvestAll': return result(game.harvestAll(), 'harvest');
        case 'clearAll': return result(game.clearAllWithered(), 'till');
        default: return null;
      }
    });

    bar.appendChild(tools);
    bar.appendChild(quick);
    return bar;
  }

  function renderToolbar() {
    document.body.dataset.tool = ui.tool;
    const nodes = ui.els.toolbar.querySelectorAll('.tool');
    Array.prototype.forEach.call(nodes, function (n) {
      n.classList.toggle('active', n.dataset.tool === ui.tool);
    });
    const seed = ui.seedId ? cfg.CROP_MAP[ui.seedId] : null;
    ui.els.seedChip.innerHTML = seed
      ? '当前种子：<b>' + seed.name + '</b> <span class="chip-sub">库存 ' + (game.state.seeds[seed.id] || 0) + ' 包</span>'
      : '当前种子：<b>未选择</b>';
  }

  /* ---------------- 右侧面板 ---------------- */

  function buildPanel() {
    const panel = el('aside', 'panel');
    panel.innerHTML =
      '<div class="tabs">' +
      '  <button class="tab" data-tab="shop">🛒 种子商店</button>' +
      '  <button class="tab" data-tab="bag">🧺 我的仓库</button>' +
      '  <button class="tab" data-tab="friend">👨‍🌾 邻居农场</button>' +
      '  <button class="tab" data-tab="info">📖 农场档案</button>' +
      '</div>' +
      '<div class="panel-body" id="panelBody"></div>';
    panel.querySelector('.tabs').addEventListener('click', function (e) {
      const b = e.target.closest('.tab');
      if (b) { audio.play('click'); switchTab(b.dataset.tab); }
    });
    return panel;
  }

  function switchTab(tab) {
    ui.tab = tab;
    renderPanel();
  }

  function renderPanel() {
    const body = ui.els.panelBody;
    Array.prototype.forEach.call(ui.els.panel.querySelectorAll('.tab'), function (t) {
      t.classList.toggle('active', t.dataset.tab === ui.tab);
    });
    const scroll = body.scrollTop;
    if (ui.tab === 'shop') body.innerHTML = shopHtml();
    else if (ui.tab === 'bag') body.innerHTML = bagHtml();
    else if (ui.tab === 'friend') body.innerHTML = friendHtml();
    else body.innerHTML = infoHtml();
    body.scrollTop = scroll;
  }

  function seedThumb(crop) {
    return '<span class="thumb">' + sprites.plant(crop, 'ripe', crop.name.length) + '</span>';
  }

  function shopHtml() {
    const p = game.state.player;
    let out = '<div class="shop-note">收获后作物进仓库，卖出才能换成金币。等级越高能种的作物越贵、越赚。</div>';
    out += '<div class="card-list">';
    cfg.CROPS.forEach(function (crop) {
      const locked = p.level < crop.level;
      const stock = game.state.seeds[crop.id] || 0;
      const profit = crop.sellPrice * crop.yield - crop.seedPrice;
      out += '<div class="crop-card' + (locked ? ' locked' : '') + (ui.seedId === crop.id ? ' picked' : '') + '" data-crop="' + crop.id + '">' +
        seedThumb(crop) +
        '<div class="crop-meta">' +
        '  <div class="crop-title">' + crop.name +
        (locked ? '<span class="need">需 ' + crop.level + ' 级</span>' : '<span class="lv">Lv.' + crop.level + '</span>') +
        (stock ? '<span class="stock">种子 x' + stock + '</span>' : '') + '</div>' +
        '  <div class="crop-line">🕒 ' + duration(crop.growMs) + ' · 产 ' + crop.yield + ' 个 · ✨' + crop.exp + '</div>' +
        '  <div class="crop-line">🪙 种子 ' + num(crop.seedPrice) + ' → 单价 ' + num(crop.sellPrice) +
        ' <b class="profit">净赚 ' + num(profit) + '</b></div>' +
        '</div>' +
        '<div class="crop-buy">' +
        '  <button class="btn btn-mini" data-buy="' + crop.id + '" data-n="1"' + (locked ? ' disabled' : '') + '>买1</button>' +
        '  <button class="btn btn-mini" data-buy="' + crop.id + '" data-n="6"' + (locked ? ' disabled' : '') + '>买6</button>' +
        '</div>' +
        '</div>';
    });
    out += '</div>';

    out += '<h4 class="panel-h">道具与土地</h4>';
    out += '<div class="item-row">' +
      '<span class="item-ico">🧪</span>' +
      '<div class="item-meta"><b>化肥</b><span>立刻推进 ' + Math.round(cfg.RULES.fertilizerBoost * 100) + '% 生长进度 · 库存 ' +
      game.state.items.fertilizer + '</span></div>' +
      '<button class="btn btn-mini" data-item="fert" data-n="1">🪙 ' + num(cfg.RULES.fertilizerPrice) + '</button>' +
      '</div>';

    const next = game.state.plots.filter(function (pl) { return !pl.unlocked; })[0];
    if (next) {
      out += '<div class="item-row">' +
        '<span class="item-ico">🟫</span>' +
        '<div class="item-meta"><b>开垦新地块</b><span>第 ' + (next.id + 1) + ' 块 · 需要 ' +
        cfg.plotUnlockLevel(next.id) + ' 级</span></div>' +
        '<button class="btn btn-mini" data-unlock="' + next.id + '">🪙 ' + num(cfg.plotUnlockCost(next.id)) + '</button>' +
        '</div>';
    } else {
      out += '<div class="item-row done"><span class="item-ico">🏆</span><div class="item-meta"><b>土地全开垦</b><span>24 块地全部到手，了不起！</span></div></div>';
    }
    return out;
  }

  function bagHtml() {
    const ids = Object.keys(game.state.bag).filter(function (id) { return cfg.CROP_MAP[id]; });
    let out = '<div class="bag-head"><span>仓库总价值 <b>🪙 ' + num(game.bagValue()) + '</b></span>' +
      '<button class="btn btn-primary btn-mini" data-sellall="1">全部卖出</button></div>';
    if (!ids.length) return out + '<div class="empty">仓库还是空的，快去收获作物吧 🌾</div>';
    out += '<div class="card-list">';
    ids.sort(function (a, b) { return cfg.CROP_MAP[b].sellPrice - cfg.CROP_MAP[a].sellPrice; });
    ids.forEach(function (id) {
      const crop = cfg.CROP_MAP[id];
      const n = game.state.bag[id];
      out += '<div class="crop-card">' + seedThumb(crop) +
        '<div class="crop-meta"><div class="crop-title">' + crop.name + '<span class="stock">x' + n + '</span></div>' +
        '<div class="crop-line">单价 🪙 ' + num(crop.sellPrice) + ' · 合计 <b class="profit">' + num(crop.sellPrice * n) + '</b></div></div>' +
        '<div class="crop-buy">' +
        '<button class="btn btn-mini" data-sell="' + id + '" data-n="1">卖1</button>' +
        '<button class="btn btn-mini" data-sell="' + id + '" data-n="' + n + '">全卖</button>' +
        '</div></div>';
    });
    return out + '</div>';
  }

  function friendHtml() {
    const now = Date.now();
    let out = '<div class="shop-note">邻居家成熟的作物可以顺手“借”一个，被发现要赔钱。每位邻居 5 分钟只能光顾一次。</div>';
    game.state.neighbors.forEach(function (nb) {
      const cd = nb.cooldownUntil - now;
      out += '<div class="friend">' +
        '<div class="friend-head">' +
        '<span class="friend-avatar" style="background:' + nb.avatar + '">' + nb.name.slice(0, 1) + '</span>' +
        '<b>' + nb.name + '</b>' +
        '<span class="friend-cd" data-nb="' + nb.id + '">' + (cd > 0 ? '⏳ ' + clock(cd) + ' 后可再来' : '✅ 可以去转转') + '</span>' +
        '</div><div class="friend-plots">';
      nb.slots.forEach(function (slot, i) {
        const crop = cfg.CROP_MAP[slot.cropId];
        const ready = slot.readyAt <= now;
        out += '<button class="fplot' + (ready ? ' ready' : '') + '" data-steal="' + nb.id + '" data-slot="' + i + '"' +
          (ready && cd <= 0 ? '' : ' disabled') + ' title="' + crop.name + '">' +
          sprites.plant(crop, ready ? 'ripe' : 'small', i + 2) +
          '<span class="fplot-tip">' + (ready ? '偷' : clock(slot.readyAt - now)) + '</span>' +
          '</button>';
      });
      out += '</div></div>';
    });
    return out;
  }

  /** 邻居面板只更新倒计时与可点状态：整块重建会打断用户的点击 */
  function updateFriendTimers() {
    const now = Date.now();
    const body = ui.els.panelBody;
    game.state.neighbors.forEach(function (nb) {
      const cd = nb.cooldownUntil - now;
      const cdEl = body.querySelector('.friend-cd[data-nb="' + nb.id + '"]');
      if (!cdEl) return;
      cdEl.textContent = cd > 0 ? '⏳ ' + clock(cd) + ' 后可再来' : '✅ 可以去转转';
      nb.slots.forEach(function (slot, i) {
        const btn = body.querySelector('.fplot[data-steal="' + nb.id + '"][data-slot="' + i + '"]');
        if (!btn) return;
        const ready = slot.readyAt <= now;
        btn.disabled = !(ready && cd <= 0);
        if (btn.classList.contains('ready') !== ready) {
          btn.classList.toggle('ready', ready);
          const crop = cfg.CROP_MAP[slot.cropId];
          btn.innerHTML = sprites.plant(crop, ready ? 'ripe' : 'small', i + 2) + '<span class="fplot-tip"></span>';
        }
        const tip = btn.querySelector('.fplot-tip');
        if (tip) tip.textContent = ready ? '偷' : clock(slot.readyAt - now);
      });
    });
  }

  function infoHtml() {
    const st = game.state.stats;
    const p = game.state.player;
    const days = Math.max(1, Math.round((Date.now() - (game.state.createdAt || Date.now())) / 86400000));
    return '' +
      '<div class="info-grid">' +
      '<div class="info-card"><b>' + num(st.harvested) + '</b><span>累计收获</span></div>' +
      '<div class="info-card"><b>' + num(st.planted) + '</b><span>累计播种</span></div>' +
      '<div class="info-card"><b>' + num(st.stolen) + '</b><span>偷菜次数</span></div>' +
      '<div class="info-card"><b>' + num(st.earned) + '</b><span>累计收入</span></div>' +
      '<div class="info-card"><b>Lv.' + p.level + '</b><span>' + titleOf(p.level) + '</span></div>' +
      '<div class="info-card"><b>' + days + '</b><span>经营天数</span></div>' +
      '</div>' +
      '<h4 class="panel-h">玩法说明</h4>' +
      '<ol class="rules">' +
      '<li><b>翻地 → 播种 → 浇水 → 收获</b>：默认“小手”会自动做当前最该做的事，点地块即可。</li>' +
      '<li><b>水分</b>会随时间流失，干透的地生长速度只有 ' + Math.round(cfg.RULES.dryGrowthFactor * 100) + '%。</li>' +
      '<li><b>杂草</b>拖慢生长，<b>害虫</b>不处理会在成熟时减产 ' + cfg.RULES.pestYieldPenalty + ' 个。</li>' +
      '<li>成熟后 ' + Math.round(cfg.RULES.witherAfterRipeMs / 60000) + ' 分钟内不收会<b>枯萎</b>，只能铲掉。</li>' +
      '<li>升级解锁更贵的作物；金币可以开垦新地块，最多 ' + cfg.RULES.plotCount + ' 块。</li>' +
      '<li>关掉网页也会继续生长，回来时自动结算离线时间。</li>' +
      '</ol>' +
      '<h4 class="panel-h">作物图鉴</h4>' +
      '<div class="dex">' + cfg.CROPS.map(function (c) {
        const known = p.level >= c.level;
        return '<div class="dex-item' + (known ? '' : ' locked') + '" title="' + c.name + '">' +
          sprites.plant(c, 'ripe', c.name.length) + '<span>' + (known ? c.name : 'Lv.' + c.level) + '</span></div>';
      }).join('') + '</div>';
  }

  function bindPanelEvents() {
    ui.els.panelBody.addEventListener('click', function (e) {
      const target = e.target;
      audio.unlock();

      const card = target.closest('.crop-card');
      const buy = target.closest('[data-buy]');
      const sell = target.closest('[data-sell]');
      const item = target.closest('[data-item]');
      const unlock = target.closest('[data-unlock]');
      const sellAll = target.closest('[data-sellall]');
      const steal = target.closest('[data-steal]');

      if (buy) {
        const r = game.buySeed(buy.dataset.buy, Number(buy.dataset.n));
        if (r.ok) ui.seedId = buy.dataset.buy;
        return result(r, 'coin');
      }
      if (sell) return result(game.sell(sell.dataset.sell, Number(sell.dataset.n)), 'coin');
      if (sellAll) return result(game.sellAll(), 'coin');
      if (item) return result(game.buyFertilizer(Number(item.dataset.n)), 'coin');
      if (unlock) return result(game.unlockPlot(Number(unlock.dataset.unlock)), 'coin');
      if (steal) {
        const r = game.stealCrop(steal.dataset.steal, Number(steal.dataset.slot));
        return result(r, 'steal');
      }
      if (card && card.dataset.crop) {
        const crop = cfg.CROP_MAP[card.dataset.crop];
        if (game.state.player.level < crop.level) {
          toast(crop.name + '需要 ' + crop.level + ' 级才能种', 'warn');
          audio.play('error');
          return;
        }
        ui.seedId = crop.id;
        ui.tool = 'hand';
        audio.play('click');
        toast('已选中 ' + crop.name + '，点空地即可播种', 'ok');
        render();
      }
    });
  }

  /* ---------------- 主渲染 ---------------- */

  function render() {
    renderTopbar();
    renderGrid();
    renderToolbar();
    renderPanel();
  }

  /** 每秒心跳：只刷新会随时间变化的部分，避免重建整个面板 */
  function renderTick() {
    renderTopbar();
    renderGrid();
    if (ui.tab === 'friend') updateFriendTimers();
  }

  /* ---------------- 启动 ---------------- */

  function mount() {
    const app = el('div', 'app');
    const topbar = buildTopbar();
    const stage = el('main', 'stage');

    const farmWrap = el('section', 'farm');
    farmWrap.innerHTML = '<div class="farm-sign"><span class="sign-post"></span><b>🌻 我的开心农场</b>' +
      '<span class="seed-chip" id="seedChip"></span></div>';
    const grid = buildGrid();
    farmWrap.appendChild(grid);
    const toolbar = buildToolbar();
    farmWrap.appendChild(toolbar);

    const panel = buildPanel();
    stage.appendChild(farmWrap);
    stage.appendChild(panel);

    app.appendChild(topbar);
    app.appendChild(stage);
    document.body.appendChild(app);
    document.body.appendChild(el('div', 'toasts'));

    ui.els = {
      app: app,
      toasts: $('.toasts'),
      lvBadge: $('#lvBadge'),
      pName: $('#pName'),
      pTitle: $('#pTitle'),
      expFill: $('#expFill'),
      expText: $('#expText'),
      coinText: $('#coinText'),
      statMini: $('#statMini'),
      btnSound: $('#btnSound'),
      toolbar: toolbar,
      seedChip: $('#seedChip'),
      panel: panel,
      panelBody: $('#panelBody')
    };

    bindPanelEvents();

    $('#btnSound').addEventListener('click', function () {
      game.state.settings.sound = !game.state.settings.sound;
      audio.unlock();
      audio.play('click');
      renderTopbar();
      game.persist();
    });
    $('#btnHelp').addEventListener('click', function () { switchTab('info'); audio.play('click'); });
    $('#btnReset').addEventListener('click', function () {
      if (!window.confirm('确定要重开农场吗？当前存档会被清空。')) return;
      game.hardReset();
      ui.seedId = null;
      ui.tool = 'hand';
      render();
      toast('农场已重置，重新开始吧！', 'ok');
    });

    game.on('float', function (d) { floatText(d.plotId, d.text, d.type); });
    game.on('levelup', function (d) {
      audio.play('levelup');
      const list = d.unlocked.length
        ? '<p>解锁了新作物，去种子商店看看吧：</p>'
        : '<p>继续努力，更贵的作物就在前面！</p>';
      const dex = d.unlocked.map(function (c) {
        return '<div class="dex-item">' + sprites.plant(c, 'ripe', 3) + '<span>' + c.name + '</span></div>';
      }).join('');
      showModal('🎉 升到 ' + d.level + " 级 · " + titleOf(d.level), list + '<div class="dex">' + dex + '</div>');
    });
    let lastRipeSfx = 0;
    game.on('ripe', function () {
      // 多块地同时成熟时只响一次
      const now = Date.now();
      if (now - lastRipeSfx < 1500) return;
      lastRipeSfx = now;
      audio.play('coin');
    });

    ui.seedId = cfg.CROPS[0].id;
    render();
  }

  Farm.ui = { mount: mount, render: render, renderTick: renderTick, toast: toast, showModal: showModal };
})(window.Farm);
