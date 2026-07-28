/* 作物矢量图形：按 (作物, 阶段) 参数化生成 SVG，无需任何图片资源 */
window.Farm = window.Farm || {};

(function (Farm) {
  'use strict';

  const GROUND_Y = 82;

  /** 稳定伪随机：同一地块的作物形态每次渲染保持一致 */
  function seeded(seed) {
    let s = (seed * 9301 + 49297) % 233280;
    return function () {
      s = (s * 9301 + 49297) % 233280;
      return s / 233280;
    };
  }

  function svg(inner, cls) {
    return '<svg class="plant-svg ' + (cls || '') + '" viewBox="0 0 100 100" ' +
      'xmlns="http://www.w3.org/2000/svg" aria-hidden="true">' + inner + '</svg>';
  }

  function mound(color) {
    return '<ellipse cx="50" cy="' + GROUND_Y + '" rx="26" ry="8" fill="' + (color || 'rgba(74,45,24,.35)') + '"/>';
  }

  function stem(height, width, color) {
    const top = GROUND_Y - height;
    return '<path d="M50 ' + GROUND_Y + ' C' + (50 - width) + ' ' + (GROUND_Y - height * 0.5) +
      ' ' + (50 + width) + ' ' + (GROUND_Y - height * 0.7) + ' 50 ' + top +
      '" stroke="' + color + '" stroke-width="3.4" fill="none" stroke-linecap="round"/>';
  }

  /** 一对叶子，dir=1 右侧优先，size 控制大小 */
  function leaf(x, y, size, dir, color, tilt) {
    const w = size, h = size * 0.62;
    const ex = x + dir * w, ey = y - h * (tilt || 0.5);
    return '<path d="M' + x + ' ' + y +
      ' C' + (x + dir * w * 0.25) + ' ' + (y - h) +
      ' ' + (ex - dir * w * 0.1) + ' ' + (ey - h * 0.7) +
      ' ' + ex + ' ' + ey +
      ' C' + (ex - dir * w * 0.35) + ' ' + (ey + h * 0.55) +
      ' ' + (x + dir * w * 0.35) + ' ' + (y + h * 0.28) +
      ' ' + x + ' ' + y + 'z" fill="' + color + '"/>';
  }

  function blossom(x, y, color, center, r) {
    const rad = r || 5;
    let out = '';
    for (let i = 0; i < 5; i++) {
      const a = (Math.PI * 2 * i) / 5 - Math.PI / 2;
      out += '<circle cx="' + (x + Math.cos(a) * rad).toFixed(1) + '" cy="' +
        (y + Math.sin(a) * rad).toFixed(1) + '" r="' + (rad * 0.72).toFixed(1) +
        '" fill="' + color + '"/>';
    }
    out += '<circle cx="' + x + '" cy="' + y + '" r="' + (rad * 0.55).toFixed(1) + '" fill="' + (center || '#ffe27a') + '"/>';
    return out;
  }

  function foliage(crop, size, count, rnd) {
    let out = '';
    for (let i = 0; i < count; i++) {
      const dir = i % 2 === 0 ? 1 : -1;
      const y = GROUND_Y - 4 - i * size * 0.34;
      const jitter = (rnd() - 0.5) * 0.3;
      out += leaf(50, y, size * (1 - i * 0.08), dir, i % 2 === 0 ? crop.leaf : crop.deep, 0.5 + jitter);
    }
    return out;
  }

  /* ---------- 各形态的成熟造型 ---------- */

  function ripeRoot(crop, rnd) {
    // 根茎类：果实上宽下尖，下半截被土堆挡住，营造“埋在土里”的效果
    const topY = 44;
    let out = '<path d="M33 ' + topY + ' A17 17 0 0 1 67 ' + topY +
      ' L57 88 Q50 98 43 88 Z" fill="' + crop.fruit + '"/>';
    out += '<path d="M41 ' + (topY + 4) + ' q-3 20 3 32" stroke="' + crop.accent +
      '" stroke-width="4.5" fill="none" opacity=".45" stroke-linecap="round"/>';
    for (let i = 0; i < 3; i++) {
      out += '<path d="M' + (36 + i * 2) + ' ' + (topY + 15 + i * 12) + ' q13 5 ' + (26 - i * 4) +
        ' 0" stroke="rgba(70,40,10,.13)" stroke-width="2" fill="none"/>';
    }
    // 果实两侧的土坡，不遮住果实本体
    out += '<path d="M12 100 Q20 80 37 86 L37 100 Z" fill="rgba(62,36,12,.42)"/>';
    out += '<path d="M88 100 Q80 80 63 86 L63 100 Z" fill="rgba(62,36,12,.42)"/>';
    out += '<ellipse cx="50" cy="96" rx="22" ry="6" fill="rgba(48,28,8,.25)"/>';
    for (let i = 0; i < 5; i++) {
      const dir = i % 2 === 0 ? 1 : -1;
      out += leaf(50, topY - 1 - i * 4, 20 - i * 1.6, dir, i % 2 ? crop.leaf : crop.deep, 0.85 + (rnd() - 0.5) * 0.2);
    }
    return out;
  }

  function ripeLeafBall(crop) {
    let out = mound();
    // 外层展开的大叶
    out += leaf(50, GROUND_Y - 4, 24, -1, crop.deep, 0.45) + leaf(50, GROUND_Y - 4, 24, 1, crop.deep, 0.45);
    out += '<ellipse cx="50" cy="' + (GROUND_Y - 14) + '" rx="22" ry="21" fill="' + crop.deep + '"/>';
    out += '<ellipse cx="50" cy="' + (GROUND_Y - 17) + '" rx="17" ry="18" fill="' + crop.leaf + '"/>';
    out += '<ellipse cx="50" cy="' + (GROUND_Y - 19) + '" rx="11" ry="14" fill="' + crop.fruit + '"/>';
    // 叶脉
    out += '<path d="M50 ' + (GROUND_Y - 33) + ' L50 ' + (GROUND_Y - 6) +
      ' M39 ' + (GROUND_Y - 28) + ' Q44 ' + (GROUND_Y - 16) + ' 46 ' + (GROUND_Y - 6) +
      ' M61 ' + (GROUND_Y - 28) + ' Q56 ' + (GROUND_Y - 16) + ' 54 ' + (GROUND_Y - 6) +
      '" stroke="' + crop.accent + '" stroke-width="1.8" fill="none" opacity=".85"/>';
    return out;
  }

  function ripeFruit(crop, rnd) {
    let out = mound() + stem(46, 5, crop.deep) + foliage(crop, 17, 4, rnd);
    const spots = [[36, 44], [63, 52], [50, 32]];
    spots.forEach(function (p, i) {
      const r = 9 - i;
      if (crop.id === 'strawberry') {
        // 草莓：水滴形 + 表面籽粒 + 绿萼
        out += '<path d="M' + (p[0] - r) + ' ' + (p[1] - r * 0.4) + ' A' + r + ' ' + r +
          ' 0 0 1 ' + (p[0] + r) + ' ' + (p[1] - r * 0.4) + ' Q' + p[0] + ' ' + (p[1] + r * 1.5) +
          ' ' + (p[0] - r) + ' ' + (p[1] - r * 0.4) + ' z" fill="' + crop.fruit + '"/>';
        out += '<circle cx="' + (p[0] - 2) + '" cy="' + (p[1] - 1) + '" r="1" fill="#fff5d0"/>' +
          '<circle cx="' + (p[0] + 2.4) + '" cy="' + (p[1] + 1.6) + '" r="1" fill="#fff5d0"/>' +
          '<circle cx="' + p[0] + '" cy="' + (p[1] + 4) + '" r="1" fill="#fff5d0"/>';
        out += '<path d="M' + (p[0] - r * 0.9) + ' ' + (p[1] - r * 0.55) + ' q' + (r * 0.9) + ' -4 ' +
          (r * 1.8) + ' 0 q-' + (r * 0.9) + ' 4 -' + (r * 1.8) + ' 0z" fill="' + crop.deep + '"/>';
      } else {
        out += '<circle cx="' + p[0] + '" cy="' + p[1] + '" r="' + r + '" fill="' + crop.fruit + '"/>';
        out += '<circle cx="' + (p[0] - r * 0.3) + '" cy="' + (p[1] - r * 0.35) + '" r="' + (r * 0.3) + '" fill="' + crop.accent + '" opacity=".75"/>';
        out += '<path d="M' + p[0] + ' ' + (p[1] - r) + ' l0 -4" stroke="' + crop.deep + '" stroke-width="2.4" stroke-linecap="round"/>';
      }
    });
    return out;
  }

  function ripeTall(crop, rnd) {
    let out = mound() + '<path d="M50 ' + GROUND_Y + ' L50 14" stroke="' + crop.deep + '" stroke-width="4.5" stroke-linecap="round"/>';
    for (let i = 0; i < 5; i++) {
      const dir = i % 2 === 0 ? 1 : -1;
      out += leaf(50, GROUND_Y - 8 - i * 13, 22 - i * 1.5, dir, i % 2 ? crop.leaf : crop.deep, 0.75 + (rnd() - 0.5) * 0.2);
    }
    if (crop.id === 'sunflower') {
      out += '<g>' + blossom(50, 20, crop.fruit, crop.accent, 13) + '</g>';
      out += '<circle cx="50" cy="20" r="7" fill="' + crop.accent + '"/>';
    } else {
      // 玉米棒
      out += '<g transform="rotate(14 62 42)">' +
        '<ellipse cx="62" cy="42" rx="8" ry="16" fill="' + crop.fruit + '"/>' +
        '<ellipse cx="62" cy="42" rx="4" ry="13" fill="' + crop.accent + '"/>' +
        '<path d="M55 32 q7 -12 14 0 q-7 22 -14 0z" fill="' + crop.leaf + '" opacity=".9"/></g>';
      out += '<g transform="rotate(-16 38 52)">' +
        '<ellipse cx="38" cy="52" rx="7" ry="14" fill="' + crop.fruit + '"/>' +
        '<ellipse cx="38" cy="52" rx="3.4" ry="11" fill="' + crop.accent + '"/></g>';
    }
    return out;
  }

  function ripeVine(crop, rnd) {
    let out = mound();
    out += '<path d="M22 ' + (GROUND_Y - 2) + ' q28 -14 56 0" stroke="' + crop.deep + '" stroke-width="3" fill="none"/>';
    out += leaf(26, GROUND_Y - 6, 18, -1, crop.leaf, 0.7) + leaf(74, GROUND_Y - 6, 18, 1, crop.deep, 0.7);
    out += '<ellipse cx="50" cy="' + (GROUND_Y - 16) + '" rx="22" ry="18" fill="' + crop.fruit + '"/>';
    if (crop.id === 'watermelon') {
      for (let i = -2; i <= 2; i++) {
        out += '<path d="M' + (50 + i * 8) + ' ' + (GROUND_Y - 33) + ' q' + (i * 3) + ' 17 0 33" stroke="' + crop.accent + '" stroke-width="2.6" fill="none" opacity=".85"/>';
      }
    } else {
      for (let i = -2; i <= 2; i++) {
        out += '<path d="M' + (50 + i * 8.5) + ' ' + (GROUND_Y - 33) + ' q' + (i * 2) +
          ' 17 0 33" stroke="rgba(150,66,8,.5)" stroke-width="2.6" fill="none"/>';
      }
    }
    out += '<path d="M50 ' + (GROUND_Y - 34) + ' l0 -7" stroke="' + crop.deep + '" stroke-width="3.4" stroke-linecap="round"/>';
    out += '<ellipse cx="42" cy="' + (GROUND_Y - 24) + '" rx="5" ry="7" fill="#fff" opacity=".18"/>';
    void rnd;
    return out;
  }

  function ripeCluster(crop, rnd) {
    let out = mound() + '<path d="M50 ' + GROUND_Y + ' L50 26" stroke="#7a5230" stroke-width="5" stroke-linecap="round"/>';
    out += '<path d="M50 40 q-16 -8 -22 -18M50 34 q16 -8 22 -17" stroke="#7a5230" stroke-width="3.4" fill="none" stroke-linecap="round"/>';
    for (let i = 0; i < 5; i++) {
      out += leaf(50, 34 - i * 3 + (i % 2) * 10, 19, i % 2 === 0 ? 1 : -1, i % 2 ? crop.leaf : crop.deep, 0.8);
    }
    // 两串果实
    [[34, 48], [66, 44]].forEach(function (base) {
      for (let row = 0; row < 4; row++) {
        const n = 3 - Math.floor(row / 1.6);
        for (let k = 0; k < n; k++) {
          const cx = base[0] + (k - (n - 1) / 2) * 8 + (rnd() - 0.5) * 1.6;
          const cy = base[1] + row * 7;
          out += '<circle cx="' + cx.toFixed(1) + '" cy="' + cy.toFixed(1) + '" r="4.3" fill="' + crop.fruit + '"/>';
          out += '<circle cx="' + (cx - 1.2).toFixed(1) + '" cy="' + (cy - 1.4).toFixed(1) + '" r="1.5" fill="' + crop.accent + '" opacity=".8"/>';
        }
      }
    });
    return out;
  }

  const RIPE = {
    root: ripeRoot,
    leaf: ripeLeafBall,
    fruit: ripeFruit,
    tall: ripeTall,
    vine: ripeVine,
    cluster: ripeCluster
  };

  /**
   * 生成作物图形
   * @param {object} crop 作物配置
   * @param {string} stageKey seed/sprout/small/grown/flower/ripe/withered
   * @param {number} seed 稳定随机种子
   */
  function plant(crop, stageKey, seed) {
    const rnd = seeded((seed || 1) + crop.name.length);

    if (stageKey === 'withered') {
      let out = mound('rgba(60,40,20,.4)');
      out += '<path d="M50 ' + GROUND_Y + ' l-9 -22 M50 ' + GROUND_Y + ' l8 -18 M50 ' + GROUND_Y + ' l1 -26" ' +
        'stroke="#9a8460" stroke-width="3" stroke-linecap="round" fill="none"/>';
      out += '<path d="M41 60 l-7 -5 M58 64 l7 -6" stroke="#b09b74" stroke-width="2.6" stroke-linecap="round"/>';
      return svg(out, 'is-withered');
    }

    if (stageKey === 'seed') {
      let out = mound('rgba(74,45,24,.45)');
      out += '<ellipse cx="50" cy="' + (GROUND_Y - 3) + '" rx="8" ry="5" fill="rgba(90,58,30,.75)"/>';
      out += '<circle cx="50" cy="' + (GROUND_Y - 5) + '" r="2.6" fill="#e9d8a6"/>';
      return svg(out, 'is-seed');
    }

    if (stageKey === 'sprout') {
      let out = mound();
      out += '<path d="M50 ' + GROUND_Y + ' L50 ' + (GROUND_Y - 12) + '" stroke="' + crop.deep + '" stroke-width="2.6" stroke-linecap="round"/>';
      out += leaf(50, GROUND_Y - 11, 12, 1, crop.leaf, 0.6) + leaf(50, GROUND_Y - 11, 11, -1, crop.deep, 0.6);
      return svg(out, 'is-sprout');
    }

    if (stageKey === 'small') {
      let out = mound() + stem(22, 3, crop.deep) + foliage(crop, 14, 3, rnd);
      return svg(out, 'is-small');
    }

    if (stageKey === 'grown') {
      const tall = crop.shape === 'tall' || crop.shape === 'cluster';
      let out = mound() + stem(tall ? 44 : 32, 4, crop.deep) + foliage(crop, 17, tall ? 5 : 4, rnd);
      return svg(out, 'is-grown');
    }

    if (stageKey === 'flower') {
      const tall = crop.shape === 'tall' || crop.shape === 'cluster';
      let out = mound() + stem(tall ? 50 : 38, 4.5, crop.deep) + foliage(crop, 18, tall ? 5 : 4, rnd);
      const fy = tall ? 26 : GROUND_Y - 42;
      out += blossom(50, fy, '#fff6d8', crop.fruit, 7);
      out += blossom(36, fy + 12, '#fff6d8', crop.fruit, 5);
      out += blossom(64, fy + 15, '#fff6d8', crop.fruit, 4.5);
      return svg(out, 'is-flower');
    }

    return svg((RIPE[crop.shape] || ripeFruit)(crop, rnd), 'is-ripe');
  }

  /** 杂草装饰 */
  function weeds(seed) {
    const rnd = seeded(seed || 3);
    let out = '';
    for (let i = 0; i < 3; i++) {
      const x = 20 + rnd() * 60, y = 62 + rnd() * 22, s = 0.7 + rnd() * 0.5;
      out += '<g transform="translate(' + x.toFixed(1) + ' ' + y.toFixed(1) + ') scale(' + s.toFixed(2) + ')">' +
        '<path d="M0 0 q-6 -8 -9 -14 M0 0 q1 -10 0 -16 M0 0 q6 -7 10 -13" stroke="#6e8f3a" ' +
        'stroke-width="2.6" fill="none" stroke-linecap="round"/></g>';
    }
    return '<svg class="weed-svg" viewBox="0 0 100 100" aria-hidden="true">' + out + '</svg>';
  }

  /** 害虫装饰 */
  function pests(seed) {
    const rnd = seeded(seed || 7);
    let out = '';
    for (let i = 0; i < 2; i++) {
      const x = 26 + rnd() * 48, y = 40 + rnd() * 30;
      out += '<g class="bug" transform="translate(' + x.toFixed(1) + ' ' + y.toFixed(1) + ')">' +
        '<ellipse cx="0" cy="0" rx="7" ry="4.6" fill="#7fbf3f"/>' +
        '<ellipse cx="5.5" cy="-1" rx="3.4" ry="3.4" fill="#5f9a2c"/>' +
        '<circle cx="6.6" cy="-1.8" r="1" fill="#22331a"/>' +
        '<path d="M-5 -3.6 l-2 -3 M0 -4.2 l0 -3.4 M4 -4 l2 -3" stroke="#4f8226" stroke-width="1.4" stroke-linecap="round"/>' +
        '</g>';
    }
    return '<svg class="pest-svg" viewBox="0 0 100 100" aria-hidden="true">' + out + '</svg>';
  }

  /** 玩家头像 */
  function avatar(color) {
    return '<svg viewBox="0 0 64 64" class="avatar-svg" aria-hidden="true">' +
      '<circle cx="32" cy="32" r="32" fill="' + (color || '#8ecb54') + '"/>' +
      '<circle cx="32" cy="26" r="11" fill="#ffe0bd"/>' +
      '<path d="M32 14 a12 12 0 0 1 12 9 h-24 a12 12 0 0 1 12 -9z" fill="#7a4a22"/>' +
      '<path d="M12 62 q6 -18 20 -18 t20 18z" fill="#f7f3e8"/>' +
      '<circle cx="27.5" cy="27" r="1.9" fill="#3a2a1a"/><circle cx="36.5" cy="27" r="1.9" fill="#3a2a1a"/>' +
      '<path d="M28 32 q4 3.4 8 0" stroke="#c96b5c" stroke-width="1.8" fill="none" stroke-linecap="round"/>' +
      '</svg>';
  }

  Farm.sprites = { plant: plant, weeds: weeds, pests: pests, avatar: avatar };
})(window.Farm);
