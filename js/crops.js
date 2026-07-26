// ============================================================
// 作物 3D 模型工厂：低多边形、flatShading，按生长阶段构建
// ============================================================
import * as THREE from 'three';

const matCache = new Map();
export function mat(color, emissive = 0) {
  const key = color + '|' + emissive;
  if (!matCache.has(key)) {
    matCache.set(key, new THREE.MeshStandardMaterial({
      color, flatShading: true, roughness: 0.85, metalness: 0,
      emissive, emissiveIntensity: emissive ? 0.5 : 0,
    }));
  }
  return matCache.get(key);
}

function sphere(r, color, w = 6, h = 5) {
  const m = new THREE.Mesh(new THREE.SphereGeometry(r, w, h), mat(color));
  m.castShadow = true; return m;
}
function ico(r, color, d = 0) {
  const m = new THREE.Mesh(new THREE.IcosahedronGeometry(r, d), mat(color));
  m.castShadow = true; return m;
}
function cone(r, h, color, seg = 6) {
  const m = new THREE.Mesh(new THREE.ConeGeometry(r, h, seg), mat(color));
  m.castShadow = true; return m;
}
function cyl(rt, rb, h, color, seg = 6) {
  const m = new THREE.Mesh(new THREE.CylinderGeometry(rt, rb, h, seg), mat(color));
  m.castShadow = true; return m;
}
function box(w, h, d, color) {
  const m = new THREE.Mesh(new THREE.BoxGeometry(w, h, d), mat(color));
  m.castShadow = true; return m;
}

const TRUNK = 0x8d6e63, SOIL_STEM = 0x6d4c41;

// 通用幼苗：两片子叶
function sprout(scale) {
  const g = new THREE.Group();
  const stem = cyl(0.05, 0.07, 0.35 * scale, 0x7fb069);
  stem.position.y = 0.18 * scale; g.add(stem);
  for (const s of [-1, 1]) {
    const leaf = sphere(0.16 * scale, 0x66bb6a, 5, 4);
    leaf.scale.set(1.4, 0.5, 0.8);
    leaf.position.set(0.16 * scale * s, 0.38 * scale, 0);
    leaf.rotation.z = -0.5 * s; g.add(leaf);
  }
  return g;
}

function leafCluster(count, r, color, y = 0, spread = 0.3) {
  const g = new THREE.Group();
  for (let i = 0; i < count; i++) {
    const a = (i / count) * Math.PI * 2;
    const leaf = cone(r * 0.5, r * 2.2, color, 4);
    leaf.position.set(Math.cos(a) * spread, y + r * 0.8, Math.sin(a) * spread);
    leaf.rotation.set(Math.sin(a) * 0.7, 0, -Math.cos(a) * 0.7);
    g.add(leaf);
  }
  return g;
}

function flowerCluster(count, y, spread, color = 0xfff7f0) {
  const g = new THREE.Group();
  for (let i = 0; i < count; i++) {
    const a = Math.random() * Math.PI * 2, r = spread * (0.4 + Math.random() * 0.6);
    const f = ico(0.12, color);
    f.position.set(Math.cos(a) * r, y + Math.random() * 0.25, Math.sin(a) * r);
    g.add(f);
  }
  return g;
}

function fruitBalls(def, positions, r = 0.22) {
  const g = new THREE.Group();
  for (const p of positions) {
    const f = sphere(r, def.fruit, 6, 5);
    f.position.set(p[0], p[1], p[2]); g.add(f);
  }
  return g;
}

function ringPositions(n, radius, y, jitter = 0.15) {
  const out = [];
  for (let i = 0; i < n; i++) {
    const a = (i / n) * Math.PI * 2 + Math.random() * 0.5;
    out.push([Math.cos(a) * radius, y + (Math.random() - 0.5) * jitter * 2, Math.sin(a) * radius]);
  }
  return out;
}

