/* 启动流程：读档 -> 补算离线 -> 挂载界面 -> 心跳循环 */
(function (Farm) {
  'use strict';

  const game = Farm.game;
  const ui = Farm.ui;

  function offlineReport(ms) {
    const min = Math.floor(ms / 60000);
    if (min < 1) return;
    const h = Math.floor(min / 60);
    const text = h ? h + ' 小时 ' + (min % 60) + ' 分钟' : min + ' 分钟';
    const ripe = game.state.plots.filter(function (p) { return p.ripe; }).length;
    const withered = game.state.plots.filter(function (p) { return p.withered; }).length;
    let body = '<p>你离开了 <b>' + text + '</b>，农场一直在生长。</p><ul class="rules">';
    body += '<li>可收获的地块：<b>' + ripe + '</b> 块</li>';
    if (withered) body += '<li>因为太久没收而枯萎：<b>' + withered + '</b> 块（用铲子清掉）</li>';
    body += '<li>记得浇水除草，不然长得很慢哦</li></ul>';
    ui.showModal('🌤 欢迎回来', body);
  }

  function bindShortcuts() {
    document.addEventListener('keydown', function (e) {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const map = { '1': 'hand', '2': 'hoe', '3': 'seed', '4': 'water', '5': 'weed', '6': 'pest', '7': 'fert', '8': 'shovel' };
      if (map[e.key]) {
        const btn = document.querySelector('.tool[data-tool="' + map[e.key] + '"]');
        if (btn) btn.click();
      } else if (e.key.toLowerCase() === 'h') {
        const btn = document.querySelector('[data-act="harvestAll"]');
        if (btn) btn.click();
      } else if (e.key.toLowerCase() === 'w') {
        const btn = document.querySelector('[data-act="careAll"]');
        if (btn) btn.click();
      }
    });
  }

  function start() {
    const info = game.init();
    ui.mount();
    offlineReport(info.offlineMs);
    bindShortcuts();

    setInterval(function () {
      game.tick();
      ui.renderTick();
    }, Farm.config.RULES.tickMs);

    setInterval(function () { game.persist(); }, 10000);

    window.addEventListener('beforeunload', function () { game.persist(); });
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) {
        game.persist();
      } else {
        game.tick();
        ui.render();
      }
    });
    document.addEventListener('pointerdown', function once() {
      Farm.audio.unlock();
      document.removeEventListener('pointerdown', once);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})(window.Farm);
