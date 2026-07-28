/* 游戏核心：生长推进、农事动作、经济与等级 */
window.Farm = window.Farm || {};

(function (Farm) {
  'use strict';

  const cfg = Farm.config;
  const RULES = cfg.RULES;
  const store = Farm.store;

  const listeners = {};
  function on(evt, fn) {
    (listeners[evt] = listeners[evt] || []).push(fn);
  }
  function emit(evt, payload) {
    (listeners[evt] || []).forEach(function (fn) { fn(payload); });
  }

  const game = {
    state: null,
    on: on,
    emit: emit
  };

  /* ---------------- 查询辅助 ---------------- */

  function cropOf(plot) {
    return plot && plot.cropId ? cfg.CROP_MAP[plot.cropId] : null;
  }

  function progressOf(plot) {
    const crop = cropOf(plot);
    if (!crop) return 0;
    return Math.min(1, plot.growth / crop.growMs);
  }

  function stageOf(plot) {
    if (!plot || !plot.cropId) return null;
    if (plot.withered) return 'withered';
    const p = progressOf(plot);
    let key = 'seed';
    for (let i = 0; i < cfg.STAGES.length; i++) {
      if (p >= cfg.STAGES[i].at) key = cfg.STAGES[i].key;
    }
    return key;
  }

  /** 当前生长速度系数（受水分、杂草、害虫影响） */
  function growthFactor(plot) {
    let f = 1;
    if (plot.water <= 0) f *= RULES.dryGrowthFactor;
    if (plot.weeds) f *= RULES.weedGrowthFactor;
    if (plot.pests) f *= RULES.pestGrowthFactor;
    return f;
  }

  /** 按当前状态估算的剩余成熟时间（毫秒） */
  function remainingMs(plot) {
    const crop = cropOf(plot);
    if (!crop || plot.ripe) return 0;
    const left = crop.growMs - plot.growth;
    return Math.max(0, left / Math.max(0.05, growthFactor(plot)));
  }

  function expNeeded(level) {
    return cfg.LEVEL_EXP[Math.min(level - 1, cfg.LEVEL_EXP.length - 1)];
  }

  function bagValue() {
    let sum = 0;
    Object.keys(game.state.bag).forEach(function (id) {
      const crop = cfg.CROP_MAP[id];
      if (crop) sum += crop.sellPrice * game.state.bag[id];
    });
    return sum;
  }

  /* ---------------- 时间推进 ---------------- */

  function stepPlot(plot, dt) {
    if (!plot.unlocked || !plot.cropId || plot.withered) return;
    const crop = cropOf(plot);
    const minutes = dt / 60000;

    if (!plot.ripe) {
      plot.water = Math.max(0, plot.water - RULES.waterDrainPerMin * minutes);
      if (Math.random() < RULES.weedChancePerMin * minutes) plot.weeds = true;
      if (Math.random() < RULES.pestChancePerMin * minutes) plot.pests = true;

      plot.growth += dt * growthFactor(plot);
      if (plot.growth >= crop.growMs) {
        plot.growth = crop.growMs;
        plot.ripe = true;
        plot.pestDamage = plot.pests ? RULES.pestYieldPenalty : 0;
        emit('ripe', plot);
      }
    } else {
      plot.ripeElapsed += dt;
      if (plot.ripeElapsed >= RULES.witherAfterRipeMs) {
        plot.withered = true;
        plot.ripe = false;
      }
    }
  }

  /** 推进 dtMs 毫秒；离线长时间用大步长近似 */
  function advance(dtMs) {
    let left = Math.max(0, dtMs);
    while (left > 0) {
      const chunk = left > 90 * 1000 ? 60 * 1000 : Math.min(left, 5000);
      for (let i = 0; i < game.state.plots.length; i++) stepPlot(game.state.plots[i], chunk);
      left -= chunk;
    }
    refreshNeighbors();
  }

  function refreshNeighbors() {
    const now = Date.now();
    game.state.neighbors.forEach(function (n) {
      n.slots.forEach(function (slot, i) {
        if (slot.readyAt <= now - 20 * 60 * 1000) {
          // 长时间无人打理，换一茬新作物
          n.slots[i] = store.randomNeighborSlot(game.state.player.level);
        }
      });
    });
  }

  /* ---------------- 收益与等级 ---------------- */

  function addCoins(n) {
    game.state.player.coins = Math.max(0, Math.round(game.state.player.coins + n));
    if (n > 0) game.state.stats.earned += n;
  }

  function addExp(n) {
    const p = game.state.player;
    p.exp += n;
    let leveled = 0;
    while (p.exp >= expNeeded(p.level) && p.level < cfg.LEVEL_EXP.length) {
      p.exp -= expNeeded(p.level);
      p.level++;
      leveled++;
    }
    if (leveled) {
      const unlocked = cfg.CROPS.filter(function (c) { return c.level === p.level; });
      emit('levelup', { level: p.level, unlocked: unlocked });
    }
  }

  function fail(msg) { return { ok: false, msg: msg }; }
  function done(msg, extra) { return Object.assign({ ok: true, msg: msg }, extra || {}); }

  /* ---------------- 农事动作 ---------------- */

  function till(plotId) {
    const plot = game.state.plots[plotId];
    if (!plot.unlocked) return fail('这块地还没开垦');
    if (plot.cropId) return fail('地里还有作物');
    if (plot.tilled) return fail('这块地已经翻好了');
    plot.tilled = true;
    addExp(RULES.exp.till);
    emit('float', { plotId: plotId, text: '+' + RULES.exp.till + ' 经验', type: 'exp' });
    return done('翻地完成，可以播种了');
  }

  function buySeed(cropId, count) {
    const crop = cfg.CROP_MAP[cropId];
    const p = game.state.player;
    count = count || 1;
    if (!crop) return fail('没有这种种子');
    if (p.level < crop.level) return fail(crop.name + '需要 ' + crop.level + ' 级才能种植');
    const cost = crop.seedPrice * count;
    if (p.coins < cost) return fail('金币不足，需要 ' + cost + ' 金币');
    addCoins(-cost);
    game.state.seeds[cropId] = (game.state.seeds[cropId] || 0) + count;
    return done('购买 ' + crop.name + '种子 x' + count, { cost: cost });
  }

  function plant(plotId, cropId) {
    const plot = game.state.plots[plotId];
    const crop = cfg.CROP_MAP[cropId];
    const p = game.state.player;
    if (!crop) return fail('没有这种种子');
    if (!plot.unlocked) return fail('这块地还没开垦');
    if (plot.cropId) return fail('地里已经有作物了');
    if (!plot.tilled) return fail('请先用锄头翻地');
    if (p.level < crop.level) return fail(crop.name + '需要 ' + crop.level + ' 级');

    if (!game.state.seeds[cropId]) {
      const r = buySeed(cropId, 1);
      if (!r.ok) return r;
    }
    game.state.seeds[cropId]--;
    if (!game.state.seeds[cropId]) delete game.state.seeds[cropId];

    plot.cropId = cropId;
    plot.growth = 0;
    plot.water = 60;
    plot.weeds = false;
    plot.pests = false;
    plot.pestDamage = 0;
    plot.ripe = false;
    plot.ripeElapsed = 0;
    plot.withered = false;
    plot.plantedAt = Date.now();
    game.state.stats.planted++;
    addExp(RULES.exp.plant);
    emit('float', { plotId: plotId, text: '播下' + crop.name, type: 'plant' });
    return done('种下了' + crop.name);
  }

  function water(plotId) {
    const plot = game.state.plots[plotId];
    if (!plot.cropId) return fail('这里没有作物');
    if (plot.withered) return fail('作物已经枯萎了，先铲掉吧');
    if (plot.water > 85) return fail('土壤还很湿润');
    plot.water = 100;
    addExp(RULES.exp.water);
    emit('float', { plotId: plotId, text: '+' + RULES.exp.water + ' 经验', type: 'water' });
    return done('浇水完成');
  }

  function removeWeeds(plotId) {
    const plot = game.state.plots[plotId];
    if (!plot.weeds) return fail('这块地没有杂草');
    plot.weeds = false;
    addExp(RULES.exp.weed);
    emit('float', { plotId: plotId, text: '+' + RULES.exp.weed + ' 经验', type: 'exp' });
    return done('杂草清理干净了');
  }

  function killPests(plotId) {
    const plot = game.state.plots[plotId];
    if (!plot.pests) return fail('这块地没有害虫');
    plot.pests = false;
    addExp(RULES.exp.pest);
    emit('float', { plotId: plotId, text: '+' + RULES.exp.pest + ' 经验', type: 'exp' });
    return done('害虫被赶走了');
  }

  function harvest(plotId) {
    const plot = game.state.plots[plotId];
    const crop = cropOf(plot);
    if (!crop) return fail('这里没有作物');
    if (plot.withered) return fail('作物已枯萎，无法收获');
    if (!plot.ripe) return fail('还没成熟，再等等');

    const amount = Math.max(1, crop.yield - (plot.pestDamage || 0));
    game.state.bag[crop.id] = (game.state.bag[crop.id] || 0) + amount;
    game.state.stats.harvested += amount;
    addExp(crop.exp);

    Object.assign(plot, store.newPlot(plot.id), { unlocked: true, tilled: false });
    emit('float', { plotId: plotId, text: crop.name + ' x' + amount, type: 'harvest' });
    return done('收获 ' + crop.name + ' x' + amount + '，获得 ' + crop.exp + ' 经验', { amount: amount });
  }

  function clearPlot(plotId) {
    const plot = game.state.plots[plotId];
    if (!plot.cropId) return fail('这里没有需要清理的东西');
    const wasWithered = plot.withered;
    Object.assign(plot, store.newPlot(plot.id), { unlocked: true, tilled: false });
    emit('float', { plotId: plotId, text: '已铲除', type: 'clear' });
    return done(wasWithered ? '枯萎作物已清除' : '作物已铲除');
  }

  function useFertilizer(plotId) {
    const plot = game.state.plots[plotId];
    const crop = cropOf(plot);
    if (!crop) return fail('这里没有作物');
    if (plot.withered) return fail('枯萎的作物救不回来了');
    if (plot.ripe) return fail('作物已经成熟啦');
    if (!game.state.items.fertilizer) return fail('化肥用完了，去商店买吧');
    game.state.items.fertilizer--;
    plot.growth = Math.min(crop.growMs, plot.growth + crop.growMs * RULES.fertilizerBoost);
    if (plot.growth >= crop.growMs) {
      plot.ripe = true;
      plot.pestDamage = plot.pests ? RULES.pestYieldPenalty : 0;
    }
    emit('float', { plotId: plotId, text: '生长 +' + Math.round(RULES.fertilizerBoost * 100) + '%', type: 'fert' });
    return done('施肥成功，作物长快了');
  }

  function buyFertilizer(count) {
    count = count || 1;
    const cost = RULES.fertilizerPrice * count;
    if (game.state.player.coins < cost) return fail('金币不足');
    addCoins(-cost);
    game.state.items.fertilizer += count;
    return done('购买化肥 x' + count);
  }

  function unlockPlot(plotId) {
    const plot = game.state.plots[plotId];
    if (plot.unlocked) return fail('这块地已经开垦过了');
    const cost = cfg.plotUnlockCost(plotId);
    const need = cfg.plotUnlockLevel(plotId);
    if (game.state.player.level < need) return fail('需要 ' + need + ' 级才能开垦这块地');
    if (game.state.player.coins < cost) return fail('金币不足，需要 ' + cost + ' 金币');
    addCoins(-cost);
    plot.unlocked = true;
    addExp(5);
    emit('float', { plotId: plotId, text: '开垦成功', type: 'plant' });
    return done('新地块开垦完成');
  }

  function sell(cropId, count) {
    const crop = cfg.CROP_MAP[cropId];
    const have = game.state.bag[cropId] || 0;
    if (!crop || !have) return fail('仓库里没有这个作物');
    count = Math.min(count || 1, have);
    const gain = crop.sellPrice * count;
    game.state.bag[cropId] = have - count;
    if (!game.state.bag[cropId]) delete game.state.bag[cropId];
    addCoins(gain);
    return done('卖出 ' + crop.name + ' x' + count + '，收入 ' + gain + ' 金币', { gain: gain });
  }

  function sellAll() {
    const ids = Object.keys(game.state.bag);
    if (!ids.length) return fail('仓库空空如也');
    let gain = 0, kinds = 0;
    ids.forEach(function (id) {
      const crop = cfg.CROP_MAP[id];
      if (!crop) return;
      gain += crop.sellPrice * game.state.bag[id];
      kinds++;
      delete game.state.bag[id];
    });
    addCoins(gain);
    return done('卖出 ' + kinds + ' 种作物，收入 ' + gain + ' 金币', { gain: gain });
  }

  /* ---------------- 批量操作 ---------------- */

  function eachPlot(fn) {
    let count = 0;
    game.state.plots.forEach(function (plot) {
      if (plot.unlocked && fn(plot)) count++;
    });
    return count;
  }

  function careAll() {
    let watered = 0, weeded = 0, killed = 0;
    eachPlot(function (plot) {
      if (plot.cropId && !plot.withered && plot.water <= 85) { plot.water = 100; watered++; addExp(RULES.exp.water); }
      if (plot.weeds) { plot.weeds = false; weeded++; addExp(RULES.exp.weed); }
      if (plot.pests) { plot.pests = false; killed++; addExp(RULES.exp.pest); }
      return false;
    });
    if (!watered && !weeded && !killed) return fail('农场里一切正常，无需照料');
    return done('浇水 ' + watered + ' 块，除草 ' + weeded + ' 处，杀虫 ' + killed + ' 处');
  }

  function harvestAll() {
    let total = 0, kinds = {};
    game.state.plots.forEach(function (plot) {
      if (plot.unlocked && plot.ripe && !plot.withered) {
        const crop = cropOf(plot);
        const r = harvest(plot.id);
        if (r.ok) { total += r.amount; kinds[crop.name] = true; }
      }
    });
    if (!total) return fail('没有成熟的作物');
    return done('一键收获：共 ' + total + ' 个果实（' + Object.keys(kinds).join('、') + '）');
  }

  function tillAll() {
    let n = 0;
    eachPlot(function (plot) {
      if (!plot.tilled && !plot.cropId) { plot.tilled = true; addExp(RULES.exp.till); n++; return true; }
      return false;
    });
    if (!n) return fail('没有需要翻的地');
    return done('翻好了 ' + n + ' 块地');
  }

  function plantAll(cropId) {
    const crop = cfg.CROP_MAP[cropId];
    if (!crop) return fail('先选一种种子');
    let n = 0, lastErr = null;
    game.state.plots.forEach(function (plot) {
      if (plot.unlocked && !plot.cropId) {
        if (!plot.tilled) plot.tilled = true;
        const r = plant(plot.id, cropId);
        if (r.ok) n++; else lastErr = r.msg;
      }
    });
    if (!n) return fail(lastErr || '没有空地可以播种');
    return done('种下 ' + n + ' 块' + crop.name);
  }

  function clearAllWithered() {
    const n = eachPlot(function (plot) {
      if (plot.withered) { clearPlot(plot.id); return true; }
      return false;
    });
    return n ? done('清理了 ' + n + ' 块枯地') : fail('没有枯萎的作物');
  }

  /* ---------------- 邻居偷菜 ---------------- */

  function stealCrop(neighborId, slotIndex) {
    const now = Date.now();
    const nb = game.state.neighbors.filter(function (n) { return n.id === neighborId; })[0];
    if (!nb) return fail('找不到这位邻居');
    if (nb.cooldownUntil > now) {
      const s = Math.ceil((nb.cooldownUntil - now) / 1000);
      return fail('刚去过' + nb.name + '家，' + Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒后再来');
    }
    const slot = nb.slots[slotIndex];
    if (!slot) return fail('这块地是空的');
    if (slot.readyAt > now) return fail('这块地还没成熟');

    const crop = cfg.CROP_MAP[slot.cropId];
    nb.cooldownUntil = now + RULES.stealCooldownMs;
    nb.slots[slotIndex] = store.randomNeighborSlot(game.state.player.level);
    nb.slots[slotIndex].readyAt = now + 3 * 60 * 1000 + Math.random() * 5 * 60 * 1000;

    if (Math.random() < RULES.stealCaughtChance) {
      addCoins(-RULES.stealFine);
      return done('被' + nb.name + '发现了！赔了 ' + RULES.stealFine + ' 金币', { caught: true });
    }
    game.state.bag[crop.id] = (game.state.bag[crop.id] || 0) + 1;
    game.state.stats.stolen++;
    addExp(RULES.exp.steal);
    return done('偷到 ' + crop.name + ' x1（+' + RULES.exp.steal + ' 经验）', { crop: crop });
  }

  /* ---------------- 生命周期 ---------------- */

  function init() {
    game.state = store.load();
    const now = Date.now();
    const offline = Math.min(now - (game.state.lastTick || now), 7 * 24 * 3600 * 1000);
    if (offline > 5000) advance(offline);
    game.state.lastTick = now;
    return { offlineMs: offline };
  }

  function tick() {
    const now = Date.now();
    const dt = Math.max(0, now - game.state.lastTick);
    game.state.lastTick = now;
    if (dt > 0) advance(dt);
  }

  function persist() { return store.save(game.state); }

  function hardReset() {
    store.reset();
    game.state = store.freshState();
  }

  Object.assign(game, {
    init: init, tick: tick, advance: advance, persist: persist, hardReset: hardReset,
    cropOf: cropOf, stageOf: stageOf, progressOf: progressOf, remainingMs: remainingMs,
    expNeeded: expNeeded, bagValue: bagValue,
    till: till, plant: plant, water: water, removeWeeds: removeWeeds, killPests: killPests,
    harvest: harvest, clearPlot: clearPlot, useFertilizer: useFertilizer,
    buySeed: buySeed, buyFertilizer: buyFertilizer, unlockPlot: unlockPlot,
    sell: sell, sellAll: sellAll,
    careAll: careAll, harvestAll: harvestAll, tillAll: tillAll, plantAll: plantAll,
    clearAllWithered: clearAllWithered, stealCrop: stealCrop
  });

  Farm.game = game;
})(window.Farm);