// ---- 各体型构建（返回完整成熟体，果实单独分组）----
const BUILDERS = {
  root(def, fruits) {  // 白萝卜/胡萝卜/人参：根茎露出 + 叶簇
    const g = new THREE.Group();
    const isCarrot = def.id === 'huluobo';
    let rootBody;
    if (isCarrot) {
      rootBody = cone(0.4, 1.1, def.fruit, 7);
      rootBody.rotation.x = Math.PI; rootBody.position.y = 0.28;
    } else {
      rootBody = sphere(def.id === 'renshen' ? 0.34 : 0.48, def.fruit, 7, 6);
      rootBody.scale.y = def.id === 'renshen' ? 1.5 : 0.95;
      rootBody.position.y = 0.3;
    }
    g.add(rootBody);
    const leaves = leafCluster(5, 0.35, def.leaf, 0.55, 0.12);
    g.add(leaves);
    if (def.id === 'renshen') {  // 人参顶端红果
      for (let i = 0; i < 3; i++) {
        const b = sphere(0.09, 0xd7263d, 5, 4);
        b.position.set((i - 1) * 0.14, 1.05, 0); fruits.add(b);
      }
    }
    return g;
  },

  cabbage(def) {  // 大白菜：层叠叶球
    const g = new THREE.Group();
    const outer = sphere(0.62, def.leaf, 7, 6); outer.scale.y = 0.8; outer.position.y = 0.45; g.add(outer);
    const inner = sphere(0.45, def.fruit, 7, 6); inner.scale.y = 0.95; inner.position.y = 0.62; g.add(inner);
    const core = sphere(0.26, 0xeef7e0, 6, 5); core.position.y = 0.85; g.add(core);
    return g;
  },

  cereal(def, fruits) {  // 小麦/水稻：秸秆束 + 穗
    const g = new THREE.Group();
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2, r = 0.18 + Math.random() * 0.18;
      const x = Math.cos(a) * r, z = Math.sin(a) * r;
      const h = 1.3 + Math.random() * 0.3;
      const stem = cyl(0.03, 0.045, h, def.leaf, 4);
      stem.position.set(x, h / 2, z); stem.rotation.set((Math.random() - 0.5) * 0.15, 0, (Math.random() - 0.5) * 0.15);
      g.add(stem);
      const ear = ico(0.16, def.fruit);
      ear.scale.set(0.7, 1.6, 0.7); ear.position.set(x, h + 0.12, z);
      fruits.add(ear);
    }
    return g;
  },

  corn(def, fruits) {  // 玉米
    const g = new THREE.Group();
    const stem = cyl(0.07, 0.1, 2.3, def.leaf, 5); stem.position.y = 1.15; g.add(stem);
    for (let i = 0; i < 4; i++) {
      const a = i * 1.7 + 0.4;
      const leaf = cone(0.16, 1.3, def.leaf, 4);
      leaf.scale.z = 0.35;
      leaf.position.set(Math.cos(a) * 0.35, 0.9 + i * 0.3, Math.sin(a) * 0.35);
      leaf.rotation.set(Math.sin(a) * 1.1, 0, -Math.cos(a) * 1.1);
      g.add(leaf);
    }
    for (const [y, a] of [[1.0, 0.6], [1.45, 2.8]]) {
      const cob = cyl(0.14, 0.17, 0.55, def.fruit, 6);
      cob.position.set(Math.cos(a) * 0.24, y, Math.sin(a) * 0.24);
      cob.rotation.set(Math.sin(a) * 0.5, 0, -Math.cos(a) * 0.5);
      fruits.add(cob);
    }
    const tassel = cone(0.12, 0.5, 0xd9c25e, 5); tassel.position.y = 2.55; g.add(tassel);
    return g;
  },

  bush(def, fruits) {  // 灌木类：土豆/茄子/番茄/豌豆/辣椒
    const g = new THREE.Group();
    for (let i = 0; i < 4; i++) {
      const a = (i / 4) * Math.PI * 2 + 0.5;
      const s = ico(0.42 + Math.random() * 0.15, def.leaf);
      s.position.set(Math.cos(a) * 0.3, 0.45 + Math.random() * 0.25, Math.sin(a) * 0.3);
      g.add(s);
    }
    const top = ico(0.45, def.leaf); top.position.y = 0.85; g.add(top);
    const fr = { qiezi: 0.24, fanqie: 0.22, lajiao: 0.16, wandou: 0.16, tudou: 0.2 }[def.id] || 0.2;
    const pos = ringPositions(def.id === 'lajiao' ? 6 : 5, 0.48, 0.55, 0.2);
    for (const p of pos) {
      let f;
      if (def.id === 'qiezi') { f = sphere(fr, def.fruit, 6, 5); f.scale.y = 1.5; }
      else if (def.id === 'lajiao') { f = cone(fr, 0.42, def.fruit, 5); f.rotation.x = Math.PI * 0.9; }
      else if (def.id === 'wandou') { f = sphere(fr, def.fruit, 5, 4); f.scale.set(0.8, 1.6, 0.6); }
      else f = sphere(fr, def.fruit, 6, 5);
      f.position.set(p[0], p[1] - 0.1, p[2]); fruits.add(f);
    }
    return g;
  },

  rose(def, fruits) {  // 红玫瑰
    const g = new THREE.Group();
    for (let i = 0; i < 3; i++) {
      const a = (i / 3) * Math.PI * 2;
      const s = ico(0.35, def.leaf);
      s.position.set(Math.cos(a) * 0.3, 0.4, Math.sin(a) * 0.3); g.add(s);
    }
    for (let i = 0; i < 4; i++) {
      const a = (i / 4) * Math.PI * 2 + 0.4;
      const stem = cyl(0.03, 0.04, 0.9, def.leaf, 4);
      stem.position.set(Math.cos(a) * 0.2, 0.75, Math.sin(a) * 0.2); g.add(stem);
      const bloom = ico(0.2, def.fruit, 1);
      bloom.position.set(Math.cos(a) * 0.2, 1.25, Math.sin(a) * 0.2);
      const petal = cone(0.12, 0.2, def.fruit, 6);
      petal.position.copy(bloom.position); petal.position.y += 0.15;
      fruits.add(bloom, petal);
    }
    return g;
  },

  ground(def, fruits) {  // 南瓜/西瓜：贴地藤蔓
    const g = new THREE.Group();
    for (let i = 0; i < 4; i++) {
      const a = (i / 4) * Math.PI * 2 + 0.3;
      const vine = box(1.3, 0.08, 0.12, def.leaf);
      vine.position.set(Math.cos(a) * 0.6, 0.06, Math.sin(a) * 0.6);
      vine.rotation.y = -a; g.add(vine);
      const leaf = sphere(0.3, def.leaf, 5, 4);
      leaf.scale.set(1.2, 0.35, 1);
      leaf.position.set(Math.cos(a) * 1.1, 0.14, Math.sin(a) * 1.1); g.add(leaf);
    }
    if (def.id === 'nangua') {
      const p = sphere(0.62, def.fruit, 8, 6); p.scale.y = 0.72; p.position.set(0.25, 0.42, 0.15); fruits.add(p);
      const stem = cyl(0.05, 0.08, 0.25, 0x6d4c41, 4); stem.position.set(0.25, 0.92, 0.15); fruits.add(stem);
    } else {  // 西瓜
      const w = sphere(0.58, 0x2e933c, 8, 6); w.scale.y = 0.85; w.position.set(-0.2, 0.44, 0.2); fruits.add(w);
      const stripe = sphere(0.585, 0x1b5e20, 8, 6); stripe.scale.set(0.35, 0.85, 1.01); stripe.position.copy(w.position); fruits.add(stripe);
    }
    return g;
  },

  low(def, fruits) {  // 草莓
    const g = new THREE.Group();
    for (let i = 0; i < 5; i++) {
      const a = (i / 5) * Math.PI * 2;
      const leaf = sphere(0.24, def.leaf, 5, 4);
      leaf.scale.set(1.3, 0.4, 0.9);
      leaf.position.set(Math.cos(a) * 0.3, 0.16, Math.sin(a) * 0.3);
      leaf.rotation.y = a; g.add(leaf);
    }
    for (let i = 0; i < 5; i++) {
      const a = (i / 5) * Math.PI * 2 + 0.5;
      const b = sphere(0.14, def.fruit, 5, 4); b.scale.y = 1.25;
      b.position.set(Math.cos(a) * 0.42, 0.16, Math.sin(a) * 0.42);
      const cap = cone(0.08, 0.1, def.leaf, 4);
      cap.position.set(b.position.x, 0.32, b.position.z);
      fruits.add(b, cap);
    }
    return g;
  },

  tree(def, fruits) {  // 果树：红枣/苹果/桃/橙/石榴/柚
    const g = new THREE.Group();
    const trunk = cyl(0.12, 0.18, 1.4, TRUNK, 5); trunk.position.y = 0.7; g.add(trunk);
    const c1 = ico(0.85, def.leaf); c1.position.y = 1.9; g.add(c1);
    const c2 = ico(0.6, def.leaf); c2.position.set(0.55, 1.55, 0.3); g.add(c2);
    const c3 = ico(0.55, def.leaf); c3.position.set(-0.5, 1.6, -0.35); g.add(c3);
    const pos = [[0.7, 1.7, 0.6], [-0.75, 1.85, 0.25], [0.15, 2.35, -0.55], [-0.3, 1.45, -0.7], [0.5, 2.3, 0.35], [-0.1, 2.6, 0.15], [0.85, 2.05, -0.2]];
    fruits.add(fruitBalls(def, pos, def.id === 'hongzao' ? 0.15 : 0.2));
    return g;
  },

  vine(def, fruits) {  // 葡萄/葫芦：木架藤蔓
    const g = new THREE.Group();
    for (const s of [-1, 1]) {
      const post = box(0.1, 1.8, 0.1, TRUNK);
      post.position.set(0.8 * s, 0.9, 0); g.add(post);
    }
    const beam = box(1.9, 0.1, 0.1, TRUNK); beam.position.y = 1.8; g.add(beam);
    const vineM = box(1.7, 0.12, 0.12, def.leaf); vineM.position.y = 1.65; vineM.rotation.z = 0.1; g.add(vineM);
    for (let i = 0; i < 3; i++) {
      const leaf = sphere(0.22, def.leaf, 5, 4); leaf.scale.set(1.2, 0.4, 1);
      leaf.position.set(-0.6 + i * 0.6, 1.78, 0.1); g.add(leaf);
    }
    if (def.id === 'putao') {
      for (const x of [-0.45, 0.35]) {
        for (let r = 0; r < 3; r++) for (let c = 0; c <= r; c++) {
          const b = sphere(0.11, def.fruit, 5, 4);
          b.position.set(x + (c - r / 2) * 0.2, 1.45 - r * 0.18, 0.08);
          fruits.add(b);
        }
      }
    } else {  // 葫芦
      for (const x of [-0.4, 0.3]) {
        const up = sphere(0.16, def.fruit, 5, 4); up.position.set(x, 1.32, 0.05);
        const dn = sphere(0.24, def.fruit, 6, 5); dn.position.set(x, 1.08, 0.05);
        fruits.add(up, dn);
      }
    }
    return g;
  },

  palm(def, fruits) {  // 香蕉/椰子
    const g = new THREE.Group();
    const trunk = cyl(0.1, 0.16, 2.5, TRUNK, 5); trunk.position.y = 1.25; trunk.rotation.z = 0.06; g.add(trunk);
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2;
      const leaf = cone(0.22, 1.6, def.leaf, 4);
      leaf.scale.z = 0.3;
      leaf.position.set(Math.cos(a) * 0.55, 2.6, Math.sin(a) * 0.55);
      leaf.rotation.set(Math.sin(a) * 1.25, 0, -Math.cos(a) * 1.25);
      g.add(leaf);
    }
    if (def.id === 'xiangjiao') {
      for (let i = 0; i < 4; i++) {
        const b = cyl(0.07, 0.07, 0.42, def.fruit, 5);
        b.position.set(0.3 + Math.cos(i) * 0.14, 2.28 - i * 0.05, Math.sin(i * 2) * 0.14);
        b.rotation.z = 0.9 + i * 0.2; fruits.add(b);
      }
    } else {
      for (let i = 0; i < 3; i++) {
        const c = sphere(0.19, def.fruit, 6, 5);
        c.position.set(Math.cos(i * 2.1) * 0.28, 2.35, Math.sin(i * 2.1) * 0.28);
        fruits.add(c);
      }
    }
    return g;
  },

  pineapple(def, fruits) {  // 菠萝
    const g = new THREE.Group();
    const leaves = leafCluster(7, 0.3, def.leaf, 0, 0.18);
    g.add(leaves);
    const bodyM = cyl(0.28, 0.34, 0.7, def.fruit, 7); bodyM.position.y = 0.75; fruits.add(bodyM);
    const cap = leafCluster(5, 0.16, 0x6fbf63, 1.1, 0.06); fruits.add(cap);
    return g;
  },

  fungus(def, fruits) {  // 灵芝：层叠扇
    const g = new THREE.Group();
    const stem = cyl(0.09, 0.13, 0.5, 0xe8d8b9, 5); stem.position.y = 0.25; g.add(stem);
    const l1 = sphere(0.5, def.fruit, 7, 5); l1.scale.set(1.2, 0.35, 1); l1.position.y = 0.6; fruits.add(l1);
    const l2 = sphere(0.36, 0xb07d4f, 6, 5); l2.scale.set(1.2, 0.3, 1); l2.position.y = 0.82; fruits.add(l2);
    const rim = sphere(0.52, 0xe8d8b9, 7, 4); rim.scale.set(1.22, 0.12, 1.02); rim.position.y = 0.58; fruits.add(rim);
    return g;
  },

  money(def, fruits) {  // 摇钱树：金冠 + 金币
    const g = new THREE.Group();
    const trunk = cyl(0.14, 0.2, 1.6, 0x7d5a43, 5); trunk.position.y = 0.8; g.add(trunk);
    const c1 = ico(0.9, def.leaf); c1.position.y = 2.1; g.add(c1);
    const c2 = ico(0.6, def.leaf); c2.position.set(0.6, 1.75, 0.35); g.add(c2);
    const c3 = ico(0.55, def.leaf); c3.position.set(-0.55, 1.8, -0.3); g.add(c3);
    const pos = ringPositions(8, 0.75, 2.0, 0.35);
    for (const p of pos) {
      const coin = cyl(0.16, 0.16, 0.05, 0xffd700, 10);
      coin.material = mat(0xffd700, 0xcc8800);
      coin.position.set(p[0], p[1], p[2]);
      coin.rotation.set(Math.PI / 2, 0, Math.random() * Math.PI);
      fruits.add(coin);
    }
    return g;
  },
};

