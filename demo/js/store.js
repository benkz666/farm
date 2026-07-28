/* 存档与状态：localStorage 持久化 + 离线时间的生长补算 */
window.Farm = window.Farm || {};

(function (Farm) {
  'use strict';

  const SAVE_KEY = 'qq-farm-save-v1';
  const cfg = Farm.config;
  const RULES = cfg.RULES;

  function newPlot(id) {
    return {
      id: id,
      unlocked: id < RULES.initialUnlocked,
      tilled: false,
      cropId: null,
      growth: 0,        // 已累积的有效生长毫秒
      water: 0,         // 0 - 100
      weeds: false,
      pests: false,
      pestDamage: 0,    // 成熟时结算的减产量
      ripe: false,
      ripeElapsed: 0,
      withered: false,
      plantedAt: 0
    };
  }

  function randomNeighborSlot(level) {
    const pool = cfg.CROPS.filter(function (c) { return c.level <= Math.max(3, level + 2); });
    const crop = pool[Math.floor(Math.random() * pool.length)];
    return {
      cropId: crop.id,
      readyAt: Date.now() + Math.floor(Math.random() * 6 * 60 * 1000),
      stolenBy: 0
    };
  }

  function newNeighbors(level) {
    return cfg.NEIGHBOR_NAMES.map(function (n, i) {
      const slots = [];
      for (let k = 0; k < 6; k++) slots.push(randomNeighborSlot(level));
      return { id: 'n' + i, name: n.name, avatar: n.avatar, slots: slots, cooldownUntil: 0 };
    });
  }

  function freshState() {
    const plots = [];
    for (let i = 0; i < RULES.plotCount; i++) plots.push(newPlot(i));
    return {
      version: 1,
      player: { name: '农场主', level: 1, exp: 0, coins: RULES.initialCoins, avatar: '#8ecb54' },
      plots: plots,
      seeds: {},
      bag: {},
      items: { fertilizer: 2 },
      neighbors: newNeighbors(1),
      stats: { planted: 0, harvested: 0, stolen: 0, earned: 0 },
      settings: { sound: true },
      lastTick: Date.now(),
      createdAt: Date.now()
    };
  }

  function load() {
    let raw = null;
    try { raw = localStorage.getItem(SAVE_KEY); } catch (e) { raw = null; }
    if (!raw) return freshState();
    try {
      const data = JSON.parse(raw);
      if (!data || data.version !== 1 || !Array.isArray(data.plots)) return freshState();
      // 兼容旧档：补齐缺失字段
      const base = freshState();
      const state = Object.assign(base, data);
      state.player = Object.assign(base.player, data.player);
      state.stats = Object.assign(base.stats, data.stats);
      state.settings = Object.assign(base.settings, data.settings);
      state.items = Object.assign(base.items, data.items);
      while (state.plots.length < RULES.plotCount) state.plots.push(newPlot(state.plots.length));
      state.plots = state.plots.slice(0, RULES.plotCount).map(function (p, i) {
        return Object.assign(newPlot(i), p, { id: i });
      });
      if (!Array.isArray(state.neighbors) || !state.neighbors.length) {
        state.neighbors = newNeighbors(state.player.level);
      }
      return state;
    } catch (e) {
      return freshState();
    }
  }

  function save(state) {
    try {
      localStorage.setItem(SAVE_KEY, JSON.stringify(state));
      return true;
    } catch (e) {
      return false;
    }
  }

  function reset() {
    try { localStorage.removeItem(SAVE_KEY); } catch (e) { /* ignore */ }
  }

  Farm.store = {
    SAVE_KEY: SAVE_KEY,
    freshState: freshState,
    newPlot: newPlot,
    newNeighbors: newNeighbors,
    randomNeighborSlot: randomNeighborSlot,
    load: load,
    save: save,
    reset: reset
  };
})(window.Farm);
