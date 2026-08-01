// ============================================================
// 作物 3D 模型工厂：低多边形、flatShading，按生长阶段构建
// ============================================================
import * as THREE from 'three';

const matCache = new Map();

/** mat() 缓存材质标记：dispose 时必须跳过，避免污染其他场景/模型 */
const SHARED_MAT_FLAG = 'farmSharedMat';

export function mat(color, emissive = 0) {
  const key = color + '|' + emissive;
  if (!matCache.has(key)) {
    const m = new THREE.MeshStandardMaterial({
      color, flatShading: true, roughness: 0.85, metalness: 0,
      emissive, emissiveIntensity: emissive ? 0.5 : 0,
    });
    m.userData[SHARED_MAT_FLAG] = true;
    matCache.set(key, m);
  }
  return matCache.get(key);
}

/** 是否为 mat() 返回的共享材质（不可 dispose） */
export function isSharedMaterial(m) {
  return !!(m && m.userData && m.userData[SHARED_MAT_FLAG]);
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

// 舒展叶片：压扁椭球，自基部伸向局部 +Z 方向
function leaf(len, w, color) {
  const g = new THREE.Group();
  const blade = sphere(0.5, color, 7, 5);
  blade.scale.set(w, 0.16, len);
  blade.position.z = len * 0.45;
  blade.rotation.x = -0.05;
  g.add(blade);
  return g;
}

// 环列叶序：count 片叶绕中心向外伸展，tilt > 0 叶尖上扬
function rosette(count, len, w, color, { y = 0, tilt = 1.05, phase = 0 } = {}) {
  const g = new THREE.Group();
  for (let i = 0; i < count; i++) {
    const a = (i / count) * Math.PI * 2 + phase;
    const holder = new THREE.Group();
    holder.rotation.y = a;
    const lf = leaf(len * (0.9 + ((i * 7) % 3) * 0.08), w, color);
    lf.position.y = y;
    lf.rotation.x = -tilt + ((i * 5) % 3) * 0.07;
    holder.add(lf);
    g.add(holder);
  }
  return g;
}

// 五瓣小花：花瓣环绕 + 花心
function blossom(r, petalColor, centerColor = 0xffe08a) {
  const g = new THREE.Group();
  for (let i = 0; i < 5; i++) {
    const a = (i / 5) * Math.PI * 2;
    const p = sphere(r, petalColor, 5, 4);
    p.scale.set(1, 0.32, 1.45);
    p.position.set(Math.sin(a) * r * 1.15, 0, Math.cos(a) * r * 1.15);
    p.rotation.y = a;
    g.add(p);
  }
  const c = sphere(r * 0.55, centerColor, 5, 4);
  c.position.y = r * 0.3;
  g.add(c);
  return g;
}

// 星形萼片（番茄/茄子/草莓果顶）
function calyx(r, color) {
  const g = new THREE.Group();
  for (let i = 0; i < 5; i++) {
    const a = (i / 5) * Math.PI * 2;
    const c = cone(r * 0.4, r * 1.5, color, 4);
    c.position.set(Math.cos(a) * r * 0.45, 0, Math.sin(a) * r * 0.45);
    c.rotation.set(Math.sin(a) * 1.05, 0, -Math.cos(a) * 1.05);
    g.add(c);
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

// ---- 共享植物学零件：只组合 sphere/cone/cyl/leaf，返回 Group，不创建独占材质 ----

// 低面枝条：基部在原点、向局部 +Y 伸展的锥形枝段
function branchBetween(length, radius, color) {
  const g = new THREE.Group();
  const seg = cyl(radius * 0.62, radius, length, color, 5);
  seg.position.y = length / 2;
  g.add(seg);
  return g;
}

// 复叶组合：mode 'palmate' 为掌状辐射，否则沿中轴羽状互生，自带中轴
function compoundLeaf({ count = 4, len = 0.34, w = 0.09, color = 0x55a04a, mode = 'pinnate' } = {}) {
  const g = new THREE.Group();
  const rachis = cyl(0.011, 0.016, len, color, 4);
  rachis.rotation.x = Math.PI / 2;
  rachis.position.z = len / 2;
  g.add(rachis);
  if (mode === 'palmate') {
    for (let i = 0; i < count; i++) {
      const a = (i / count) * Math.PI * 2;
      const holder = new THREE.Group();
      holder.position.z = len;
      holder.rotation.y = a;
      const lf = leaf(len * 0.62, w, color);
      lf.rotation.x = -0.22;
      holder.add(lf);
      g.add(holder);
    }
    return g;
  }
  const pairs = Math.max(1, Math.floor((count - 1) / 2));
  for (let i = 0; i < pairs; i++) {
    const z = len * (0.3 + (0.5 * i) / pairs);
    for (const s of [-1, 1]) {
      const holder = new THREE.Group();
      holder.position.z = z;
      holder.rotation.y = s * (Math.PI / 2);
      const lf = leaf(len * 0.42, w, color);
      lf.rotation.x = -0.28;
      holder.add(lf);
      g.add(holder);
    }
  }
  const tip = leaf(len * 0.5, w, color);
  tip.position.z = len;
  tip.rotation.x = -0.2;
  g.add(tip);
  return g;
}

// 狭长披针叶：细长叶片 + 叶尖
function lanceLeaf(len, w, color) {
  const g = new THREE.Group();
  const blade = sphere(0.5, color, 6, 4);
  blade.scale.set(w, 0.09, len * 0.78);
  blade.position.z = len * 0.36;
  g.add(blade);
  const tip = cone(w * 0.4, len * 0.3, color, 4);
  tip.rotation.x = Math.PI / 2;
  tip.position.z = len * 0.85;
  g.add(tip);
  return g;
}

// 两点之间的细梗（果梗/果轴），from/to 为三维数组
function stemBetween(from, to, r, color) {
  const a = new THREE.Vector3(...from);
  const b = new THREE.Vector3(...to);
  const dir = b.clone().sub(a);
  const len = dir.length();
  const m = cyl(r, r, len, color, 4);
  m.position.copy(a).addScaledVector(dir, 0.5);
  m.quaternion.setFromUnitVectors(new THREE.Vector3(0, 1, 0), dir.normalize());
  return m;
}

// 肾形灵芝菌盖：压扁扇面 + 浅色生长边缘
function kidneyCap(width, depth, color, rimColor) {
  const g = new THREE.Group();
  const capM = sphere(0.5, color, 8, 5);
  capM.scale.set(width, width * 0.16, depth);
  g.add(capM);
  const rim = sphere(0.5, rimColor, 8, 4);
  rim.scale.set(width * 1.05, width * 0.07, depth * 1.05);
  rim.position.y = -width * 0.045;
  g.add(rim);
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

// 幼苗：按体型分化的破土苗
function sprout(def, scale, ctx = DEFAULT_CONTEXT) {
  const g = new THREE.Group();
  const body = def?.body;
  if (body === 'fungus') {  // 灵芝幼菇
    const stipe = cyl(0.05 * scale, 0.08 * scale, 0.24 * scale, 0xe8d8b9);
    stipe.position.y = 0.12 * scale; g.add(stipe);
    const capMini = sphere(0.13 * scale, 0x9c6644, 6, 4);
    capMini.scale.set(1.25, 0.45, 1);
    capMini.position.y = 0.27 * scale; g.add(capMini);
    return g;
  }
  if (body === 'cereal' || body === 'corn') {  // 禾苗：细叶丛生
    for (let i = 0; i < 3; i++) {
      const holder = new THREE.Group();
      holder.rotation.y = i * 2.1 + 0.4;
      const lf = leaf(0.52 * scale, 0.09 * scale, 0x84b95f);
      lf.position.y = 0.26 * scale;
      lf.rotation.x = -1.2 + i * 0.12;
      holder.add(lf); g.add(holder);
    }
    const stem = cyl(0.035, 0.05, 0.3 * scale, 0x7fb069);
    stem.position.y = 0.15 * scale; g.add(stem);
    return g;
  }
  if (body === 'tree' || body === 'money' || body === 'palm') {  // 木本幼苗：对生真叶 + 顶芽
    const stem = cyl(0.045, 0.065, 0.36 * scale, 0x8a6b4f);
    stem.position.y = 0.18 * scale; g.add(stem);
    for (const s of [-1, 1]) {
      const holder = new THREE.Group();
      holder.rotation.y = s > 0 ? 0.5 : Math.PI + 0.5;
      const lf = leaf(0.3 * scale, 0.16 * scale, 0x6fb25a);
      lf.position.y = 0.32 * scale;
      lf.rotation.x = -0.9;
      holder.add(lf); g.add(holder);
    }
    const bud = cone(0.05 * scale, 0.13 * scale, 0x9ccb6f, 4);
    bud.position.y = 0.44 * scale; g.add(bud);
    return g;
  }
  if (body === 'pineapple') {  // 菠萝幼苗：莲座针叶
    g.add(rosette(6, 0.3 * scale, 0.07 * scale, 0x6fbf63, { y: 0.08 * scale, tilt: 1.2 }));
    return g;
  }
  if (def?.id === 'wandou') {  // 豌豆幼苗：细茎 + 小复叶 + 顶端卷须（第二轮：藤本特征前置）
    const stem = cyl(0.03 * scale, 0.045 * scale, 0.34 * scale, 0x7fb069);
    stem.position.y = 0.17 * scale; g.add(stem);
    for (let i = 0; i < 2; i++) {
      const holder = new THREE.Group();
      holder.rotation.y = i * 2.2 + 0.4;
      const cl = compoundLeaf({ count: 3, len: 0.13 * scale, w: 0.045 * scale, color: def.leaf });
      cl.position.y = (0.2 + i * 0.12) * scale;
      cl.rotation.x = -0.45;
      holder.add(cl); g.add(holder);
    }
    const tendril = cone(0.012 * scale, 0.14 * scale, def.leaf, 3);
    tendril.position.set(0.04 * scale, 0.42 * scale, 0);
    tendril.rotation.set(0.5, 0, -0.9);
    g.add(tendril);
    return g;
  }
  if (body === 'vine') {  // 葡萄/葫芦幼苗：斜出小蔓 + 幼叶 + 卷须
    const vine = cyl(0.03 * scale, 0.045 * scale, 0.4 * scale, def.leaf, 5);
    vine.position.set(0.06 * scale, 0.2 * scale, 0);
    vine.rotation.z = -0.28;
    g.add(vine);
    for (let i = 0; i < 3; i++) {
      const holder = new THREE.Group();
      holder.position.set((0.02 + i * 0.06) * scale, (0.16 + i * 0.11) * scale, 0);
      holder.rotation.y = i * 2.1 + 0.6;
      const lf = leaf(0.2 * scale, 0.14 * scale, def.leaf);
      lf.rotation.x = -0.55;
      holder.add(lf); g.add(holder);
    }
    const tendril = cone(0.012 * scale, 0.13 * scale, def.leaf, 3);
    tendril.position.set(0.2 * scale, 0.44 * scale, 0.02 * scale);
    tendril.rotation.set(0.4, 0, -1.0);
    g.add(tendril);
    return g;
  }
  if (body === 'ground') {  // 南瓜/西瓜幼苗：两条贴地小蔓 + 对生叶
    for (const s of [-1, 1]) {
      const a = s > 0 ? 0.3 : Math.PI + 0.3;
      const vine = cyl(0.028 * scale, 0.04 * scale, 0.32 * scale, def.leaf, 5);
      vine.position.set(Math.cos(a) * 0.16 * scale, 0.04 * scale, Math.sin(a) * 0.16 * scale);
      vine.rotation.set(Math.sin(a) * 1.45, 0, -Math.cos(a) * 1.45);
      g.add(vine);
      const holder = new THREE.Group();
      holder.position.set(Math.cos(a) * 0.28 * scale, 0.06 * scale, Math.sin(a) * 0.28 * scale);
      holder.rotation.y = a + 0.5;
      const lf = leaf(0.2 * scale, 0.15 * scale, def.leaf);
      lf.rotation.x = -0.4;
      holder.add(lf); g.add(holder);
      const tendril = cone(0.01 * scale, 0.1 * scale, def.leaf, 3);
      tendril.position.set(Math.cos(a) * 0.36 * scale, 0.08 * scale, Math.sin(a) * 0.36 * scale);
      tendril.rotation.set(0.8, 0, 0.5);
      g.add(tendril);
    }
    return g;
  }
  if (def?.id === 'renshen') {  // 人参幼苗：细茎 + 三枚小复叶
    const stem = cyl(0.03, 0.045, 0.4 * scale, 0x6a994e);
    stem.position.y = 0.2 * scale; g.add(stem);
    for (let i = 0; i < 3; i++) {
      const holder = new THREE.Group();
      holder.rotation.y = i * 2.1 + 0.5;
      const cl = compoundLeaf({ count: 3, len: 0.16 * scale, w: 0.05 * scale, color: 0x6a994e });
      cl.position.y = 0.38 * scale;
      cl.rotation.x = -0.5;
      holder.add(cl); g.add(holder);
    }
    return g;
  }
  // 默认（草本/根茎/灌木/藤蔓）：细茎 + 两片舒展子叶
  const stem = cyl(0.045, 0.065, 0.32 * scale, 0x7fb069);
  stem.position.y = 0.16 * scale; g.add(stem);
  for (const s of [-1, 1]) {
    const holder = new THREE.Group();
    holder.rotation.y = s > 0 ? 0 : Math.PI;
    const lf = leaf(0.34 * scale, 0.2 * scale, 0x66bb6a);
    lf.position.y = 0.28 * scale;
    lf.rotation.x = -0.55;
    holder.add(lf); g.add(holder);
  }
  return g;
}

// 开花期花色（按作物真实花色配置）
const FLOWER_COLOR = {
  bailuobo: 0xffffff, huluobo: 0xf6f0ff, renshen: 0xe8ecd8,
  dabaicai: 0xffe066, nangua: 0xffd23f, xigua: 0xffe08a,
  tudou: 0xf6e6ff, qiezi: 0xc39bd3, fanqie: 0xffd94d, wandou: 0xe8b7d4, lajiao: 0xfffaf0,
  putao: 0xd8e8b0, hulu: 0xfff3c4,
  hongzao: 0xf5e6c8, pingguo: 0xffe3ec, taozi: 0xffc2d4, chengzi: 0xfff2e0, shiliu: 0xff6b4a, youzi: 0xfff2e0,
  hongmeigui: 0xe5386d, caomei: 0xffffff, boluo: 0xb565d8,
};

// 开花期花序（阶段 3）：按体型定制，花朵锚定在枝条/叶腋而非空中
function flowerShow(def, scale, ctx = DEFAULT_CONTEXT) {
  const g = new THREE.Group();
  const color = FLOWER_COLOR[def.id] || 0xfff7f0;
  switch (def.body) {
    case 'tree': {  // 树冠表面点缀五瓣花
      for (let i = 0; i < 8; i++) {
        const a = (i / 8) * Math.PI * 2 + 0.3;
        const b = blossom(0.1 * scale, color);
        b.position.set(Math.cos(a) * 0.78 * scale, (1.75 + (i % 3) * 0.25) * scale, Math.sin(a) * 0.78 * scale);
        b.rotation.x = -0.4;
        g.add(b);
      }
      return g;
    }
    case 'bush': {  // 灌木花：专用作物锚定果位，其余表面星形小花
      if (def.id === 'qiezi') {  // 紫色星形花锚定果位
        for (const [x, y, z] of EGGPLANT_SPOTS) {
          const b = blossom(0.1 * scale, color);
          b.position.set(x * scale, (y + 0.3) * scale, z * scale);
          b.rotation.x = -0.4;
          g.add(b);
        }
        return g;
      }
      if (def.id === 'fanqie') {  // 黄色花簇锚定果簇位
        for (const [x, y, z] of TOMATO_CLUSTERS) {
          for (const [ox, oy, oz] of [[0, 0.06, 0], [0.1, -0.03, 0.05], [-0.08, -0.08, -0.04]]) {
            const b = blossom(0.07 * scale, color);
            b.position.set((x + ox) * scale, (y + oy) * scale, (z + oz) * scale);
            g.add(b);
          }
        }
        return g;
      }
      if (def.id === 'wandou') {  // 淡紫蝶形小花沿藤
        for (const [x, y, z] of PEA_NODES) {
          const b = blossom(0.075 * scale, color);
          b.position.set(x * scale, y * scale, z * scale);
          g.add(b);
        }
        return g;
      }
      if (def.id === 'lajiao') {  // 白色小花于叶腋果位
        for (const [x, y, z] of PEPPER_SPOTS) {
          const b = blossom(0.06 * scale, color);
          b.position.set(x * scale, (y + 0.14) * scale, z * scale);
          g.add(b);
        }
        return g;
      }
      for (let i = 0; i < 6; i++) {
        const a = (i / 6) * Math.PI * 2 + 0.6;
        const b = blossom(0.085 * scale, color);
        b.position.set(Math.cos(a) * 0.4 * scale, (0.5 + (i % 2) * 0.28) * scale, Math.sin(a) * 0.4 * scale);
        b.rotation.set(-0.5, a, 0);
        g.add(b);
      }
      return g;
    }
    case 'root':
    case 'cabbage': {  // 抽薹开花：中央花薹 + 顶端小花
      const tall = def.body === 'root' ? 0.9 : 0.7;
      const stalk = cyl(0.03 * scale, 0.045 * scale, tall * scale, 0x7fb069);
      stalk.position.y = (def.body === 'root' ? 0.85 : 0.95) * scale; g.add(stalk);
      const topY = (def.body === 'root' ? 1.2 : 1.25) * scale;
      for (let i = 0; i < 5; i++) {
        const a = (i / 5) * Math.PI * 2;
        const b = blossom(0.065 * scale, color);
        b.position.set(Math.cos(a) * 0.13 * scale, topY + (i % 2) * 0.11 * scale, Math.sin(a) * 0.13 * scale);
        g.add(b);
      }
      return g;
    }
    case 'vine': {  // 棚架下垂花序
      for (const x of [-0.5, -0.1, 0.35, 0.6]) {
        const b = blossom(0.075 * scale, color, 0xd8e8b0);
        b.position.set(x * scale, 1.5 * scale, 0.12 * scale);
        b.rotation.x = Math.PI;
        g.add(b);
      }
      return g;
    }
    case 'ground': {  // 蔓上喇叭状黄花
      for (let i = 0; i < 4; i++) {
        const a = (i / 4) * Math.PI * 2 + 0.7;
        const f = cone(0.09 * scale, 0.17 * scale, color, 6);
        f.position.set(Math.cos(a) * 0.75 * scale, 0.18 * scale, Math.sin(a) * 0.75 * scale);
        f.rotation.set(Math.sin(a) * 0.5, 0, -Math.cos(a) * 0.5 + 0.3);
        g.add(f);
      }
      return g;
    }
    case 'rose': {  // 玫瑰花苞：锚定枝端，大小不一，不提前显示成熟花
      const tips = roseBranchTips();
      const budSizes = [0.075, 0.09, 0.105];
      tips.forEach(([x, y, z], i) => {
        const bud = new THREE.Group();
        bud.name = 'rose-bud';
        bud.position.set(x * scale, (y + 0.05) * scale, z * scale);
        const b = ico(budSizes[i] * scale, color, 1);
        b.scale.y = 1.35;
        bud.add(b);
        const sepal = cone(budSizes[i] * 0.6 * scale, budSizes[i] * 1.1 * scale, def.leaf, 5);
        sepal.rotation.x = Math.PI;
        sepal.position.y = -budSizes[i] * 0.8 * scale;
        bud.add(sepal);
        g.add(bud);
      });
      return g;
    }
    case 'low': {  // 草莓白花（黄心）
      for (let i = 0; i < 5; i++) {
        const a = (i / 5) * Math.PI * 2 + 0.25;
        const b = blossom(0.075 * scale, color);
        b.position.set(Math.cos(a) * 0.32 * scale, 0.24 * scale, Math.sin(a) * 0.32 * scale);
        g.add(b);
      }
      return g;
    }
    case 'pineapple': {  // 菠萝中心紫色小花带（莲座中心，果身尚未形成）
      for (let i = 0; i < 6; i++) {
        const a = (i / 6) * Math.PI * 2;
        const b = blossom(0.055 * scale, color);
        b.position.set(Math.cos(a) * 0.1 * scale, 0.16 * scale, Math.sin(a) * 0.1 * scale);
        g.add(b);
      }
      return g;
    }
    case 'fungus':
      return null;  // 菌类不显示花朵：灵芝开花阶段的视觉变化完全由菌盖展开表达
    default:
      return null;  // 禾本科/玉米/棕榈/摇钱树：无明显花期特征，保持株型
  }
}

// ---- 灌木与玫瑰专用构建：按作物独立组装，不共享多面体叶球构图 ----

// 茄子果位（开花阶段紫花锚定点共用）
const EGGPLANT_SPOTS = [[0.32, 0.5, 0.2], [-0.3, 0.58, -0.18], [0.04, 0.44, -0.32]];

function buildEggplant(def, fruits, ctx) {  // 茄子：紫绿高茎三分叉，宽叶下垂，梨形紫果悬于叶下
  const { young } = ctx;
  const g = new THREE.Group();
  g.name = 'eggplant-plant';
  const STEM = 0x6e5a9e;
  const stem = cyl(0.045, 0.07, 1.05, STEM, 6);
  stem.position.y = 0.525;
  g.add(stem);
  const forks = young
    ? [[0.5, 0.4, 0.8, 0.3], [0.68, 2.5, 0.85, 0.28]]
    : [[0.55, 0.4, 0.8, 0.36], [0.75, 2.5, 0.85, 0.34], [0.9, 4.6, 0.75, 0.3]];
  for (const [hy, az, tilt, len] of forks) {
    const fork = new THREE.Group();
    fork.position.y = hy;
    fork.rotation.y = az;
    const br = branchBetween(len, 0.032, STEM);
    br.rotation.x = tilt;
    fork.add(br);
    const lf = leaf(young ? 0.42 : 0.52, 0.4, def.leaf);  // 宽卵形下垂叶
    lf.name = 'eggplant-leaf';
    lf.position.set(0, len * Math.cos(tilt), len * Math.sin(tilt));
    lf.rotation.x = 0.5;
    fork.add(lf);
    g.add(fork);
  }
  if (!young) {
    for (const [x, y, z] of EGGPLANT_SPOTS) {
      const f = new THREE.Group();
      f.name = 'eggplant-fruit';
      f.position.set(x, y, z);
      const hang = cyl(0.014, 0.02, 0.22, 0x6fae54, 4);
      hang.position.y = 0.32;
      f.add(hang);
      const cx = calyx(0.13, 0x6fae54);  // 星形大萼
      cx.position.y = 0.2;
      f.add(cx);
      const neck = sphere(0.12, def.fruit, 7, 6);
      neck.position.y = 0.06;
      f.add(neck);
      const bodyMain = sphere(0.19, def.fruit, 7, 6);  // 梨形果身
      bodyMain.scale.y = 1.25;
      bodyMain.position.y = -0.12;
      f.add(bodyMain);
      fruits.add(f);
    }
  }
  return g;
}

// 番茄果簇位（开花阶段黄花锚定点共用）
const TOMATO_CLUSTERS = [[0.34, 0.66, 0.4], [-0.44, 0.7, -0.26]];

function buildTomato(def, fruits, ctx) {  // 番茄：多级斜枝 + 复叶，3-4 颗果为一簇沿斜梗分布
  const { young } = ctx;
  const g = new THREE.Group();
  g.name = 'tomato-plant';
  const STEM = 0x55803f;
  const stem = cyl(0.05, 0.075, 0.55, STEM, 6);
  stem.position.y = 0.275;
  g.add(stem);
  const branches = young
    ? [[0.38, 0.3, 1.0, 0.45], [0.5, 2.2, 1.05, 0.42], [0.6, 4.3, 0.95, 0.4]]
    : [[0.4, 0.3, 1.0, 0.6], [0.52, 2.1, 1.05, 0.55], [0.62, 4.2, 0.95, 0.58], [0.7, 5.4, 1.1, 0.5]];
  for (const [hy, az, tilt, len] of branches) {
    const fork = new THREE.Group();
    fork.position.y = hy;
    fork.rotation.y = az;
    const br = branchBetween(len, 0.03, STEM);
    br.rotation.x = tilt;
    fork.add(br);
    for (const t of [0.55, 1]) {  // 复叶轮廓：枝中部与枝端
      const cl = compoundLeaf({ count: young ? 3 : 5, len: 0.3, w: 0.08, color: def.leaf });
      cl.position.set(0, len * t * Math.cos(tilt), len * t * Math.sin(tilt));
      cl.rotation.x = tilt * 0.5 + 0.25;
      fork.add(cl);
    }
    g.add(fork);
  }
  if (!young) {
    for (const [cx, cy, cz] of TOMATO_CLUSTERS) {
      const cluster = new THREE.Group();
      cluster.name = 'tomato-fruit-cluster';
      cluster.position.set(cx, cy, cz);
      cluster.add(stemBetween([0, 0.12, 0], [0.1, -0.26, 0.06], 0.014, 0x6fae54));  // 斜果梗
      const spots = [[0, -0.02, 0], [0.09, -0.1, 0.05], [-0.04, -0.18, -0.03], [0.08, -0.26, 0.02]];
      for (const [fx, fy, fz] of spots) {
        const t = sphere(0.14, def.fruit, 7, 6);
        t.scale.y = 0.92;
        t.position.set(fx, fy, fz);
        cluster.add(t);
        const cx2 = calyx(0.08, 0x6fae54);
        cx2.position.set(fx, fy + 0.12, fz);
        cluster.add(cx2);
      }
      fruits.add(cluster);
    }
  }
  return g;
}

// 豌豆荚节点（开花阶段小花锚定点共用）
const PEA_NODES = [[0.16, 0.85, 0.06], [0.2, 1.12, -0.05], [-0.14, 0.95, 0.14]];

function buildPea(def, fruits, ctx) {  // 豌豆：单根支架 + 两条攀援藤，卷须复叶，豆荚成对下垂
  const { young } = ctx;
  const g = new THREE.Group();
  g.name = 'pea-vines';
  const stake = cyl(0.032, 0.045, 1.7, TRUNK, 5);
  stake.position.y = 0.85;
  g.add(stake);
  const vines = [
    { base: [0.07, 0, 0.02], segs: [[0.5, 0.18, 0.6], [2.6, 0.3, 0.55], [4.7, 0.35, 0.5]] },
    { base: [-0.08, 0, -0.04], segs: [[3.4, 0.2, 0.58], [5.6, 0.32, 0.52], [1.8, 0.3, 0.45]] },
  ];
  for (const { base, segs } of vines) {
    let p = new THREE.Vector3(...base);
    const segScale = young ? 0.62 : 1;  // 同一藤蔓路径，幼年期仅缩短段长（第二轮：构件数量不跳变）
    for (const [az, tilt, len] of segs) {
      const dir = new THREE.Vector3(Math.sin(az) * Math.sin(tilt), Math.cos(tilt), Math.cos(az) * Math.sin(tilt));
      const q = p.clone().addScaledVector(dir, len * segScale);
      g.add(stemBetween(p.toArray(), q.toArray(), 0.022, def.leaf));
      p = q;
    }
  }
  const leafNodes = 5;  // 交替复叶与卷须：小叶/大叶同构，仅尺寸与间距不同
  const leafLen = young ? 0.16 : 0.24;
  const nodeY = (i) => (young ? 0.3 + i * 0.19 : 0.4 + i * 0.26);
  for (let i = 0; i < leafNodes; i++) {
    const a = i * 2.3 + 0.5;
    const cl = compoundLeaf({ count: 3, len: leafLen, w: 0.08, color: def.leaf });
    cl.position.set(Math.sin(a) * 0.16, nodeY(i), Math.cos(a) * 0.16);
    cl.rotation.y = a;
    cl.rotation.x = -0.3;
    g.add(cl);
    if (i % 2 === 1) {
      const t1 = cone(0.012, 0.16, def.leaf, 3);
      t1.position.set(Math.sin(a + 1) * 0.18, nodeY(i) + 0.06, Math.cos(a + 1) * 0.18);
      t1.rotation.set(0.9, 0, 0.6);
      g.add(t1);
      const t2 = cone(0.01, 0.1, def.leaf, 3);
      t2.position.set(Math.sin(a + 1) * 0.2 + 0.04, nodeY(i) + 0.12, Math.cos(a + 1) * 0.2);
      t2.rotation.set(1.2, 0, -0.4);
      g.add(t2);
    }
  }
  if (!young) {
    for (const [nx, ny, nz] of PEA_NODES) {
      for (const s of [-1, 1]) {  // 成对下垂豆荚
        const pod = new THREE.Group();
        pod.name = 'pea-pod';
        pod.position.set(nx + s * 0.07, ny - 0.16, nz + s * 0.03);
        pod.rotation.z = s * 0.12;
        const hang = cyl(0.01, 0.014, 0.1, 0x6fae54, 4);
        hang.position.y = 0.14;
        pod.add(hang);
        const body = sphere(0.11, def.fruit, 6, 5);
        body.scale.set(0.62, 1.5, 0.45);
        pod.add(body);
        for (let k = 0; k < 3; k++) {  // 豆粒凸起贴着荚体
          const pea = sphere(0.042, 0x9ede7f, 4, 3);
          pea.position.set(0.05, -0.1 + k * 0.1, 0);
          pod.add(pea);
        }
        fruits.add(pod);
      }
    }
  }
  return g;
}

// 辣椒果位（x/y/z + 弯曲方位，开花阶段白花锚定点共用）
const PEPPER_SPOTS = [
  [0.18, 0.46, 0.14, 0.4], [-0.2, 0.6, 0.12, 2.2], [0.14, 0.72, -0.18, 4.0],
  [-0.16, 0.84, -0.1, 5.4], [0.22, 0.62, -0.02, 1.4],
];

function buildPepper(def, fruits, ctx) {  // 辣椒：直立细株短枝，狭长披针叶，红椒分段弯曲下垂
  const { young } = ctx;
  const g = new THREE.Group();
  g.name = 'pepper-plant';
  const STEM = 0x4f7d3a;
  const stemH = young ? 0.65 : 1.0;
  const stem = cyl(0.04, 0.06, stemH, STEM, 6);
  stem.position.y = stemH / 2;
  g.add(stem);
  const branches = young
    ? [[0.35, 0.6, 0.9, 0.16], [0.5, 2.8, 0.95, 0.15]]
    : [[0.42, 0.6, 0.9, 0.22], [0.56, 2.5, 0.95, 0.2], [0.7, 4.3, 0.85, 0.2], [0.82, 1.4, 1.0, 0.18], [0.9, 3.6, 0.8, 0.16]];
  for (const [hy, az, tilt, len] of branches) {
    const fork = new THREE.Group();
    fork.position.y = hy;
    fork.rotation.y = az;
    const br = branchBetween(len, 0.024, STEM);
    br.rotation.x = tilt;
    fork.add(br);
    g.add(fork);
  }
  const leafRows = young
    ? [[0.3, 0.2], [0.48, 2.4], [0.6, 4.6]]
    : [[0.3, 0.2], [0.46, 2.3], [0.62, 4.5], [0.76, 1.2], [0.88, 3.4]];
  for (const [hy, az] of leafRows) {
    const holder = new THREE.Group();
    holder.position.y = hy;
    holder.rotation.y = az;
    const lf = lanceLeaf(0.4, 0.09, def.leaf);
    lf.rotation.x = -0.35;
    holder.add(lf);
    g.add(holder);
  }
  if (!young) {
    for (const [x, y, z, az] of PEPPER_SPOTS) {
      const f = new THREE.Group();
      f.name = 'pepper-fruit';
      f.position.set(x, y, z);
      const hang = cyl(0.012, 0.016, 0.1, 0x5aa54a, 4);  // 短果柄
      hang.position.y = 0.06;
      f.add(hang);
      const cx = cone(0.05, 0.07, 0x5aa54a, 5);  // 绿色萼片
      cx.rotation.x = Math.PI;
      f.add(cx);
      const s1 = cone(0.08, 0.2, def.fruit, 6);  // 两段几何形成自然弯曲
      s1.rotation.set(Math.PI - 0.25 * Math.sin(az), 0, 0.25 * Math.cos(az));
      s1.position.y = -0.11;
      f.add(s1);
      const s2 = cone(0.045, 0.13, def.fruit, 5);
      s2.rotation.set(Math.PI - 0.55 * Math.sin(az), 0, 0.55 * Math.cos(az));
      s2.position.set(0.06 * Math.cos(az), -0.24, 0.06 * Math.sin(az));
      f.add(s2);
      fruits.add(f);
    }
  }
  return g;
}

// 红玫瑰枝条（方位角/倾角/长度），枝端位置供花苞与花朵共用
const ROSE_BRANCHES = [[0.4, 0.22, 0.95], [2.5, 0.3, 1.15], [4.6, 0.18, 1.3]];
const ROSE_WOOD = 0x7a5a44;

function roseBranchTips(factor = 1) {
  return ROSE_BRANCHES.map(([az, tilt, len]) => {
    const L = len * factor;
    return [Math.sin(az) * Math.sin(tilt) * L, Math.cos(tilt) * L, Math.cos(az) * Math.sin(tilt) * L];
  });
}

// 玫瑰花朵：内外两层花瓣 + 萼片
function roseBloom(def, r) {
  const g = new THREE.Group();
  for (let i = 0; i < 6; i++) {  // 外层花瓣
    const a = (i / 6) * Math.PI * 2;
    const p = sphere(r * 0.62, def.fruit, 5, 4);
    p.scale.set(1, 0.38, 1.4);
    p.position.set(Math.sin(a) * r * 0.8, 0, Math.cos(a) * r * 0.8);
    p.rotation.y = a;
    g.add(p);
  }
  for (let i = 0; i < 4; i++) {  // 内层花瓣
    const a = (i / 4) * Math.PI * 2 + 0.4;
    const p = sphere(r * 0.5, 0xc2255c, 5, 4);
    p.scale.set(1, 0.55, 1.1);
    p.position.set(Math.sin(a) * r * 0.4, r * 0.28, Math.cos(a) * r * 0.4);
    p.rotation.y = a;
    g.add(p);
  }
  const heart = ico(r * 0.42, 0xc2255c, 1);
  heart.position.y = r * 0.42;
  g.add(heart);
  const sepal = cone(r * 0.55, r * 0.9, def.leaf, 5);
  sepal.rotation.x = Math.PI;
  sepal.position.y = -r * 0.5;
  g.add(sepal);
  return g;
}

function buildRose(def, fruits, ctx) {  // 红玫瑰：3 根错落木质枝 + 复叶 + 三角刺，1 主花 2 侧花
  const { young } = ctx;
  const g = new THREE.Group();
  g.name = 'rose-branches';
  const factor = young ? 0.72 : 1;
  ROSE_BRANCHES.forEach(([az, tilt, len], bi) => {
    const L = len * factor;
    const br = new THREE.Group();
    br.rotation.y = az;
    const stem = branchBetween(L, 0.032, ROSE_WOOD);
    stem.rotation.x = tilt;
    br.add(stem);
    const leafT = young ? [0.6] : [0.45, 0.78];  // 每枝复叶 2 组
    for (const t of leafT) {
      const cl = compoundLeaf({ count: 5, len: 0.26, w: 0.08, color: def.leaf });
      cl.position.set(0, L * t * Math.cos(tilt), L * t * Math.sin(tilt));
      cl.rotation.x = tilt + 0.3;
      br.add(cl);
    }
    if (!young && bi !== 1) {  // 少量三角形刺
      for (const t of [0.3, 0.62]) {
        const thorn = cone(0.02, 0.055, 0x5d4037, 3);
        thorn.position.set(0.03, L * t * Math.cos(tilt), L * t * Math.sin(tilt));
        thorn.rotation.z = -1.2;
        br.add(thorn);
      }
    }
    g.add(br);
  });
  if (!young) {
    const tips = roseBranchTips();
    tips.forEach(([x, y, z], i) => {
      const bloom = roseBloom(def, i === 2 ? 0.16 : 0.11);  // 最高枝为主花
      bloom.name = 'rose-bloom';
      bloom.position.set(x, y + 0.05, z);
      fruits.add(bloom);
    });
  }
  return g;
}

// ---- 果树专用构建：苹果/红枣/橙子/柚子分别定义树冠组合与果实锚点 ----

function buildApple(def, fruits, ctx) {  // 苹果：低粗干 + 横向粗枝 + 宽圆重叠树冠，大果带凹窝短梗
  const { young } = ctx;
  const g = new THREE.Group();
  const trunk = cyl(0.13, 0.19, 0.95, TRUNK, 6);
  trunk.position.y = 0.475;
  g.add(trunk);
  const canopy = new THREE.Group();
  canopy.name = 'apple-canopy';
  if (young) {
    const c1 = ico(0.5, def.leaf);
    c1.scale.y = 0.8;
    c1.position.y = 1.2;
    canopy.add(c1);
  } else {
    for (const [az, tilt, len] of [[0.4, 1.1, 0.55], [2.5, 1.15, 0.52], [4.6, 1.05, 0.5]]) {
      const fork = new THREE.Group();
      fork.position.y = 0.82;
      fork.rotation.y = az;
      const br = branchBetween(len, 0.055, TRUNK);
      br.rotation.x = tilt;
      fork.add(br);
      g.add(fork);
    }
    for (const [x, y, z, r] of [[0, 1.42, 0, 0.6], [0.55, 1.3, 0.28, 0.48], [-0.5, 1.28, -0.3, 0.46], [0, 1.26, -0.52, 0.42]]) {
      const b = ico(r, def.leaf);
      b.scale.y = 0.78;
      b.position.set(x, y, z);
      canopy.add(b);
    }
  }
  g.add(canopy);
  if (!young) {
    const spots = [
      [0.62, 1.15, 0.35], [-0.58, 1.1, 0.4], [0.15, 1.2, -0.62], [-0.35, 1.05, -0.5],
      [0.45, 1.25, -0.3], [-0.1, 1.08, 0.66], [0.7, 1.2, -0.05],
    ];
    for (const [x, y, z] of spots) {
      const f = new THREE.Group();
      f.name = 'apple-fruit';
      f.position.set(x, y, z);
      const hang = cyl(0.014, 0.02, 0.12, 0x6d4c41, 4);  // 短梗
      hang.position.y = 0.2;
      f.add(hang);
      const dimple = sphere(0.05, 0xa82636, 5, 4);  // 顶部凹窝
      dimple.scale.y = 0.45;
      dimple.position.y = 0.15;
      f.add(dimple);
      const body = sphere(0.16, def.fruit, 7, 6);
      body.scale.y = 0.9;
      f.add(body);
      fruits.add(f);
    }
  }
  return g;
}

function buildJujube(def, fruits, ctx) {  // 红枣：高细干 + 疏朗分层小冠 + 细枝可见，小椭圆果密生
  const { young } = ctx;
  const g = new THREE.Group();
  const trunk = cyl(0.07, 0.11, 1.55, TRUNK, 6);
  trunk.position.y = 0.775;
  g.add(trunk);
  const canopy = new THREE.Group();
  canopy.name = 'jujube-canopy';
  const layers = young
    ? [[1.15, 0.34, 0.3], [1.5, 0.3, 2.2]]
    : [[1.1, 0.38, 0.3], [1.4, 0.34, 1.6], [1.68, 0.3, 0.9], [1.95, 0.26, 2.2]];
  for (const [hy, r, az] of layers) {
    for (const s of [0, Math.PI]) {  // 每层两根可见细枝
      const fork = new THREE.Group();
      fork.position.y = hy - 0.05;
      fork.rotation.y = az + s;
      const br = branchBetween(0.38, 0.018, TRUNK);
      br.rotation.x = 0.95;
      fork.add(br);
      g.add(fork);
    }
    const b = ico(r, def.leaf);
    b.scale.y = 0.85;
    b.position.set(Math.sin(az) * 0.18, hy + 0.1, Math.cos(az) * 0.18);
    canopy.add(b);
  }
  g.add(canopy);
  if (!young) {
    const spots = [
      [0.3, 1.05, 0.15], [-0.25, 1.18, -0.2], [0.1, 1.0, -0.3],
      [0.35, 1.38, -0.1], [-0.3, 1.45, 0.18], [0.05, 1.32, 0.35],
      [0.28, 1.66, 0.2], [-0.22, 1.75, -0.15], [0, 1.6, -0.3],
      [0.2, 1.95, 0.1], [-0.15, 2.0, -0.12],
    ];
    for (const [x, y, z] of spots) {
      const f = new THREE.Group();
      f.name = 'jujube-fruit';
      f.position.set(x, y, z);
      const hang = cyl(0.008, 0.012, 0.08, 0x6d4c41, 4);
      hang.position.y = 0.12;
      f.add(hang);
      const body = sphere(0.085, def.fruit, 6, 5);  // 竖长椭圆小果
      body.scale.set(0.8, 1.25, 0.8);
      f.add(body);
      fruits.add(f);
    }
  }
  return g;
}

function buildOrange(def, fruits, ctx) {  // 橙子：紧凑近球形树冠 + 较深密叶块 + 小橙果
  const { young } = ctx;
  const g = new THREE.Group();
  const trunk = cyl(0.1, 0.15, 1.25, TRUNK, 6);
  trunk.position.y = 0.625;
  g.add(trunk);
  const canopy = new THREE.Group();
  canopy.name = 'orange-canopy';
  const blobs = young
    ? [[0, 1.35, 0, 0.52]]
    : [[0, 1.72, 0, 0.68], [0.42, 1.5, 0.15, 0.5], [-0.4, 1.52, -0.2, 0.48], [0.02, 1.5, 0.4, 0.44]];
  for (const [x, y, z, r] of blobs) {
    const b = ico(r, 0x3d7a33);  // 较深绿叶块
    b.scale.y = 0.9;
    b.position.set(x, y, z);
    canopy.add(b);
  }
  g.add(canopy);
  if (!young) {
    const spots = [
      [0.5, 1.5, 0.3], [-0.48, 1.55, 0.28], [0.2, 1.4, -0.45], [-0.3, 1.35, -0.4],
      [0.55, 1.75, -0.15], [-0.55, 1.8, -0.1], [0.1, 1.95, 0.45], [-0.15, 1.32, 0.5],
    ];
    for (const [x, y, z] of spots) {
      const f = new THREE.Group();
      f.name = 'orange-fruit';
      f.position.set(x, y, z);
      const hang = cyl(0.012, 0.016, 0.1, 0x6d4c41, 4);
      hang.position.y = 0.14;
      f.add(hang);
      const navel = sphere(0.04, 0x5aa54a, 4, 3);  // 果顶绿蒂
      navel.position.y = 0.13;
      f.add(navel);
      const body = sphere(0.14, def.fruit, 7, 6);
      f.add(body);
      fruits.add(f);
    }
  }
  return g;
}

const POMELO_FRUIT = new THREE.Color(0xf4d35e).lerp(new THREE.Color(0x9ab54e), 0.4).getHex();  // 浅黄绿

function buildPomelo(def, fruits, ctx) {  // 柚子：高干开放主枝 + 较长叶片，梨形大果长梗下垂
  const { young } = ctx;
  const g = new THREE.Group();
  const trunk = cyl(0.09, 0.14, 1.7, TRUNK, 6);
  trunk.position.y = 0.85;
  g.add(trunk);
  const canopy = new THREE.Group();
  canopy.name = 'pomelo-canopy';
  const branches = young
    ? [[0.3, 0.85, 0.5], [2.4, 0.8, 0.48]]
    : [[0.3, 0.85, 0.6], [2.4, 0.8, 0.58], [4.5, 0.9, 0.56], [1.2, 0.3, 0.55]];
  for (const [az, tilt, len] of branches) {
    const fork = new THREE.Group();
    fork.position.y = 1.55;
    fork.rotation.y = az;
    const br = branchBetween(len, 0.045, TRUNK);
    br.rotation.x = tilt;
    fork.add(br);
    g.add(fork);
    const bx = Math.sin(az) * Math.sin(tilt) * len;
    const by = 1.55 + Math.cos(tilt) * len;
    const bz = Math.cos(az) * Math.sin(tilt) * len;
    const blob = ico(young ? 0.34 : 0.36, def.leaf);  // 主枝间留有空隙
    blob.position.set(bx, by + 0.08, bz);
    canopy.add(blob);
    if (!young) {
      const lf = leaf(0.55, 0.18, 0x5da352);  // 较长叶片
      lf.position.set(bx * 0.6, by - 0.15, bz * 0.6);
      lf.rotation.y = az;
      lf.rotation.x = -0.25;
      canopy.add(lf);
    }
  }
  g.add(canopy);
  if (!young) {
    const spots = [[0.32, 1.5, 0.38], [0.4, 1.45, -0.3], [-0.38, 1.55, -0.05], [-0.05, 1.4, 0.18]];
    for (const [x, y, z] of spots) {
      const f = new THREE.Group();
      f.name = 'pomelo-fruit';
      f.position.set(x, y, z);
      f.add(stemBetween([0, 0.56, 0], [0, 0.26, 0], 0.015, 0x6d4c41));  // 长果梗
      const body = sphere(0.23, POMELO_FRUIT, 7, 6);  // 梨形大果
      body.scale.set(0.95, 1.18, 0.95);
      f.add(body);
      const neck = sphere(0.14, POMELO_FRUIT, 6, 5);
      neck.position.y = 0.22;
      f.add(neck);
      fruits.add(f);
    }
  }
  return g;
}

function buildBanana(def, fruits, ctx) {  // 香蕉：短粗假茎 + 6 片宽大蕉叶扇冠 + 冠心下垂多层蕉串
  const { young } = ctx;
  const g = new THREE.Group();
  const stemH = young ? 1.0 : 1.45;
  const pseudo = cyl(0.15, 0.21, stemH, 0x86b364, 7);  // 短粗假茎
  pseudo.position.y = stemH / 2;
  g.add(pseudo);
  const sheath = cyl(0.19, 0.24, 0.5, 0x7aa457, 7);
  sheath.position.y = 0.25;
  g.add(sheath);
  const canopy = new THREE.Group();
  canopy.name = 'banana-canopy';
  const leafTilts = young ? [-0.55, -0.3, -0.1, -0.42] : [-0.55, -0.32, -0.12, 0.08, -0.42, 0.22];
  leafTilts.forEach((tilt, i) => {
    const a = (i / leafTilts.length) * Math.PI * 2 + 0.25;
    const holder = new THREE.Group();
    holder.position.y = stemH + 0.06;
    holder.rotation.y = a;
    const lf = leaf(young ? 0.9 : 1.35, young ? 0.38 : 0.52, def.leaf);  // 宽大蕉叶自冠心扇形展开
    lf.rotation.x = tilt;   // 叶尖略下垂
    holder.add(lf);
    const rib = cyl(0.012, 0.02, young ? 0.7 : 1.05, 0x5da352, 4);  // 叶脉
    rib.rotation.x = Math.PI / 2 + tilt;
    rib.position.y = 0.02;
    holder.add(rib);
    canopy.add(holder);
  });
  g.add(canopy);
  if (!young) {
    const bunch = new THREE.Group();
    bunch.name = 'banana-bunch';
    const axisTop = new THREE.Vector3(0.24, stemH + 0.02, 0.14);  // 果轴自冠心下方垂落
    const axisBot = new THREE.Vector3(0.38, 0.98, 0.2);
    bunch.add(stemBetween(axisTop.toArray(), axisBot.toArray(), 0.032, 0x6fae54));
    for (let hand = 0; hand < 3; hand++) {  // 3 层蕉把位于叶冠下方
      const c = axisTop.clone().lerp(axisBot, 0.28 + hand * 0.27);
      for (let k = 0; k < 4; k++) {
        const a = (k / 4) * Math.PI * 2 + hand * 0.55;
        const fing = new THREE.Group();  // 果指两段几何向外并向上弯
        fing.position.copy(c);
        fing.rotation.y = a;
        const s1 = cyl(0.038, 0.046, 0.17, def.fruit, 5);
        s1.rotation.x = 1.35;
        s1.position.set(0, 0.085 * Math.cos(1.35), 0.085 * Math.sin(1.35));
        fing.add(s1);
        const s2 = cyl(0.032, 0.04, 0.15, def.fruit, 5);
        s2.rotation.x = 0.55;
        s2.position.set(0, 0.17 * Math.cos(1.35) + 0.075 * Math.cos(0.55), 0.17 * Math.sin(1.35) + 0.075 * Math.sin(0.55));
        fing.add(s2);
        bunch.add(fing);
      }
    }
    fruits.add(bunch);
  }
  return g;
}

function buildGinseng(def, fruits, ctx) {  // 人参：叉状根 + 直立茎 + 顶端掌状复叶 + 伞形红果簇
  const { young } = ctx;
  const g = new THREE.Group();
  const ROOT_C = def.fruit;  // 0xe8d8b9
  const root = new THREE.Group();
  root.name = 'ginseng-root';
  const rootScale = ctx.mature ? 1 : 0.78;  // 大叶阶段根体小于成熟期
  const mainRoot = cyl(0.08, 0.13, 0.52, ROOT_C, 7);  // 主根
  mainRoot.position.y = 0.3;
  root.add(mainRoot);
  for (const s of [-1, 1]) {  // 左右两条叉根“腿”
    const leg = cyl(0.035, 0.06, 0.32, ROOT_C, 5);
    leg.position.set(s * 0.09, 0.02, 0);
    leg.rotation.z = -s * 0.38;
    root.add(leg);
  }
  for (const [az, hy] of [[0.8, 0.3], [3.9, 0.24]]) {  // 中部短侧根
    const side = cyl(0.016, 0.026, 0.18, ROOT_C, 4);
    side.position.set(Math.sin(az) * 0.12, hy, Math.cos(az) * 0.12);
    side.rotation.set(Math.cos(az) * 1.1, 0, -Math.sin(az) * 1.1);
    root.add(side);
  }
  for (const [az, hy] of [[2.2, 0.1], [5.2, 0.14]]) {  // 细须根
    const thin = cyl(0.006, 0.012, 0.14, ROOT_C, 3);
    thin.position.set(Math.sin(az) * 0.08, hy, Math.cos(az) * 0.08);
    thin.rotation.set(Math.cos(az) * 0.9, 0, -Math.sin(az) * 0.9);
    root.add(thin);
  }
  root.scale.setScalar(rootScale);
  g.add(root);
  const stem = cyl(0.03, 0.045, 0.62, def.leaf, 5);  // 单根直立茎
  stem.position.y = 0.78;
  g.add(stem);
  const leaves = new THREE.Group();
  leaves.name = 'ginseng-compound-leaves';
  leaves.position.y = 1.1;
  const leafN = young ? 3 : 4;
  for (let i = 0; i < leafN; i++) {  // 顶端 3-4 枚掌状复叶
    const a = (i / leafN) * Math.PI * 2 + 0.4;
    const holder = new THREE.Group();
    holder.rotation.y = a;
    const cl = compoundLeaf({ count: 5, len: young ? 0.24 : 0.3, w: 0.08, color: def.leaf, mode: 'palmate' });
    cl.rotation.x = -0.42;
    holder.add(cl);
    leaves.add(holder);
  }
  g.add(leaves);
  if (!young) {
    const berries = new THREE.Group();
    berries.name = 'ginseng-berries';
    berries.position.y = 1.34;  // 叶心上方伞形红果簇
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2;
      const r = i === 0 ? 0 : 0.11;
      const pedicel = cyl(0.006, 0.008, 0.07, def.leaf, 3);
      pedicel.position.set(Math.sin(a) * r * 0.5, 0.02, Math.cos(a) * r * 0.5);
      pedicel.rotation.set(Math.cos(a) * 0.6, 0, -Math.sin(a) * 0.6);
      berries.add(pedicel);
      const b = sphere(0.05, 0xd7263d, 5, 4);
      b.position.set(Math.sin(a) * r, 0.06, Math.cos(a) * r);
      berries.add(b);
    }
    fruits.add(berries);
  }
  return g;
}

// ---- 各体型构建（返回完整体，果实单独分组；ctx.young 为生长期简化结构）----
const BUILDERS = {
  root(def, fruits, ctx) {  // 白萝卜/胡萝卜：根茎半露 + 叶丛；人参专用构建
    if (def.id === 'renshen') return buildGinseng(def, fruits, ctx);
    const g = new THREE.Group();
    const { young } = ctx;
    const isCarrot = def.id === 'huluobo';
    const rootG = new THREE.Group();
    if (isCarrot) {
      const rootBody = cone(0.38, 1.05, def.fruit, 8);
      rootBody.rotation.x = Math.PI; rootBody.position.y = 0.3;
      rootG.add(rootBody);
      const shoulder = sphere(0.29, 0xffa15e, 7, 5);  // 根肩橙红渐变
      shoulder.scale.set(1, 0.35, 1); shoulder.position.y = 0.78; rootG.add(shoulder);
      const tail = cone(0.06, 0.3, def.fruit, 5);  // 根须尾
      tail.rotation.x = Math.PI; tail.position.y = -0.18; rootG.add(tail);
    } else {
      const rootBody = sphere(0.46, def.fruit, 8, 6);
      rootBody.scale.y = 0.9; rootBody.position.y = 0.28;
      rootG.add(rootBody);
      const shoulder = sphere(0.29, 0xcfe3b4, 7, 5);  // 肩部淡绿
      shoulder.scale.set(1, 0.5, 1); shoulder.position.y = 0.58; rootG.add(shoulder);
      const tail = cone(0.07, 0.32, def.fruit, 5);
      tail.rotation.x = Math.PI; tail.position.y = -0.2; rootG.add(tail);
    }
    if (young) { rootG.scale.setScalar(0.55); rootG.position.y = -0.06; }
    g.add(rootG);
    if (isCarrot) {  // 胡萝卜：羽状细叶
      for (let i = 0; i < 8; i++) {
        const a = (i / 8) * Math.PI * 2;
        const holder = new THREE.Group();
        holder.rotation.y = a;
        const lf = leaf(0.75, 0.06, def.leaf);
        lf.position.y = 0.72;
        lf.rotation.x = -1.3 + (i % 3) * 0.16;
        holder.add(lf); g.add(holder);
      }
    } else {  // 舒展莲座叶丛
      g.add(rosette(6, 0.7, 0.28, def.leaf, { y: 0.55, tilt: 0.85 }));
    }
    return g;
  },

  cabbage(def, fruits, ctx) {  // 大白菜：直立裹叶头 + 深绿外叶抱拢（第二轮重做）
    const g = new THREE.Group();
    const { young } = ctx;
    const f = young ? 0.72 : 1;  // 大叶期为小一号同构裹叶头
    const HEAD = 0xd6e4b0, RING = 0xe4edcb, HEART = 0xeef4dc;
    const outerN = young ? 3 : 4;
    for (let i = 0; i < outerN; i++) {  // 深绿外叶自基部向上抱拢，叶尖略外翻
      const a = (i / outerN) * Math.PI * 2 + 0.5;
      const holder = new THREE.Group();
      holder.name = 'napa-outer-leaf';
      holder.rotation.y = a;
      const lf = leaf(0.52 * f, 0.3 * f, def.leaf);
      lf.position.y = 0.1;
      lf.rotation.x = -1.02 + (i % 2) * 0.1;  // 向上环抱而非摊开
      holder.add(lf);
      g.add(holder);
    }
    const head = new THREE.Group();
    head.name = 'napa-head';
    const core = sphere(0.3, HEAD, 8, 6);  // 淡黄绿长圆裹叶头，高大于宽
    core.scale.set(0.88 * f, 1.45 * f, 0.88 * f);
    core.position.y = 0.52 * f;
    head.add(core);
    const ring1 = rosette(5, 0.22 * f, 0.1 * f, RING, { y: 0.98 * f, tilt: 0.5, phase: 0.3 });  // 顶部内叶卷边
    head.add(ring1);
    const ring2 = rosette(4, 0.14 * f, 0.07 * f, HEART, { y: 1.1 * f, tilt: 0.9, phase: 0.7 });
    head.add(ring2);
    g.add(head);
    return g;
  },

  cereal(def, fruits, ctx) {  // 小麦/水稻：秸秆束 + 穗（麦直立带芒，稻穗下垂）
    const g = new THREE.Group();
    const { young } = ctx;
    const isRice = def.id === 'shuidao';
    const stems = young ? 4 : 7;
    for (let i = 0; i < stems; i++) {
      const a = (i / stems) * Math.PI * 2, r = 0.16 + ((i * 5) % 4) * 0.05;
      const x = Math.cos(a) * r, z = Math.sin(a) * r;
      const h = (isRice ? 1.15 : 1.35) + ((i * 3) % 3) * 0.1;
      const stem = cyl(0.028, 0.042, h, def.leaf, 4);
      stem.position.set(x, h / 2, z);
      stem.rotation.set((Math.random() - 0.5) * 0.12, 0, (Math.random() - 0.5) * 0.12);
      g.add(stem);
      const holder = new THREE.Group();  // 旗叶
      holder.position.set(x, h * 0.72, z);
      holder.rotation.y = a + 1.2;
      const lf = leaf(0.55, 0.07, def.leaf);
      lf.rotation.x = -0.9;
      holder.add(lf); g.add(holder);
      if (isRice) {  // 稻穗：下垂散穗
        const ear = new THREE.Group();
        ear.position.set(x, h - 0.02, z);
        ear.rotation.set(Math.sin(a) * 0.4, 0, Math.cos(a) * 0.4 + 0.5);
        for (let k = 0; k < 6; k++) {
          const grain = sphere(0.055, def.fruit, 5, 4);
          grain.scale.set(0.8, 1.3, 0.8);
          grain.position.set((k % 2 ? 0.05 : -0.05) * (1 + k * 0.12), -0.06 - k * 0.07, 0);
          ear.add(grain);
        }
        fruits.add(ear);
      } else {  // 麦穗：直立籽粒 + 长芒
        const ear = new THREE.Group();
        ear.position.set(x, h + 0.04, z);
        for (let k = 0; k < 5; k++) {
          const grain = sphere(0.06, def.fruit, 5, 4);
          grain.scale.set(0.85, 1.25, 0.85);
          grain.position.set(k % 2 ? 0.045 : -0.045, k * 0.09, 0);
          ear.add(grain);
        }
        for (let k = 0; k < 3; k++) {
          const awn = cone(0.012, 0.28, def.fruit, 3);
          awn.position.set((k - 1) * 0.05, 0.5, 0);
          awn.rotation.z = (k - 1) * 0.25;
          ear.add(awn);
        }
        fruits.add(ear);
      }
    }
    return g;
  },

  corn(def, fruits, ctx) {  // 玉米：茎节 + 长叶 + 苞叶果穗
    const g = new THREE.Group();
    const { young } = ctx;
    const stem = cyl(0.065, 0.095, 2.3, def.leaf, 6); stem.position.y = 1.15; g.add(stem);
    for (const y of [0.7, 1.25, 1.8]) {  // 茎节
      const node = cyl(0.1, 0.1, 0.06, def.leaf, 6);
      node.position.y = y; g.add(node);
    }
    const leafCount = young ? 3 : 5;
    for (let i = 0; i < leafCount; i++) {
      const a = i * 1.65 + 0.4;
      const holder = new THREE.Group();
      holder.position.y = 0.75 + i * 0.32;
      holder.rotation.y = a;
      const lf = leaf(1.15, 0.15, def.leaf);
      lf.rotation.x = -0.55 - (i % 2) * 0.18;
      holder.add(lf); g.add(holder);
    }
    for (const [y, a] of [[1.05, 0.6], [1.5, 2.8]]) {
      const husk = new THREE.Group();
      husk.position.set(Math.cos(a) * 0.22, y, Math.sin(a) * 0.22);
      husk.rotation.set(Math.sin(a) * 0.42, 0, -Math.cos(a) * 0.42);
      const cob = cyl(0.12, 0.15, 0.5, def.fruit, 7);
      husk.add(cob);
      for (const s of [-1, 1]) {  // 苞叶
        const hp = cone(0.13, 0.5, 0x6fb25a, 5);
        hp.scale.z = 0.45;
        hp.position.set(s * 0.11, -0.04, 0);
        hp.rotation.z = s * 0.12;
        husk.add(hp);
      }
      for (let k = 0; k < 3; k++) {  // 玉米须
        const silk = cone(0.015, 0.2, 0xd9b382, 3);
        silk.position.set((k - 1) * 0.04, 0.32, 0);
        silk.rotation.z = (k - 1) * 0.3;
        husk.add(silk);
      }
      fruits.add(husk);
    }
    g.add(rosette(5, 0.42, 0.05, 0xd9c25e, { y: 2.32, tilt: 1.3 }));  // 雄穗
    return g;
  },

  bush(def, fruits, ctx) {  // 灌木类：仅按 ID 分派；土豆保留土垄，其余进入专用构建
    const dedicated = {
      qiezi: buildEggplant, fanqie: buildTomato, wandou: buildPea, lajiao: buildPepper,
    }[def.id];
    if (dedicated) return dedicated(def, fruits, ctx);
    const g = new THREE.Group();
    const { young } = ctx;
    const blobCount = young ? 3 : 4;
    for (let i = 0; i < blobCount; i++) {
      const a = (i / 4) * Math.PI * 2 + 0.5;
      const s = ico(0.4 + ((i * 3) % 3) * 0.05, def.leaf);
      s.position.set(Math.cos(a) * 0.3, 0.45 + ((i * 5) % 3) * 0.08, Math.sin(a) * 0.3);
      g.add(s);
    }
    if (!young) {
      const top = ico(0.42, def.leaf); top.position.y = 0.82; g.add(top);
    }
    if (def.id === 'tudou') {  // 土豆：垄土 + 半埋薯块（薯块长在土里）
      const mound = sphere(0.55, 0x8a6b4f, 8, 5);
      mound.scale.set(1.15, 0.3, 1.15); mound.position.y = 0.05; g.add(mound);
      for (let i = 0; i < 4; i++) {
        const a = (i / 4) * Math.PI * 2 + 0.4;
        const t = sphere(0.17, def.fruit, 6, 5);
        t.scale.set(1, 0.8, 1.25);
        t.position.set(Math.cos(a) * 0.44, 0.13, Math.sin(a) * 0.44);
        t.rotation.y = a;
        fruits.add(t);
        const eye = sphere(0.03, 0xb08d5f, 4, 3);  // 芽眼
        eye.position.set(Math.cos(a) * 0.52, 0.21, Math.sin(a) * 0.52);
        fruits.add(eye);
      }
      return g;
    }
    return g;
  },

  rose: buildRose,  // 红玫瑰：错落木质枝 + 复叶 + 双层花（专用构建）

  ground(def, fruits, ctx) {  // 南瓜/西瓜：贴地藤蔓（分节弯曲）
    const g = new THREE.Group();
    const { young } = ctx;
    const vineCount = 4;  // 幼年期同构：藤数一致，仅藤段更短、叶片更小（第二轮）
    const seg = young ? 0.68 : 1;
    for (let i = 0; i < vineCount; i++) {
      const a = (i / 4) * Math.PI * 2 + 0.3;
      const tilt = Math.PI / 2 - 0.12;
      const v1 = cyl(0.045, 0.06, 0.7 * seg, def.leaf, 5);
      v1.position.set(Math.cos(a) * 0.45 * seg, 0.06, Math.sin(a) * 0.45 * seg);
      v1.rotation.set(Math.sin(a) * tilt, 0, -Math.cos(a) * tilt);
      g.add(v1);
      const a2 = a + 0.35;
      const v2 = cyl(0.03, 0.045, 0.55 * seg, def.leaf, 5);
      v2.position.set(Math.cos(a2) * 0.95 * seg, 0.05, Math.sin(a2) * 0.95 * seg);
      v2.rotation.set(Math.sin(a2) * tilt, 0, -Math.cos(a2) * tilt);
      g.add(v2);
      for (const [off, ll, ww] of [[0, 0.5 * seg, 0.38], [0.3, 0.42 * seg, 0.3]]) {  // 错位叶片
        const holder = new THREE.Group();
        holder.position.set(Math.cos(a + off) * (off ? 1.15 : 0.8) * seg, 0.12, Math.sin(a + off) * (off ? 1.15 : 0.8) * seg);
        holder.rotation.y = a + off + 0.4;
        const lf = leaf(ll, ww, def.leaf);
        lf.rotation.x = -0.32;
        holder.add(lf); g.add(holder);
      }
      const tendril = cone(0.02, 0.22, def.leaf, 4);  // 卷须
      tendril.position.set(Math.cos(a + 0.6) * 0.7, 0.18, Math.sin(a + 0.6) * 0.7);
      tendril.rotation.set(0.7, 0, 0.5);
      g.add(tendril);
    }
    if (def.id === 'nangua') {
      const p = new THREE.Group();  // 南瓜：瓣棱（环列裂片）
      p.position.set(0.25, 0.4, 0.15);
      for (let i = 0; i < 8; i++) {
        const a = (i / 8) * Math.PI * 2;
        const lobe = sphere(0.4, def.fruit, 7, 5);
        lobe.scale.set(0.55, 0.78, 1.05);
        lobe.position.set(Math.cos(a) * 0.18, 0, Math.sin(a) * 0.18);
        lobe.rotation.y = Math.PI / 2 - a;
        p.add(lobe);
      }
      fruits.add(p);
      const stem = cyl(0.045, 0.07, 0.28, 0x6d4c41, 5);
      stem.position.set(0.25, 0.78, 0.15); stem.rotation.z = 0.25;
      fruits.add(stem);
    } else {  // 西瓜：球体 + 纵向条纹环
      const w = new THREE.Group();
      w.position.set(-0.2, 0.42, 0.2);
      const bodyM = sphere(0.52, 0x2e933c, 9, 7);
      bodyM.scale.y = 0.88; w.add(bodyM);
      for (let i = 0; i < 5; i++) {
        const stripe = new THREE.Mesh(new THREE.TorusGeometry(0.5, 0.035, 5, 24), mat(0x1b5e20));
        stripe.castShadow = true;
        stripe.scale.set(1, 0.88, 1);
        stripe.rotation.y = (i / 5) * Math.PI;
        w.add(stripe);
      }
      const tail = cone(0.03, 0.12, 0x6d4c41, 4);  // 瓜蒂
      tail.position.y = 0.5; w.add(tail);
      fruits.add(w);
    }
    return g;
  },

  low(def, fruits, ctx) {  // 草莓：深绿三出复叶 + 自然深红果自株心下垂
    const g = new THREE.Group();
    const { young } = ctx;
    const LEAF = 0x3f7a34;  // 比 def.leaf 深一档
    const leaves = new THREE.Group();
    leaves.name = 'strawberry-leaves';
    const leafCount = young ? 3 : 5;
    for (let i = 0; i < leafCount; i++) {
      const a = (i / leafCount) * Math.PI * 2 + 0.4;
      const tri = new THREE.Group();
      tri.rotation.y = a;
      const petiole = cyl(0.012, 0.016, 0.24, LEAF, 4);
      petiole.rotation.x = 1.15;
      petiole.position.set(0, 0.1, 0.11);
      tri.add(petiole);
      for (const [off, l] of [[-0.55, 0.24], [0, 0.28], [0.55, 0.24]]) {  // 三出复叶
        const lf = leaf(l, 0.15, LEAF);
        lf.position.set(0, 0.17, 0.2);
        lf.rotation.y = off;
        lf.rotation.x = -0.25;
        tri.add(lf);
      }
      leaves.add(tri);
    }
    g.add(leaves);
    if (young) return g;
    // 成熟深红 0xd9364b + 少量次熟浅红，果梗自株心向外下垂
    const spots = [
      [0.3, 0.08, 0.22, true], [-0.28, 0.07, 0.2, true], [0.04, 0.06, -0.3, true],
      [-0.14, 0.07, -0.16, false], [0.32, 0.07, -0.08, false],
    ];
    for (const [x, y, z, ripe] of spots) {
      const b = new THREE.Group();
      b.name = 'strawberry-fruit';
      b.position.set(x, y, z);
      const color = ripe ? 0xd9364b : 0xe0646f;
      b.add(stemBetween([-x * 0.9, 0.2, -z * 0.9], [0, 0.08, 0], 0.012, LEAF));  // 果梗自株心
      const berry = new THREE.Group();
      berry.name = 'strawberry-berry';
      // 一体水滴形：圆顶球与倒锥半嵌融合成连续上宽下尖轮廓（非两段式灯笼）
      const bodyB = sphere(0.115, color, 6, 5);
      bodyB.scale.set(1, 1.02, 0.92);
      berry.add(bodyB);
      const tipB = cone(0.1, 0.22, color, 6);  // 锥底嵌入球体下半，收成下尖
      tipB.rotation.x = Math.PI;
      tipB.position.y = -0.075;
      berry.add(tipB);
      const seedRows = [  // 暖黄籽点按纵列分布整个果面（含锥部）
        [0.06, 0.1], [-0.01, 0.108], [-0.09, 0.055],
      ];
      for (let r = 0; r < seedRows.length; r++) {
        const [sy, sr] = seedRows[r];
        for (let k = 0; k < 3; k++) {
          const sa = (k / 3) * Math.PI * 2 + r * 0.7 + 0.4;
          const seed = sphere(0.013, 0xffe08a, 4, 3);
          seed.scale.set(1, 0.6, 0.6);
          seed.position.set(Math.cos(sa) * sr, sy, Math.sin(sa) * sr * 0.9);
          seed.rotation.y = -sa;
          berry.add(seed);
        }
      }
      const cap = calyx(0.07, LEAF);  // 星形萼片位于顶端果柄连接处
      cap.position.y = 0.115;
      berry.add(cap);
      b.add(berry);
      fruits.add(b);
    }
    return g;
  },

  tree(def, fruits, ctx) {  // 果树：苹果/红枣/橙子/柚子专用组装，其余按品种参数构建
    const dedicated = {
      pingguo: buildApple, hongzao: buildJujube, chengzi: buildOrange, youzi: buildPomelo,
    }[def.id];
    if (dedicated) return dedicated(def, fruits, ctx);
    const g = new THREE.Group();
    const { young } = ctx;
    const S = TREE_STYLE[def.id] || TREE_STYLE.taozi;
    const trunkH = S.trunkH;
    const trunk = cyl(0.11, 0.17, trunkH, TRUNK, 6);
    trunk.position.y = trunkH / 2; g.add(trunk);
    if (!young) {  // 主干分枝
      for (const [a, y] of [[0.6, trunkH * 0.72], [2.8, trunkH * 0.84]]) {
        const br = cyl(0.045, 0.075, 0.55, TRUNK, 5);
        br.position.set(Math.cos(a) * 0.24, y, Math.sin(a) * 0.24);
        br.rotation.set(Math.sin(a) * 0.85, 0, -Math.cos(a) * 0.85);
        g.add(br);
      }
    }
    const canopyY = trunkH + 0.42;
    const c1 = ico(S.c1, def.leaf);
    c1.position.y = canopyY + 0.12; c1.scale.y = S.flat ?? 1; g.add(c1);
    if (!young) {
      const c2 = ico(S.c1 * 0.68, def.leaf);
      c2.position.set(0.55, canopyY - 0.22, 0.3); g.add(c2);
      const c3 = ico(S.c1 * 0.62, def.leaf);
      c3.position.set(-0.5, canopyY - 0.18, -0.35); g.add(c3);
      for (const p of S.fruitPos) {
        const fr = S.fruitR;
        const stem = cyl(0.014, 0.02, 0.16, 0x6d4c41, 4);  // 果梗
        stem.position.set(p[0], p[1] + fr + 0.05, p[2]);
        fruits.add(stem);
        const f = makeTreeFruit(def, fr);
        f.position.set(p[0], p[1], p[2]);
        fruits.add(f);
      }
    }
    return g;
  },

  vine(def, fruits, ctx) {  // 葡萄/葫芦：木架藤蔓 + 叶幕
    const g = new THREE.Group();
    const { young } = ctx;
    for (const s of [-1, 1]) {
      const post = box(0.09, 1.8, 0.09, TRUNK);
      post.position.set(0.8 * s, 0.9, 0); g.add(post);
    }
    const beam = box(1.9, 0.08, 0.08, TRUNK); beam.position.y = 1.8; g.add(beam);
    const vineH = young ? 1.3 : 1.62;  // 同一棚架同一路径，幼年期主蔓更矮、叶片更小（第二轮：构件数量不跳变）
    const v1 = cyl(0.04, 0.055, vineH, def.leaf, 5);  // 主蔓攀爬
    v1.position.set(-0.55, vineH / 2, 0); v1.rotation.z = 0.12; g.add(v1);
    const vineM = box(1.6, 0.09, 0.09, def.leaf);
    vineM.position.y = vineH + 0.04; vineM.rotation.z = 0.08; g.add(vineM);
    const leafN = 5;
    const lfLen = young ? 0.3 : 0.42;
    for (let i = 0; i < leafN; i++) {
      const holder = new THREE.Group();
      holder.position.set(-0.65 + i * 0.33, vineH + 0.1, 0.08);
      holder.rotation.y = i * 1.3;
      const lf = leaf(lfLen, 0.32, def.leaf);
      lf.rotation.x = -0.5;
      holder.add(lf); g.add(holder);
    }
    if (def.id === 'putao') {
      for (const x of [-0.45, 0.35]) {
        const hang = cyl(0.018, 0.018, 0.2, 0x6fae54, 4);  // 果梗
        hang.position.set(x, vineH - 0.02, 0.08); fruits.add(hang);
        for (let r = 0; r < 3; r++) for (let c = 0; c <= r; c++) {  // 果粒随机错位
          const b = sphere(0.1 + ((r + c) % 2) * 0.015, def.fruit, 5, 4);
          b.position.set(
            x + (c - r / 2) * 0.19 + ((r * 3 + c) % 2) * 0.03,
            vineH - 0.22 - r * 0.17,
            0.08 + (c % 2) * 0.04,
          );
          fruits.add(b);
        }
      }
    } else {  // 葫芦：上下双球 + 脐痕
      for (const x of [-0.4, 0.3]) {
        const hang = cyl(0.018, 0.018, 0.24, 0x6fae54, 4);
        hang.position.set(x, vineH - 0.02, 0.05); fruits.add(hang);
        const up = sphere(0.15, def.fruit, 6, 5); up.position.set(x, vineH - 0.34, 0.05);
        const dn = sphere(0.23, def.fruit, 7, 6); dn.position.set(x, vineH - 0.58, 0.05);
        fruits.add(up, dn);
        const navel = sphere(0.04, 0x8aa844, 4, 3);
        navel.position.set(x, vineH - 0.81, 0.05); fruits.add(navel);
      }
    }
    return g;
  },

  palm(def, fruits, ctx) {  // 香蕉专用假茎宽叶结构；椰子保留棕榈轮廓
    if (def.id === 'xiangjiao') return buildBanana(def, fruits, ctx);
    const g = new THREE.Group();
    const { young } = ctx;
    const trunkH = 2.4;
    const trunk = cyl(0.09, 0.15, trunkH, TRUNK, 6);
    trunk.position.y = trunkH / 2; trunk.rotation.z = 0.05; g.add(trunk);
    for (let i = 0; i < 4; i++) {  // 茎干环纹
      const ring = cyl(0.13 - i * 0.008, 0.13 - i * 0.008, 0.05, 0x7a5c4d, 6);
      ring.position.set(0.02, 0.5 + i * 0.5, 0); ring.rotation.z = 0.05;
      g.add(ring);
    }
    const leafCount = young ? 4 : 7;
    for (let i = 0; i < leafCount; i++) {
      const a = (i / leafCount) * Math.PI * 2 + 0.2;
      const holder = new THREE.Group();
      holder.position.set(0.12, trunkH + 0.08, 0);
      holder.rotation.y = a;
      const lf = leaf(1.5, 0.32, def.leaf);
      lf.rotation.x = -0.5 - (i % 3) * 0.22;
      holder.add(lf); g.add(holder);
    }
    // 椰子贴干簇生 + 椰眼
    for (let i = 0; i < 3; i++) {
      const a = i * 2.1 + 0.5;
      const c = sphere(0.17, def.fruit, 7, 6);
      c.position.set(Math.cos(a) * 0.2 + 0.1, trunkH - 0.12, Math.sin(a) * 0.2);
      fruits.add(c);
      const eye = sphere(0.03, 0x4e3525, 4, 3);
      eye.position.set(Math.cos(a) * 0.31 + 0.1, trunkH - 0.05, Math.sin(a) * 0.31);
      fruits.add(eye);
    }
    return g;
  },

  pineapple(def, fruits, ctx) {  // 菠萝：贴地放射莲座 + 居中椭柱果身 + 贴面菱形果眼 + 双层矮冠芽
    const g = new THREE.Group();
    const { young } = ctx;
    const ros = new THREE.Group();
    ros.name = 'pineapple-rosette';
    const outer = young ? 4 : 7;
    for (let i = 0; i < outer; i++) {  // 外层叶更平
      const a = (i / outer) * Math.PI * 2 + 0.2;
      const holder = new THREE.Group();
      holder.rotation.y = a;
      const lf = lanceLeaf(young ? 0.45 : 0.6, 0.065, def.leaf);
      lf.rotation.x = -0.15;
      lf.position.y = 0.06;
      holder.add(lf);
      ros.add(holder);
    }
    const inner = young ? 3 : 5;
    for (let i = 0; i < inner; i++) {  // 内层叶更直
      const a = (i / inner) * Math.PI * 2 + 0.55;
      const holder = new THREE.Group();
      holder.rotation.y = a;
      const lf = lanceLeaf(young ? 0.35 : 0.45, 0.055, 0x7cc46f);
      lf.rotation.x = -0.7;
      lf.position.y = 0.06;
      holder.add(lf);
      ros.add(holder);
    }
    g.add(ros);
    if (young) return g;
    const bodyG = new THREE.Group();
    bodyG.name = 'pineapple-fruit-body';
    const bodyM = cyl(0.24, 0.3, 0.55, 0xdf9a35, 8);  // 上窄下宽椭圆柱金橙果身，与莲座基部相接
    bodyM.position.y = 0.4;
    bodyG.add(bodyM);
    fruits.add(bodyG);
    for (let r = 0; r < 4; r++) {  // 微凸四棱锥果眼 + 绿色苞片尖，螺旋错位铺满果面
      const y = 0.22 + r * 0.12;
      const rad = 0.3 - 0.06 * ((y - 0.125) / 0.55) + 0.055;  // 眼心凸出果面约 0.055
      for (let c = 0; c < 7; c++) {
        const a = ((c + r * 0.5) / 7) * Math.PI * 2;
        const eye = new THREE.Group();
        eye.name = 'pineapple-eye';
        eye.position.set(Math.sin(a) * rad, y, Math.cos(a) * rad);
        eye.rotation.y = a;
        const bump = cone(0.062, 0.1, (r + c) % 2 ? 0xb5762a : 0xe8ac45, 4);  // 低矮四棱锥，非尖刺
        bump.rotation.x = Math.PI / 2 - 0.25;  // 指向果面外并略向上
        eye.add(bump);
        const bract = cone(0.02, 0.06, 0x5f9e46, 4);  // 果眼顶端苞片尖
        bract.position.set(0, 0.028, 0.058);
        bract.rotation.x = Math.PI / 2 - 0.55;
        eye.add(bract);
        fruits.add(eye);
      }
    }
    const crown = new THREE.Group();
    crown.name = 'pineapple-crown';
    crown.position.y = 0.68;
    for (let i = 0; i < 5; i++) {  // 冠芽下层（高度不超过果身 45%）
      const a = (i / 5) * Math.PI * 2;
      const holder = new THREE.Group();
      holder.rotation.y = a;
      const lf = leaf(0.2, 0.04, 0x6fbf63);
      lf.rotation.x = -0.55;
      holder.add(lf);
      crown.add(holder);
    }
    for (let i = 0; i < 4; i++) {  // 冠芽上层
      const a = (i / 4) * Math.PI * 2 + 0.4;
      const holder = new THREE.Group();
      holder.rotation.y = a;
      const lf = leaf(0.13, 0.035, 0x8fd07a);
      lf.rotation.x = -1.0;
      holder.add(lf);
      crown.add(holder);
    }
    fruits.add(crown);
    return g;
  },

  fungus(def, fruits, ctx) {  // 灵芝：按阶段构建，菌盖为主体（不依赖成熟果实显隐）
    const g = new THREE.Group();
    const stage = ctx.mature ? ctx.totalStages : ctx.stage;
    const STEM = 0x7a4a32, CAP = 0x9c6644, CAP_DARK = 0x8a4a3a, RING = 0xb0714f, RIM = 0xf2e8d5;
    if (stage <= 0) {  // 发芽：短白菌蕾，顶部有小褐点
      const stipe = cyl(0.05, 0.08, 0.22, 0xe8d8b9, 6);
      stipe.name = 'lingzhi-stipe';
      stipe.position.y = 0.11;
      g.add(stipe);
      const bud = sphere(0.1, 0xe8d8b9, 6, 5);
      bud.scale.set(1.1, 0.9, 1);
      bud.position.y = 0.28;
      g.add(bud);
      const dot = sphere(0.04, 0x8a5636, 5, 4);  // 顶部小褐点
      dot.position.y = 0.36;
      g.add(dot);
      return g;
    }
    // 菌柄柱体随阶段单调升高，任一阶段都完整露出（第二轮修订：不允许菌盖贴地遮柄）
    const stipeH = { 1: 0.36, 2: 0.56, 3: 0.7 }[stage] ?? 0.85;
    const stipe = cyl(0.045, 0.075, stipeH, STEM, 6);
    stipe.name = 'lingzhi-stipe';
    stipe.position.set(-0.04, stipeH / 2, 0);
    stipe.rotation.z = 0.06;  // 略倾，仿侧生柄
    g.add(stipe);
    const capW = { 1: 0.34, 2: 0.62, 3: 0.78 }[stage] ?? 0.84;
    const cap = new THREE.Group();
    cap.name = 'lingzhi-cap';
    // 盖底（含浅色边缘）与柄顶咬合 0.02，菌盖由菌柄直接托举、不悬空
    cap.position.set(0.03, stipeH + 0.08 * capW - 0.02, 0);
    cap.rotation.z = 0.1;  // 扇面微侧，菌柄接于盖缘附近
    if (stage === 1) {  // 小叶：短柄 + 小型圆肾形褐色菌盖
      cap.add(kidneyCap(0.34, 0.3, CAP, RIM));
    } else if (stage === 2) {  // 大叶：中高菌柄托举单层展开扇面，带浅色边缘
      cap.add(kidneyCap(0.62, 0.5, CAP, RIM));
    } else {  // 开花/成熟：更高菌柄 + 更宽扇面 + 同心棕红环带（菌类无花）
      cap.add(kidneyCap(stage === 3 ? 0.78 : 0.84, stage === 3 ? 0.6 : 0.64, CAP, RIM));
      const band1 = sphere(0.5, CAP_DARK, 8, 4);
      band1.scale.set(0.56, 0.035, 0.44);
      band1.position.y = 0.055;
      cap.add(band1);
      const band2 = sphere(0.5, RING, 8, 4);
      band2.scale.set(0.34, 0.035, 0.26);
      band2.position.y = 0.06;
      cap.add(band2);
      if (stage >= 4) {  // 成熟：主扇面 + 带小柄的侧生小扇面
        const sideG = new THREE.Group();
        sideG.name = 'lingzhi-cap-side';
        const sideStipe = cyl(0.03, 0.05, 0.3, STEM, 5);
        sideStipe.position.set(0.24, 0.15, 0.12);
        sideStipe.rotation.z = -0.2;
        sideG.add(sideStipe);
        const side = kidneyCap(0.4, 0.32, CAP, RIM);
        side.position.set(0.34, 0.34, 0.14);
        side.rotation.z = -0.18;
        sideG.add(side);
        g.add(sideG);
      }
    }
    g.add(cap);
    return g;
  },

  money(def, fruits, ctx) {  // 摇钱树：金冠 + 金币
    const g = new THREE.Group();
    const { young } = ctx;
    const trunk = cyl(0.13, 0.19, 1.6, 0x7d5a43, 6); trunk.position.y = 0.8; g.add(trunk);
    const c1 = ico(0.9, def.leaf); c1.position.y = 2.1; g.add(c1);
    if (!young) {
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
    }
    return g;
  },
};

// 果树品种特征：树干高、树冠尺寸、果径、果实分布（桃子/石榴通用路径）
const TREE_STYLE = {
  taozi: {  // 桃子：粉果带尖
    trunkH: 1.35, c1: 0.78, fruitR: 0.16,
    fruitPos: [[0.66, 1.78, 0.5], [-0.7, 1.88, 0.28], [0.18, 2.28, -0.48], [0.52, 2.24, 0.3], [-0.28, 1.62, -0.6], [-0.08, 2.46, 0.15]],
  },
  shiliu: {  // 石榴：红果萼冠
    trunkH: 1.3, c1: 0.74, fruitR: 0.16,
    fruitPos: [[0.64, 1.75, 0.5], [-0.68, 1.85, 0.26], [0.16, 2.2, -0.46], [-0.28, 1.58, -0.58], [0.48, 2.18, 0.3], [-0.08, 2.38, 0.14]],
  },
};

// 果树果实造型（品种特征件）
function makeTreeFruit(def, r) {
  const g = new THREE.Group();
  const f = sphere(r, def.fruit, 7, 6);
  switch (def.id) {
    case 'taozi': {  // 桃：圆果带尖
      f.scale.set(1, 1.05, 0.95);
      const tip = cone(r * 0.3, r * 0.5, def.fruit, 5);
      tip.position.set(r * 0.35, r * 0.75, 0);
      tip.rotation.z = -0.5;
      g.add(tip);
      break;
    }
    case 'shiliu': {  // 石榴：顶部萼冠
      for (let i = 0; i < 5; i++) {
        const a = (i / 5) * Math.PI * 2;
        const c = cone(r * 0.22, r * 0.55, def.fruit, 4);
        c.position.set(Math.cos(a) * r * 0.35, r * 0.95, Math.sin(a) * r * 0.35);
        c.rotation.set(Math.sin(a) * 0.5, 0, -Math.cos(a) * 0.5);
        g.add(c);
      }
      break;
    }
  }
  g.add(f);
  return g;
}

/**
 * 创建作物模型
 * @param def 作物定义
 * @param opts { stage, totalStages, mature, withered }
 */
export function createCropModel(def, opts) {
  const g = new THREE.Group();
  // 完整阶段上下文：构建器据此区分发芽/小叶/大叶/开花/成熟
  const context = {
    young: !opts.mature && opts.stage < opts.totalStages - 1,
    stage: opts.stage,
    totalStages: opts.totalStages,
    mature: Boolean(opts.mature),
  };
  if (opts.withered) {  // 枯萎：灰褐残株（成熟上下文，避免构建器读到 undefined）
    const deadCtx = { young: false, stage: opts.totalStages, totalStages: opts.totalStages, mature: true };
    const dead = BUILDERS[def.body] ? buildSafe(def, new THREE.Group(), deadCtx) : null;
    const wrap = new THREE.Group();
    if (dead) {
      dead.traverse(o => { if (o.isMesh) o.material = mat(0x9e8a72); });
      dead.scale.setScalar(0.8); dead.rotation.z = 0.18; wrap.add(dead);
    }
    return wrap;
  }
  if (def.body === 'fungus') {  // 灵芝：全阶段由专用构建器输出，菌盖不走果实显隐
    g.add(buildSafe(def, new THREE.Group(), context));
    return g;
  }
  if (!opts.mature && opts.stage === 0) { g.add(sprout(def, 0.8, context)); return g; }
  if (!opts.mature && opts.stage === 1) { g.add(sprout(def, 1.6, context)); return g; }

  const fruits = new THREE.Group();
  // 生长期（未到最后阶段）使用简化结构：树冠/果枝等后期才展开
  const body = buildSafe(def, fruits, context);
  const scale = opts.mature ? 1 : (context.young ? 0.68 : 0.92);
  body.scale.setScalar(scale);
  fruits.scale.setScalar(scale);
  g.add(body);
  if (opts.mature) g.add(fruits);   // 仅成熟阶段显示果实
  if (!opts.mature && opts.stage >= 3) {  // 开花阶段：按体型定制花序
    const fl = flowerShow(def, scale, context);
    if (fl) g.add(fl);
  }
  return g;
}

const DEFAULT_CONTEXT = { young: false, stage: 3, totalStages: 3, mature: true };

function buildSafe(def, fruits, context = DEFAULT_CONTEXT) {
  const builder = BUILDERS[def.body] || BUILDERS.bush;
  return builder(def, fruits, context);
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

const DOG_MODEL_STYLE = Object.freeze({
  tugou: {
    scale: 1.52,
    bodyScale: [1.52, 0.91, 0.8], bodyY: 0.49,
    chestScale: [0.76, 1.08, 0.8], chestY: 0.48,
    headRadius: 0.29, headScale: [1.04, 1.05, 0.94], headPivot: [0.34, 0.74],
    snoutRadius: 0.15, snoutScale: [1.22, 0.72, 0.88], snoutX: 0.42,
    earRadius: 0.12, earHeight: 0.3, earY: 0.32, earZ: 0.18,
    eyeZ: 0.23,
    legLength: 0.38, legRadius: 0.075, legPivotY: 0.36, frontX: 0.27, hindX: -0.3,
    tailThickness: 0.095, collar: 0xc84638, lightBlend: 0.56,
  },
  muyang: {
    scale: 1.57,
    bodyScale: [1.72, 0.86, 0.73], bodyY: 0.55,
    chestScale: [0.68, 1.2, 0.74], chestY: 0.53,
    headRadius: 0.27, headScale: [1.1, 1.12, 0.84], headPivot: [0.4, 0.82],
    snoutRadius: 0.14, snoutScale: [1.48, 0.64, 0.74], snoutX: 0.45,
    earRadius: 0.11, earHeight: 0.36, earY: 0.37, earZ: 0.16,
    eyeZ: 0.19,
    legLength: 0.47, legRadius: 0.065, legPivotY: 0.43, frontX: 0.31, hindX: -0.35,
    tailThickness: 0.08, collar: 0x2d6688, lightBlend: 0.84,
  },
  zangao: {
    scale: 1.7,
    bodyScale: [1.58, 1.12, 1.02], bodyY: 0.55,
    chestScale: [0.94, 1.22, 0.98], chestY: 0.53,
    headRadius: 0.34, headScale: [1.08, 1.08, 1.04], headPivot: [0.37, 0.81],
    snoutRadius: 0.18, snoutScale: [1.22, 0.78, 0.94], snoutX: 0.46,
    earRadius: 0.115, earHeight: 0.24, earY: 0.3, earZ: 0.21,
    eyeZ: 0.31,
    legLength: 0.43, legRadius: 0.1, legPivotY: 0.41, frontX: 0.29, hindX: -0.32,
    tailThickness: 0.135, collar: 0xa77a26, lightBlend: 0.28,
  },
});

function addDogEye(headPivot, z, darkCoat) {
  const side = Math.sign(z);
  const eyeWhite = sphere(0.058, darkCoat, 7, 5);
  eyeWhite.scale.set(1.08, 0.88, 0.22);
  eyeWhite.userData.baseScaleY = eyeWhite.scale.y;
  eyeWhite.position.set(0.315, 0.125, z);
  headPivot.add(eyeWhite);

  const iris = sphere(0.034, 0xa97835, 6, 5);
  iris.scale.set(0.92, 0.94, 0.2);
  iris.position.set(0.332, 0.123, z + side * 0.009);
  headPivot.add(iris);

  const pupil = sphere(0.019, 0x100e0d, 6, 5);
  pupil.scale.set(0.72, 1, 0.18);
  pupil.userData.baseScaleY = pupil.scale.y;
  pupil.position.set(0.348, 0.123, z + side * 0.014);
  headPivot.add(pupil);

  const highlight = sphere(0.008, 0xffffff, 5, 4);
  highlight.position.set(0.36, 0.142, z + side * 0.017);
  headPivot.add(highlight);

  const brow = box(0.13, 0.027, 0.04, darkCoat);
  brow.position.set(0.3, 0.215, z);
  brow.rotation.x = -0.16 * Math.sign(z);
  brow.rotation.z = -0.08;
  headPivot.add(brow);
  return { eyeWhite, iris, pupil, highlight, brow };
}

function addDogLeg(root, {
  x, z, style, upperColor, lowerColor, pawColor, sockColor = null,
}) {
  const pivot = new THREE.Group();
  pivot.position.set(x, style.legPivotY, z);
  root.add(pivot);

  const upperLength = style.legLength * 0.55;
  const lowerLength = style.legLength * 0.48;
  const upper = cyl(style.legRadius * 0.86, style.legRadius * 1.25, upperLength, upperColor, 6);
  upper.position.y = -upperLength / 2;
  pivot.add(upper);

  const knee = sphere(style.legRadius * 1.02, lowerColor, 6, 5);
  knee.scale.set(0.9, 0.82, 0.92);
  knee.position.y = -upperLength;
  pivot.add(knee);

  const lower = cyl(style.legRadius * 0.7, style.legRadius * 0.86, lowerLength, sockColor ?? lowerColor, 6);
  lower.position.set(0.025, -upperLength - lowerLength / 2, 0);
  lower.rotation.z = -0.08;
  pivot.add(lower);

  const paw = sphere(style.legRadius * 1.25, pawColor, 6, 5);
  paw.scale.set(1.38, 0.55, 1.02);
  paw.position.set(0.07, -style.legLength - 0.015, 0);
  pivot.add(paw);

  pivot.userData.parts = { upper, knee, lower, paw };
  return pivot;
}

function addDogTail(root, breedId, style, coatColor, lightCoat, darkCoat) {
  const pivot = new THREE.Group();
  pivot.position.set(-0.49, breedId === 'zangao' ? 0.7 : 0.61, 0);
  root.add(pivot);

  const segments = [];
  const points = breedId === 'muyang'
    ? [[0, 0], [-0.15, -0.02], [-0.29, -0.08], [-0.41, -0.18], [-0.52, -0.31]]
    : [[0, 0], [-0.14, 0.045], [-0.26, 0.13], [-0.35, 0.25], [-0.39, 0.41]];
  const radii = breedId === 'muyang'
    ? [style.tailThickness * 0.78, style.tailThickness * 0.72, style.tailThickness * 0.62, style.tailThickness * 0.46, style.tailThickness * 0.24]
    : [style.tailThickness, style.tailThickness * 0.92, style.tailThickness * 0.8, style.tailThickness * 0.58, style.tailThickness * 0.28];

  for (let index = 0; index < points.length - 1; index++) {
    const from = points[index];
    const to = points[index + 1];
    const dx = to[0] - from[0];
    const dy = to[1] - from[1];
    const length = Math.hypot(dx, dy);
    const color = breedId === 'muyang' && index === 3
      ? lightCoat
      : breedId === 'tugou' && index === 3 ? darkCoat : coatColor;
    const segment = cyl(radii[index + 1], radii[index], length * 1.08, color, 7);
    segment.position.set((from[0] + to[0]) / 2, (from[1] + to[1]) / 2, 0);
    segment.rotation.z = -Math.atan2(dx, dy);
    pivot.add(segment);
    segments.push(segment);

    const joint = sphere(radii[index + 1] * 1.04, color, 7, 5);
    joint.position.set(to[0], to[1], 0);
    pivot.add(joint);
  }
  pivot.userData.segments = segments;
  return pivot;
}

// 看门狗模型；兼容旧的 color 数字参数，默认按土狗体型创建。
export function createDogModel(dogDef) {
  const definition = typeof dogDef === 'object' && dogDef !== null
    ? dogDef
    : { id: 'tugou', color: dogDef };
  const breedId = DOG_MODEL_STYLE[definition.id] ? definition.id : 'tugou';
  const style = DOG_MODEL_STYLE[breedId];
  const g = new THREE.Group();
  // 田地和围栏比例较大，按场景尺度放大，保证俯视角也能读出表情与姿势。
  g.scale.setScalar(style.scale);
  const coat = new THREE.Color(definition.color ?? 0xb08968);
  const c = coat.getHex();
  const darkCoat = coat.clone().lerp(new THREE.Color(0x231c18), 0.35).getHex();
  const deepCoat = coat.clone().lerp(new THREE.Color(0x171310), 0.58).getHex();
  const lightCoat = coat.clone().lerp(new THREE.Color(0xf5eee3), style.lightBlend).getHex();
  const markings = {};

  const root = new THREE.Group();
  g.add(root);
  const body = sphere(0.34, c, 10, 7);
  body.scale.set(...style.bodyScale);
  body.position.set(-0.03, style.bodyY, 0);
  root.add(body);

  const flank = sphere(0.29, darkCoat, 9, 6);
  flank.scale.set(0.82, 0.78, 0.78);
  flank.position.set(-0.36, style.bodyY + 0.01, 0);
  root.add(flank);

  const belly = sphere(0.22, lightCoat, 8, 6);
  belly.scale.set(1.45, 0.32, 0.72);
  belly.position.set(-0.04, style.bodyY - 0.24, 0);
  root.add(belly);
  markings.belly = belly;

  const chest = sphere(0.23, lightCoat, 9, 6);
  chest.scale.set(...style.chestScale);
  chest.position.set(0.28, style.chestY, 0);
  root.add(chest);

  if (breedId === 'muyang') {
    const saddle = sphere(0.31, deepCoat, 10, 7);
    saddle.scale.set(1.28, 0.42, 0.86);
    saddle.position.set(-0.1, 0.74, 0);
    root.add(saddle);
    markings.saddle = saddle;

    const shoulderCape = sphere(0.24, darkCoat, 9, 6);
    shoulderCape.scale.set(0.68, 0.95, 0.88);
    shoulderCape.position.set(0.2, 0.62, 0);
    root.add(shoulderCape);
    markings.shoulderCape = shoulderCape;
  } else if (breedId === 'zangao') {
    const maneColor = coat.clone().lerp(new THREE.Color(0x9a7658), 0.24).getHex();
    const mane = sphere(0.34, maneColor, 10, 7);
    mane.scale.set(0.92, 1.16, 1.2);
    mane.position.set(0.25, 0.63, 0);
    root.add(mane);
    markings.mane = mane;

    const maneCrown = sphere(0.28, darkCoat, 9, 6);
    maneCrown.scale.set(0.7, 1.1, 1.2);
    maneCrown.position.set(0.35, 0.72, 0);
    root.add(maneCrown);
    markings.maneCrown = maneCrown;
  } else {
    const shoulderPatch = sphere(0.19, lightCoat, 8, 5);
    shoulderPatch.scale.set(0.58, 0.82, 0.86);
    shoulderPatch.position.set(0.25, 0.5, 0);
    root.add(shoulderPatch);
    markings.shoulderPatch = shoulderPatch;
  }

  // 头、腿与尾巴使用独立枢轴，行为控制器只改枢轴，不破坏模型局部坐标。
  const headPivot = new THREE.Group();
  headPivot.position.set(style.headPivot[0], style.headPivot[1], 0);
  headPivot.userData.baseX = headPivot.position.x;
  root.add(headPivot);
  const headColor = breedId === 'muyang' ? darkCoat : c;
  const head = sphere(style.headRadius, headColor, 10, 7);
  head.scale.set(...style.headScale);
  head.position.set(0.17, 0.04, 0);
  headPivot.add(head);

  const cheekColor = breedId === 'zangao' ? lightCoat : breedId === 'muyang' ? 0xe7e6dc : lightCoat;
  const cheeks = [];
  for (const side of [-1, 1]) {
    const cheek = sphere(style.snoutRadius * 0.8, cheekColor, 8, 6);
    cheek.scale.set(1.12, 0.78, 0.78);
    cheek.position.set(style.snoutX - 0.005, -0.075, side * style.snoutRadius * 0.54);
    headPivot.add(cheek);
    cheeks.push(cheek);
  }
  const snout = sphere(style.snoutRadius, cheekColor, 9, 6);
  snout.scale.set(...style.snoutScale);
  snout.position.set(style.snoutX, -0.07, 0);
  headPivot.add(snout);

  const nose = sphere(breedId === 'zangao' ? 0.072 : 0.064, 0x1a1716, 7, 5);
  nose.scale.set(1.05, 0.82, 1.18);
  nose.position.set(style.snoutX + 0.15, -0.052, 0);
  headPivot.add(nose);
  const noseShine = sphere(0.014, 0x807873, 5, 4);
  noseShine.position.set(style.snoutX + 0.188, -0.026, 0.028);
  headPivot.add(noseShine);

  const mouth = box(0.105, 0.016, 0.026, 0x30231f);
  mouth.position.set(style.snoutX + 0.055, -0.169, 0);
  mouth.rotation.z = -0.05;
  headPivot.add(mouth);
  const chin = sphere(style.snoutRadius * 0.58, cheekColor, 7, 5);
  chin.scale.set(1.18, 0.34, 0.74);
  chin.position.set(style.snoutX + 0.015, -0.175, 0);
  headPivot.add(chin);

  if (breedId === 'muyang') {
    const blaze = sphere(0.125, 0xf0eee4, 8, 6);
    blaze.scale.set(1.4, 0.34, 0.46);
    blaze.position.set(0.22, 0.275, 0);
    headPivot.add(blaze);
    markings.blaze = blaze;
    const mask = sphere(0.2, deepCoat, 8, 6);
    mask.scale.set(0.62, 0.52, 0.9);
    mask.position.set(0.25, 0.13, 0);
    headPivot.add(mask);
    markings.mask = mask;
  } else if (breedId === 'zangao') {
    const browPatchA = sphere(0.052, 0xb38a56, 6, 5);
    browPatchA.scale.set(1.3, 0.5, 0.72);
    browPatchA.position.set(0.32, 0.215, -style.eyeZ);
    headPivot.add(browPatchA);
    const browPatchB = browPatchA.clone();
    browPatchB.position.z = style.eyeZ;
    headPivot.add(browPatchB);
    markings.browPatches = [browPatchA, browPatchB];
  }

  const eyes = [];
  const eyeWhites = [];
  const eyeHighlights = [];
  const brows = [];
  for (const side of [-1, 1]) {
    const eyeParts = addDogEye(headPivot, style.eyeZ * side, darkCoat);
    eyes.push(eyeParts.pupil);
    eyeWhites.push(eyeParts.eyeWhite);
    eyeHighlights.push(eyeParts.highlight);
    brows.push(eyeParts.brow);
  }

  const ears = [];
  for (const side of [-1, 1]) {
    const ear = new THREE.Group();
    ear.position.set(0.08, style.earY, style.earZ * side);
    ear.rotation.x = breedId === 'zangao' ? 0.48 * side : 0.08 * side;
    ear.rotation.z = breedId === 'zangao' ? 0.34 : -0.12;
    ear.userData.baseRotationX = ear.rotation.x;
    ear.userData.baseRotationZ = ear.rotation.z;
    headPivot.add(ear);

    const outer = cone(style.earRadius, style.earHeight, darkCoat, 5);
    outer.position.y = style.earHeight * 0.2;
    ear.add(outer);
    const inner = cone(style.earRadius * 0.52, style.earHeight * 0.62, breedId === 'zangao' ? 0x6f5044 : 0xd9957d, 5);
    inner.position.set(0.018, style.earHeight * 0.2, 0.008 * side);
    inner.scale.z = 0.72;
    ear.add(inner);
    ears.push(ear);
  }

  const collar = cyl(
    breedId === 'zangao' ? 0.235 : 0.205,
    breedId === 'zangao' ? 0.235 : 0.205,
    0.075,
    style.collar,
    10,
  );
  collar.rotation.z = Math.PI / 2;
  collar.position.set(-0.04, -0.02, 0);
  collar.scale.z = breedId === 'zangao' ? 1.08 : 0.94;
  headPivot.add(collar);

  const tagPivot = new THREE.Group();
  tagPivot.position.set(-0.015, -0.23, 0);
  headPivot.add(tagPivot);
  const tag = sphere(breedId === 'zangao' ? 0.052 : 0.044, 0xe2ba45, 6, 5);
  tag.scale.set(0.48, 1, 0.82);
  tagPivot.add(tag);
  const tagInset = sphere(breedId === 'zangao' ? 0.026 : 0.022, 0xffe89a, 6, 5);
  tagInset.scale.set(0.5, 1, 0.82);
  tagInset.position.x = 0.023;
  tagPivot.add(tagInset);

  const frontLegs = [];
  const hindLegs = [];
  for (const side of [-1, 1]) {
    const sockColor = breedId === 'muyang' ? 0xe8e6dc : breedId === 'zangao' ? lightCoat : null;
    frontLegs.push(addDogLeg(root, {
      x: style.frontX,
      z: 0.18 * side,
      style,
      upperColor: breedId === 'muyang' ? darkCoat : c,
      lowerColor: c,
      pawColor: breedId === 'muyang' ? 0xe8e6dc : darkCoat,
      sockColor,
    }));
    hindLegs.push(addDogLeg(root, {
      x: style.hindX,
      z: 0.18 * side,
      style,
      upperColor: darkCoat,
      lowerColor: c,
      pawColor: breedId === 'muyang' ? 0xe8e6dc : darkCoat,
      sockColor: breedId === 'muyang' ? 0xe8e6dc : null,
    }));
  }

  const tailPivot = addDogTail(root, breedId, style, c, lightCoat, darkCoat);

  g.traverse((node) => {
    if (node.isMesh) node.receiveShadow = true;
  });

  g.userData.tail = tailPivot;
  g.userData.breedId = breedId;
  g.userData.rig = {
    root,
    body,
    head: headPivot,
    tail: tailPivot,
    frontLegs,
    hindLegs,
    ears,
    eyes,
    eyeWhites,
    eyeHighlights,
    brows,
    collar,
    tag: tagPivot,
    markings,
  };
  return g;
}