/**
 * 创建作物模型
 * @param def 作物定义
 * @param opts { stage, totalStages, mature, withered }
 */
export function createCropModel(def, opts) {
  const g = new THREE.Group();
  if (opts.withered) {  // 枯萎：灰褐残株
    const dead = BUILDERS[def.body] ? buildSafe(def, new THREE.Group()) : null;
    const wrap = new THREE.Group();
    if (dead) {
      dead.traverse(o => { if (o.isMesh) o.material = mat(0x9e8a72); });
      dead.scale.setScalar(0.8); dead.rotation.z = 0.18; wrap.add(dead);
    }
    return wrap;
  }
  if (!opts.mature && opts.stage === 0) { g.add(sprout(0.8)); return g; }
  if (!opts.mature && opts.stage === 1) { g.add(sprout(1.6)); return g; }

  const fruits = new THREE.Group();
  const body = buildSafe(def, fruits);
  const scale = opts.mature ? 1 : (opts.stage >= opts.totalStages - 1 ? 0.92 : 0.7);
  body.scale.setScalar(scale);
  fruits.scale.setScalar(scale);
  g.add(body);
  if (opts.mature) g.add(fruits);   // 仅成熟阶段显示果实
  if (!opts.mature && opts.stage >= 3) {  // 开花阶段
    const fl = flowerCluster(4, 1.1 * scale, 0.5 * scale);
    g.add(fl);
  }
  return g;
}

function buildSafe(def, fruits) {
  const builder = BUILDERS[def.body] || BUILDERS.bush;
  return builder(def, fruits);
}

// 杂草模型
export function createWeedModel() {
  const g = new THREE.Group();
  for (let i = 0; i < 4; i++) {
    const a = (i / 4) * Math.PI * 2 + Math.random();
    const blade = cone(0.09, 0.7 + Math.random() * 0.4, 0x3e8e41, 4);
    blade.position.set(Math.cos(a) * 0.2, 0.35, Math.sin(a) * 0.2);
    blade.rotation.set((Math.random() - 0.5) * 0.6, 0, (Math.random() - 0.5) * 0.6);
    g.add(blade);
  }
  return g;
}

// 害虫模型（在 farm3d 中做环绕动画）
export function createPestModel() {
  const g = new THREE.Group();
  for (let i = 0; i < 3; i++) {
    const bug = sphere(0.07, 0x2b2b2b, 4, 3);
    bug.userData.angle = (i / 3) * Math.PI * 2;
    g.add(bug);
  }
  return g;
}

// 待清理残株
export function createResidueModel() {
  const g = new THREE.Group();
  for (let i = 0; i < 3; i++) {
    const stick = cyl(0.04, 0.06, 0.7, SOIL_STEM, 4);
    stick.position.set((Math.random() - 0.5) * 0.5, 0.3, (Math.random() - 0.5) * 0.5);
    stick.rotation.set((Math.random() - 0.5) * 0.7, 0, (Math.random() - 0.5) * 0.7);
    g.add(stick);
  }
  return g;
}

// 看门狗模型
export function createDogModel(color) {
  const g = new THREE.Group();
  const c = color;
  const body = box(0.7, 0.4, 0.38, c); body.position.y = 0.42; g.add(body);
  const head = box(0.36, 0.34, 0.34, c); head.position.set(0.48, 0.72, 0); g.add(head);
  const snout = box(0.18, 0.14, 0.18, c); snout.position.set(0.72, 0.64, 0); g.add(snout);
  const nose = sphere(0.05, 0x222222, 4, 3); nose.position.set(0.82, 0.66, 0); g.add(nose);
  for (const s of [-1, 1]) {
    const ear = cone(0.09, 0.22, c, 4); ear.position.set(0.42, 0.95, 0.12 * s); g.add(ear);
    const legF = box(0.1, 0.3, 0.1, c); legF.position.set(0.26, 0.15, 0.13 * s); g.add(legF);
    const legB = box(0.1, 0.3, 0.1, c); legB.position.set(-0.26, 0.15, 0.13 * s); g.add(legB);
  }
  const tail = box(0.26, 0.08, 0.08, c); tail.position.set(-0.46, 0.56, 0); tail.rotation.z = 0.6; g.add(tail);
  g.userData.tail = tail;
  return g;
}
