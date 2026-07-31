// ============================================================
// 3D 农场场景：低多边形田园 + 日夜循环 + 交互动画
// ============================================================
import * as THREE from 'three';
import { OrbitControls } from 'three/examples/jsm/controls/OrbitControls.js';
import { RoundedBoxGeometry } from 'three/examples/jsm/geometries/RoundedBoxGeometry.js';
import { mat, isSharedMaterial, createCropModel, createWeedModel, createPestModel, createResidueModel, createDogModel } from './crops.js';
import { DogBehaviorController } from './dogBehavior.js';
import { clearAndDispose, disposeExclusiveMaterial, disposeObject3D } from './dispose3d.js';
import { PLOT } from './state.js';

const disposeOpts = { isSharedMaterial };

const COLS = 6, ROWS = 3, GAP = 5;
const DAY_SHADOW_INTENSITY = 0.72;
const CLOUD_SHADOW_MIN_OPACITY = 0.09;
const CLOUD_SHADOW_OPACITY_RANGE = 0.05;
const PLOT_RIM_HEIGHT = 0.22;
const PLOT_RIM_CENTER_Y = 0.05;
// 顶部土层必须和圆角底座拉开深度距离；过小会在俯视移动时发生 Z-fighting。
const PLOT_SURFACE_Y = PLOT_RIM_CENTER_Y + PLOT_RIM_HEIGHT / 2 + 0.02;
const PLOT_FURROW_HEIGHT = 0.05;
const PLOT_FURROW_Y = PLOT_SURFACE_Y + PLOT_FURROW_HEIGHT / 2 + 0.002;
export const plotPos = (id) => ({
  x: ((id % COLS) - (COLS - 1) / 2) * GAP,
  z: ((ROWS - 1) / 2 - Math.floor(id / COLS)) * GAP,   // 初始地块靠近相机，扩地向远处延伸
});

// 日夜关键色
const SKY = [
  { t: 0.00, sky: 0x1b2a4a, sun: 0x93a8e8, sunI: 0.55, hemi: 0.5 },  // 午夜
  { t: 0.20, sky: 0x1b2a4a, sun: 0x93a8e8, sunI: 0.55, hemi: 0.5 },
  { t: 0.28, sky: 0xffb37e, sun: 0xffcc88, sunI: 0.8,  hemi: 0.55 },  // 日出
  { t: 0.38, sky: 0x8fd3ff, sun: 0xfff3d6, sunI: 1.15, hemi: 0.8  },  // 上午
  { t: 0.55, sky: 0x9adcff, sun: 0xffffff, sunI: 1.25, hemi: 0.9  },  // 午后
  { t: 0.72, sky: 0xffab6e, sun: 0xffb066, sunI: 0.85, hemi: 0.58 },  // 黄昏
  { t: 0.80, sky: 0x45548c, sun: 0xa8b8f0, sunI: 0.58, hemi: 0.5 },
  { t: 1.00, sky: 0x1b2a4a, sun: 0x93a8e8, sunI: 0.55, hemi: 0.5 },
];

function lerpKey(phase) {
  let a = SKY[0], b = SKY[SKY.length - 1];
  for (let i = 0; i < SKY.length - 1; i++) {
    if (phase >= SKY[i].t && phase <= SKY[i + 1].t) { a = SKY[i]; b = SKY[i + 1]; break; }
  }
  const f = (phase - a.t) / Math.max(1e-6, b.t - a.t);
  const lerpC = (x, y) => new THREE.Color(x).lerp(new THREE.Color(y), f);
  return { sky: lerpC(a.sky, b.sky), sun: lerpC(a.sun, b.sun), sunI: a.sunI + (b.sunI - a.sunI) * f, hemi: a.hemi + (b.hemi - a.hemi) * f };
}

function makeGrassTexture(createElement) {
  const c = createElement('canvas'); c.width = c.height = 256;
  const ctx = c.getContext('2d');
  if (ctx) {
    ctx.fillStyle = '#7cbc66'; ctx.fillRect(0, 0, 256, 256);
    for (let i = 0; i < 900; i++) {
      const g = 150 + Math.random() * 60;
      ctx.fillStyle = `rgba(${100 + Math.random() * 40 | 0},${g},${70 + Math.random() * 40 | 0},0.35)`;
      ctx.fillRect(Math.random() * 256, Math.random() * 256, 2, 2 + Math.random() * 3);
    }
  }
  const tex = new THREE.CanvasTexture(c);
  tex.wrapS = tex.wrapT = THREE.RepeatWrapping; tex.repeat.set(8, 8);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}

/**
 * 柔边云影材质：多个椭圆高斯场叠成不规则云团，边缘在片元着色器内渐隐。
 * 这样仍保留低多边形云体，但地面不会出现 CircleGeometry 的硬折线轮廓。
 */
function makeCloudShadowMaterial(seed, opacity) {
  return new THREE.ShaderMaterial({
    transparent: true,
    depthWrite: false,
    toneMapped: false,
    uniforms: {
      uColor: { value: new THREE.Color(0x315f3a) },
      uOpacity: { value: opacity },
      uSeed: { value: seed },
    },
    vertexShader: `
      varying vec2 vUv;
      void main() {
        vUv = uv;
        gl_Position = projectionMatrix * modelViewMatrix * vec4(position, 1.0);
      }
    `,
    fragmentShader: `
      varying vec2 vUv;
      uniform vec3 uColor;
      uniform float uOpacity;
      uniform float uSeed;

      float softLobe(vec2 point, vec2 center, vec2 radius) {
        vec2 q = (point - center) / radius;
        return exp(-dot(q, q) * 2.6);
      }

      void main() {
        vec2 drift = vec2(
          sin(uSeed * 2.17) * 0.035,
          cos(uSeed * 1.73) * 0.025
        );
        float field = softLobe(vUv, vec2(0.30, 0.51) + drift, vec2(0.30, 0.31));
        field = max(field, softLobe(vUv, vec2(0.48, 0.43) - drift, vec2(0.32, 0.35)));
        field = max(field, softLobe(vUv, vec2(0.67, 0.51) + drift.yx, vec2(0.28, 0.29)));
        field = max(field, softLobe(vUv, vec2(0.53, 0.62) - drift.yx, vec2(0.38, 0.25)));

        float alpha = smoothstep(0.035, 0.72, field) * uOpacity;
        if (alpha < 0.001) discard;
        gl_FragColor = vec4(uColor, alpha);
      }
    `,
  });
}

function defaultEnv() {
  return {
    createRenderer: () => new THREE.WebGLRenderer({ antialias: true }),
    createControls: (camera, dom) => new OrbitControls(camera, dom),
    requestAnimationFrame: (cb) => requestAnimationFrame(cb),
    cancelAnimationFrame: (id) => cancelAnimationFrame(id),
    addEventListener: (type, fn) => addEventListener(type, fn),
    removeEventListener: (type, fn) => removeEventListener(type, fn),
    createElement: (tag) => document.createElement(tag),
    random: Math.random,
    getViewport: () => ({
      width: innerWidth,
      height: innerHeight,
      pixelRatio: typeof devicePixelRatio === 'number' ? devicePixelRatio : 1,
    }),
  };
}

export class FarmScene {
  constructor(container, env = {}) {
    this.container = container;
    this.env = { ...defaultEnv(), ...env };
    this._disposed = false;
    this._running = false;
    this._rafId = null;

    const vp = this.env.getViewport();
    this.renderer = this.env.createRenderer();
    this.renderer.setPixelRatio(Math.min(vp.pixelRatio, 2));
    this.renderer.setSize(vp.width, vp.height);
    this.renderer.shadowMap.enabled = true;
    this.renderer.shadowMap.type = THREE.PCFSoftShadowMap;
    this.renderer.toneMapping = THREE.ACESFilmicToneMapping;
    this.renderer.toneMappingExposure = 1.05;
    container.appendChild(this.renderer.domElement);

    this.scene = new THREE.Scene();
    this.scene.background = new THREE.Color(0x8fd3ff);
    this.scene.fog = new THREE.Fog(0x8fd3ff, 55, 130);

    this.camera = new THREE.PerspectiveCamera(42, vp.width / vp.height, 0.1, 300);
    this.camera.position.set(2, 24, 26);

    this.controls = this.env.createControls(this.camera, this.renderer.domElement);
    this.controls.target.set(0, 0.5, 0);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.08;
    this.controls.minDistance = 13;
    this.controls.maxDistance = 48;
    this.controls.maxPolarAngle = Math.PI * 0.42;
    this.controls.minPolarAngle = Math.PI * 0.16;
    this.controls.enablePan = false;

    // 光照
    this.hemi = new THREE.HemisphereLight(0xcfe8ff, 0x7a9a5c, 0.8);
    this.scene.add(this.hemi);
    this.sun = new THREE.DirectionalLight(0xfff3d6, 1.2);
    this.sun.castShadow = true;
    this.sun.shadow.mapSize.set(2048, 2048);
    const sc = this.sun.shadow.camera;
    sc.left = -30; sc.right = 30; sc.top = 30; sc.bottom = -30; sc.far = 120;
    this.sun.shadow.bias = -0.0008;
    this.sun.shadow.intensity = DAY_SHADOW_INTENSITY;
    this.scene.add(this.sun, this.sun.target);

    this.plotGroups = [];
    this.animated = [];      // 每帧动画回调
    this.particles = [];
    this.hoverRing = null;
    this.dogGroup = null;
    this.dogBehavior = null;
    this.dayPhase = 0.35;

    this.buildEnvironment();
    this.buildPlotsBase();
    this.setupPicking();

    this._onResize = () => {
      if (this._disposed || !this.renderer) return;
      const size = this.env.getViewport();
      this.camera.aspect = size.width / size.height;
      this.camera.updateProjectionMatrix();
      this.renderer.setSize(size.width, size.height);
    };
    this.env.addEventListener('resize', this._onResize);
  }

  // ---------------- 环境 ----------------
  buildEnvironment() {
    // 地面
    const ground = new THREE.Mesh(
      new THREE.PlaneGeometry(160, 160),
      new THREE.MeshLambertMaterial({ map: makeGrassTexture(this.env.createElement) })
    );
    ground.rotation.x = -Math.PI / 2;
    ground.receiveShadow = true;
    this.scene.add(ground);

    // 栅栏
    const fence = new THREE.Group();
    const fx = 17.5, fz = 10.5;
    const addPost = (x, z) => {
      const p = new THREE.Mesh(new THREE.BoxGeometry(0.22, 1.1, 0.22), mat(0xa9825a));
      p.position.set(x, 0.55, z); p.castShadow = true; fence.add(p);
    };
    const addRail = (x, z, len, rot) => {
      const r = new THREE.Mesh(new THREE.BoxGeometry(len, 0.12, 0.1), mat(0xbd9268));
      r.position.set(x, 0.75, z); r.rotation.y = rot; r.castShadow = true; fence.add(r);
    };
    for (let x = -fx; x <= fx; x += 2.5) { addPost(x, -fz); addPost(x, fz); }
    for (let z = -fz; z <= fz; z += 2.5) { addPost(-fx, z); addPost(fx, z); }
    addRail(0, -fz, fx * 2 + 0.4, 0); addRail(0, fz, fx * 2 + 0.4, 0);
    addRail(-fx, 0, fz * 2 + 0.4, Math.PI / 2); addRail(fx, 0, fz * 2 + 0.4, Math.PI / 2);
    this.scene.add(fence);

    // 小屋
    const house = new THREE.Group();
    const walls = new THREE.Mesh(new THREE.BoxGeometry(4.4, 3, 3.6), mat(0xf0e0c8));
    walls.position.y = 1.5; walls.castShadow = true; house.add(walls);
    const roof = new THREE.Mesh(new THREE.ConeGeometry(3.6, 2.2, 4), mat(0xc96f4a));
    roof.position.y = 4.1; roof.rotation.y = Math.PI / 4; roof.castShadow = true; house.add(roof);
    const door = new THREE.Mesh(new THREE.BoxGeometry(0.9, 1.6, 0.1), mat(0x8d6e63));
    door.position.set(0.6, 0.8, 1.85); house.add(door);
    const win = new THREE.Mesh(new THREE.BoxGeometry(0.9, 0.9, 0.1), mat(0xffe9a8, 0xffd970));
    win.position.set(-1.1, 1.8, 1.85); house.add(win);
    const chimney = new THREE.Mesh(new THREE.BoxGeometry(0.5, 1.2, 0.5), mat(0xb08968));
    chimney.position.set(1.3, 4.6, 0); house.add(chimney);
    house.position.set(-23, 0, -8);
    house.rotation.y = 0.5;
    this.scene.add(house);
    this.houseWindow = win;

    // 风车
    const mill = new THREE.Group();
    const tower = new THREE.Mesh(new THREE.CylinderGeometry(0.9, 1.4, 6, 6), mat(0xe8d5b7));
    tower.position.y = 3; tower.castShadow = true; mill.add(tower);
    const cap = new THREE.Mesh(new THREE.ConeGeometry(1.3, 1.2, 6), mat(0xc96f4a));
    cap.position.y = 6.6; mill.add(cap);
    this.blades = new THREE.Group();
    for (let i = 0; i < 4; i++) {
      const blade = new THREE.Mesh(new THREE.BoxGeometry(0.28, 3.4, 0.08), mat(0xf5efe0));
      blade.position.y = 1.7;
      const holder = new THREE.Group();
      holder.add(blade); holder.rotation.z = (i * Math.PI) / 2;
      this.blades.add(holder);
    }
    this.blades.position.set(0, 5.9, 1.6);
    mill.add(this.blades);
    mill.position.set(23, 0, -10);
    mill.rotation.y = -0.5;
    this.scene.add(mill);

    // 装饰树
    const treeAt = (x, z, s = 1) => {
      const t = new THREE.Group();
      const trunk = new THREE.Mesh(new THREE.CylinderGeometry(0.18 * s, 0.28 * s, 1.6 * s, 5), mat(0x8d6e63));
      trunk.position.y = 0.8 * s; trunk.castShadow = true; t.add(trunk);
      const c1 = new THREE.Mesh(new THREE.IcosahedronGeometry(1.3 * s, 0), mat(0x58a05a));
      c1.position.y = 2.4 * s; c1.castShadow = true; t.add(c1);
      const c2 = new THREE.Mesh(new THREE.IcosahedronGeometry(0.9 * s, 0), mat(0x69b45f));
      c2.position.set(0.7 * s, 1.9 * s, 0.4 * s); c2.castShadow = true; t.add(c2);
      t.position.set(x, 0, z);
      this.scene.add(t);
    };
    treeAt(-24, 6, 1.2); treeAt(24, 8, 1); treeAt(-20, 13, 0.9); treeAt(20, -14, 1.3); treeAt(-26, -14, 1);

    // 石头与花
    for (let i = 0; i < 10; i++) {
      const rock = new THREE.Mesh(new THREE.IcosahedronGeometry(0.25 + Math.random() * 0.3, 0), mat(0xb8b0a4));
      rock.position.set((Math.random() - 0.5) * 44, 0.15, (Math.random() < 0.5 ? -1 : 1) * (12 + Math.random() * 5));
      rock.castShadow = true; this.scene.add(rock);
    }
    for (let i = 0; i < 16; i++) {
      const f = new THREE.Mesh(new THREE.IcosahedronGeometry(0.14, 0), mat([0xff8fa3, 0xffd166, 0xffffff, 0xc77dff][i % 4]));
      f.position.set((Math.random() - 0.5) * 40, 0.14, (Math.random() < 0.5 ? -1 : 1) * (11.5 + Math.random() * 4));
      this.scene.add(f);
    }

    // 云
    this.clouds = [];
    this.cloudShadows = [];
    const cloudMat = new THREE.MeshLambertMaterial({ color: 0xffffff, transparent: true, opacity: 0.92 });
    for (let i = 0; i < 5; i++) {
      const cloud = new THREE.Group();
      for (let j = 0; j < 4; j++) {
        const b = new THREE.Mesh(new THREE.SphereGeometry(1.6 + Math.random() * 1.4, 7, 6), cloudMat);
        b.position.set(j * 2 - 3 + Math.random(), Math.random() * 0.8, Math.random() * 1.5);
        b.scale.y = 0.55; cloud.add(b);
      }
      cloud.position.set((Math.random() - 0.5) * 90, 16 + Math.random() * 6, -30 - Math.random() * 20);
      cloud.userData.speed = 0.2 + Math.random() * 0.3;
      const shadowOpacity = CLOUD_SHADOW_MIN_OPACITY + Math.random() * CLOUD_SHADOW_OPACITY_RANGE;
      const shadow = new THREE.Mesh(
        new THREE.PlaneGeometry(1, 1),
        makeCloudShadowMaterial(i + Math.random(), shadowOpacity)
      );
      shadow.rotation.x = -Math.PI / 2;
      shadow.scale.set(14 + Math.random() * 7, 6 + Math.random() * 3, 1);
      shadow.userData.baseOpacity = shadowOpacity;
      shadow.userData.offsetX = -5 + Math.random() * 4;
      shadow.position.set(
        cloud.position.x + shadow.userData.offsetX,
        0.035,
        -20 + Math.random() * 40
      );
      shadow.renderOrder = 1;
      cloud.userData.shadow = shadow;
      this.cloudShadows.push(shadow);
      this.clouds.push(cloud); this.scene.add(cloud);
      this.scene.add(shadow);
    }

    // 蝴蝶
    this.butterflies = [];
    const wingGeo = new THREE.PlaneGeometry(0.28, 0.2);
    for (let i = 0; i < 3; i++) {
      const b = new THREE.Group();
      const wmat = new THREE.MeshBasicMaterial({ color: [0xffb3d9, 0xa8e6ff, 0xfff3a8][i], side: THREE.DoubleSide });
      const w1 = new THREE.Mesh(wingGeo, wmat); w1.position.x = -0.13;
      const w2 = new THREE.Mesh(wingGeo, wmat); w2.position.x = 0.13;
      b.add(w1, w2); b.userData = { w1, w2, seed: i * 2.1 };
      this.butterflies.push(b); this.scene.add(b);
    }

    // 星星（夜晚可见）
    const starGeo = new THREE.BufferGeometry();
    const starPos = new Float32Array(300 * 3);
    for (let i = 0; i < 300; i++) {
      const a = Math.random() * Math.PI * 2, e = Math.random() * Math.PI * 0.45 + 0.15;
      starPos[i * 3] = Math.cos(a) * Math.cos(e) * 120;
      starPos[i * 3 + 1] = Math.sin(e) * 120;
      starPos[i * 3 + 2] = Math.sin(a) * Math.cos(e) * 120;
    }
    starGeo.setAttribute('position', new THREE.BufferAttribute(starPos, 3));
    this.stars = new THREE.Points(starGeo, new THREE.PointsMaterial({ color: 0xffffff, size: 0.5, transparent: true, opacity: 0 }));
    this.scene.add(this.stars);

    // 光尘微粒
    const dustGeo = new THREE.BufferGeometry();
    const dustPos = new Float32Array(50 * 3);
    for (let i = 0; i < 50; i++) {
      dustPos[i * 3] = (Math.random() - 0.5) * 34;
      dustPos[i * 3 + 1] = Math.random() * 5;
      dustPos[i * 3 + 2] = (Math.random() - 0.5) * 22;
    }
    dustGeo.setAttribute('position', new THREE.BufferAttribute(dustPos, 3));
    this.dust = new THREE.Points(dustGeo, new THREE.PointsMaterial({ color: 0xfff6c9, size: 0.14, transparent: true, opacity: 0.6 }));
    this.scene.add(this.dust);
  }

  // ---------------- 地块 ----------------
  buildPlotsBase() {
    for (let i = 0; i < COLS * ROWS; i++) {
      const g = new THREE.Group();
      const { x, z } = plotPos(i);
      g.position.set(x, 0, z);

      // 半埋式低矮菜畦：恢复真实厚度与阴影，但用圆润斜边避免“积木台座”。
      const rim = new THREE.Mesh(new RoundedBoxGeometry(4.3, PLOT_RIM_HEIGHT, 4.3, 2, 0.08), mat(0x8b6e50));
      rim.position.y = PLOT_RIM_CENTER_Y;
      rim.receiveShadow = true;
      rim.castShadow = true;
      g.add(rim);
      g.userData.rim = rim;

      // 顶部土壤保持平整，覆盖在低矮土畦上并承担拾取。
      const base = new THREE.Mesh(new THREE.PlaneGeometry(4.08, 4.08), mat(0xb5977a));
      base.rotation.x = -Math.PI / 2;
      base.position.y = PLOT_SURFACE_Y;
      base.receiveShadow = true; base.castShadow = false;
      base.userData.plotId = i;
      g.add(base);
      g.userData.base = base;

      // 翻耕后的圆角土垄：保留轻微起伏，厚度远低于旧版方块。
      const furrows = new THREE.Group();
      for (let r = -1; r <= 1; r++) {
        const f = new THREE.Mesh(new RoundedBoxGeometry(3.78, PLOT_FURROW_HEIGHT, 0.68, 1, 0.025), mat(0x684936));
        f.position.set(0, PLOT_FURROW_Y, r * 1.22);
        f.receiveShadow = true;
        f.castShadow = true;
        furrows.add(f);
      }
      furrows.visible = false;
      g.add(furrows);
      g.userData.furrows = furrows;

      // 悬停高亮环
      const ring = new THREE.Mesh(
        new THREE.TorusGeometry(2.75, 0.1, 6, 4),
        new THREE.MeshBasicMaterial({ color: 0xffe28a, transparent: true, opacity: 0.95 })
      );
      ring.rotation.x = -Math.PI / 2;
      ring.rotation.z = Math.PI / 4;
      ring.position.y = 0.29;
      ring.visible = false;
      g.add(ring);
      g.userData.ring = ring;

      // 成熟光环
      const halo = new THREE.Mesh(
        new THREE.TorusGeometry(1.9, 0.07, 6, 32),
        new THREE.MeshBasicMaterial({ color: 0xffd54f, transparent: true, opacity: 0.85 })
      );
      halo.rotation.x = -Math.PI / 2;
      halo.position.y = 0.27;
      halo.visible = false;
      g.add(halo);
      g.userData.halo = halo;

      // 内容容器（作物/杂草/害虫等）
      const content = new THREE.Group();
      content.position.y = PLOT_SURFACE_Y + 0.01;
      g.add(content);
      g.userData.content = content;
      g.userData.key = '';

      // 锁定牌
      const sign = new THREE.Group();
      const post = new THREE.Mesh(new THREE.BoxGeometry(0.12, 0.8, 0.12), mat(0xa9825a));
      post.position.y = 0.4; sign.add(post);
      const board = new THREE.Mesh(new THREE.BoxGeometry(1.7, 0.8, 0.08), mat(0xbd9268));
      board.position.y = 0.95; sign.add(board);
      g.add(sign);
      g.userData.sign = sign;
      g.userData.signBoard = board;

      this.scene.add(g);
      this.plotGroups.push(g);
    }
  }

  signTexture(text) {
    const c = this.env.createElement('canvas'); c.width = 128; c.height = 64;
    const ctx = c.getContext('2d');
    if (ctx) {
      ctx.fillStyle = '#bd9268'; ctx.fillRect(0, 0, 128, 64);
      ctx.fillStyle = '#5d4023'; ctx.font = 'bold 34px sans-serif';
      ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
      ctx.fillText(text, 64, 34);
    }
    const tex = new THREE.CanvasTexture(c);
    tex.colorSpace = THREE.SRGBColorSpace;
    return tex;
  }

  /**
   * 同步单个地块视觉。info: { unlocked, lockText, state, cropDef, stage, totalStages, dry, weed, pest }
   */
  updatePlot(g, info) {
    const u = g.userData;
    const key = JSON.stringify(info);
    if (u.key === key) return;
    u.key = key;
    u.pestGroup = null; u.cropGroup = null;

    // 锁定牌
    u.sign.visible = !info.unlocked && info.lockText !== '';
    if (!info.unlocked) {
      if (info.lockText && u.signText !== info.lockText) {
        u.signText = info.lockText;
        const prev = u.signBoard.material;
        u.signBoard.material = new THREE.MeshLambertMaterial({ map: this.signTexture(info.lockText) });
        disposeExclusiveMaterial(prev, disposeOpts);
      }
      u.base.material = mat(0x86a874);
      u.rim.material = mat(0x5f864f);
      u.furrows.visible = false;
      clearAndDispose(u.content, disposeOpts);
      u.halo.visible = false;
      return;
    }
    // 土壤颜色：荒地浅 / 已翻深 / 缺水更浅；未知状态不伪装成可操作土地。
    if (info.state === PLOT.UNKNOWN) {
      u.base.material = mat(0x6c6470);
      u.rim.material = mat(0x443f49);
    } else if (info.state === PLOT.WASTELAND) {
      u.base.material = mat(0xb99a7d);
      u.rim.material = mat(0x8b6e50);
    } else if (info.dry) {
      u.base.material = mat(0xc9b291);
      u.rim.material = mat(0x997452);
    } else {
      u.base.material = mat(0x79543e);
      u.rim.material = mat(0x513727);
    }
    u.furrows.visible = info.state !== PLOT.WASTELAND && info.state !== PLOT.UNKNOWN;

    // 内容重建
    clearAndDispose(u.content, disposeOpts);
    u.halo.visible = false;
    if (info.state === PLOT.GROWING || info.state === PLOT.MATURE) {
      const crop = createCropModel(info.cropDef, {
        stage: info.stage, totalStages: info.totalStages, mature: info.state === PLOT.MATURE,
      });
      u.content.add(crop);
      u.cropGroup = crop;
      if (info.weed) { const w = createWeedModel(); w.position.set(1.1, 0, 0.9); u.content.add(w); }
      if (info.weed) { const w2 = createWeedModel(); w2.position.set(-1.0, 0, -0.8); w2.scale.setScalar(0.7); u.content.add(w2); }
      if (info.pest) { const p = createPestModel(); p.position.y = 1.0; u.content.add(p); u.pestGroup = p; }
      else u.pestGroup = null;
      if (info.state === PLOT.MATURE) u.halo.visible = true;
    } else if (info.state === PLOT.WITHERED) {
      if (info.cropDef) {
        const dead = createCropModel(info.cropDef, { stage: 2, totalStages: 3, mature: true, withered: true });
        u.content.add(dead);
      } else {
        // 服务端清空 crop_id 的枯萎地块：无作物定义，渲染为通用枯萎残株，不伪造具体作物
        u.content.add(createResidueModel());
      }
    } else if (info.state === PLOT.RESIDUE) {
      u.content.add(createResidueModel());
    } else if (info.state === PLOT.WASTELAND) {
      // 荒地上的野草石
      const w = createWeedModel(); w.scale.setScalar(0.6); w.position.set(0.8, 0, -0.6); u.content.add(w);
      const r = new THREE.Mesh(new THREE.IcosahedronGeometry(0.2, 0), mat(0xa79c8e));
      r.position.set(-0.9, 0.1, 0.7); u.content.add(r);
    }
  }

  forEachPlot(fn) { this.plotGroups.forEach(fn); }

  // ---------------- 狗 ----------------
  setDog(dogDef, hungry) {
    if (!dogDef) {
      this._disposeDog();
      return;
    }
    if (!this.dogGroup || this.dogGroup.userData.dogId !== dogDef.id) {
      this._disposeDog();
      this.dogGroup = createDogModel(dogDef);
      this.dogGroup.userData.dogId = dogDef.id;
      this.dogBehavior = new DogBehaviorController({ random: this.env.random });
      this.dogBehavior.attach(this.dogGroup);
      this.scene.add(this.dogGroup);
    }
    this.dogGroup.userData.hungry = hungry;
  }

  _disposeDog() {
    if (!this.dogGroup) return;
    this.scene.remove(this.dogGroup);
    disposeObject3D(this.dogGroup, disposeOpts);
    this.dogGroup = null;
    this.dogBehavior = null;
  }

  // ---------------- 拾取 ----------------
  setupPicking() {
    this.raycaster = new THREE.Raycaster();
    this.pointer = new THREE.Vector2();
    this.clickCb = null; this.hoverCb = null;
    let downPos = null;
    const el = this.renderer.domElement;

    this._onPointerDown = (e) => { downPos = [e.clientX, e.clientY]; };
    this._onPointerUp = (e) => {
      if (!downPos) return;
      const moved = Math.hypot(e.clientX - downPos[0], e.clientY - downPos[1]);
      downPos = null;
      if (moved > 6) return;  // 拖拽视角不触发点击
      const hit = this.pick(e);
      if (hit !== null && this.clickCb) this.clickCb(hit);
    };
    this._onPointerMove = (e) => {
      const hit = this.pick(e);
      this.plotGroups.forEach(g => { g.userData.ring.visible = g.userData.base.userData.plotId === hit; });
      el.style.cursor = hit !== null ? 'pointer' : 'grab';
      if (this.hoverCb) this.hoverCb(hit, e.clientX, e.clientY);
    };
    el.addEventListener('pointerdown', this._onPointerDown);
    el.addEventListener('pointerup', this._onPointerUp);
    el.addEventListener('pointermove', this._onPointerMove);
  }

  pick(e) {
    const vp = this.env.getViewport();
    this.pointer.set((e.clientX / vp.width) * 2 - 1, -(e.clientY / vp.height) * 2 + 1);
    this.raycaster.setFromCamera(this.pointer, this.camera);
    const bases = this.plotGroups.map(g => g.userData.base);
    const hits = this.raycaster.intersectObjects(bases, false);
    return hits.length ? hits[0].object.userData.plotId : null;
  }

  // ---------------- 粒子 ----------------
  burst(plotId, color, count = 18, up = true) {
    const { x, z } = plotPos(plotId);
    for (let i = 0; i < count; i++) {
      const size = 0.08 + Math.random() * 0.1;
      const p = new THREE.Mesh(new THREE.SphereGeometry(size, 4, 3),
        new THREE.MeshBasicMaterial({ color, transparent: true }));
      p.position.set(x + (Math.random() - 0.5) * 2.4, up ? 0.8 + Math.random() : 3.5 + Math.random(), z + (Math.random() - 0.5) * 2.4);
      p.userData = {
        vx: (Math.random() - 0.5) * 1.4, vz: (Math.random() - 0.5) * 1.4,
        vy: up ? 1.5 + Math.random() * 1.6 : -(2 + Math.random() * 2),
        life: 1,
      };
      this.particles.push(p); this.scene.add(p);
    }
  }
  waterAnim(id) { this.burst(id, 0x5cb3ff, 22, false); }
  harvestAnim(id) { this.burst(id, 0xffd54f, 20, true); }
  magicAnim(id) { this.burst(id, 0xc77dff, 16, true); }

  // ---------------- 日夜 ----------------
  setDayPhase(phase) {
    this.dayPhase = phase;
    const k = lerpKey(phase);
    this.scene.background = k.sky;
    this.scene.fog.color.copy(k.sky);
    this.sun.color.copy(k.sun);
    this.sun.intensity = k.sunI;
    this.hemi.intensity = k.hemi;

    const a = phase * Math.PI * 2 - Math.PI / 2;   // phase 0.25 ≈ 正午
    const h = Math.sin(a), r = 55;
    // 夜晚月亮保持在空中，避免方向光沉入地下
    this.sun.position.set(Math.cos(a) * r * 0.6, (Math.abs(h) * 0.8 + 0.12) * r, 24);
    const night = h < -0.05;
    const shadowDaylight = THREE.MathUtils.smoothstep(h, -0.02, 0.55);
    // 场景没有独立月光源，黄昏后同步收掉实体硬阴影，避免夜间出现“太阳影子”。
    this.sun.shadow.intensity = DAY_SHADOW_INTENSITY * shadowDaylight;
    this.cloudShadows.forEach((shadow) => {
      shadow.material.uniforms.uOpacity.value = shadow.userData.baseOpacity * shadowDaylight;
    });
    this.stars.material.opacity = night ? Math.min(1, -h * 2.5) : 0;
    this.dust.material.opacity = night ? 0.15 : 0.65;
    this.butterflies.forEach(b => (b.visible = !night));
    this.houseWindow.material = mat(h < 0.12 ? 0xffe9a8 : 0xd8cfc0, h < 0.12 ? 0xffd970 : 0);
  }

  // ---------------- 主循环 ----------------
  start() {
    if (this._disposed || this._running) return;
    this._running = true;
    const clock = new THREE.Clock();
    const loop = () => {
      if (this._disposed || !this._running) {
        this._rafId = null;
        return;
      }
      this._rafId = this.env.requestAnimationFrame(loop);
      const dt = Math.min(clock.getDelta(), 0.05);
      const t = clock.elapsedTime;

      this.controls.update();
      this.blades.rotation.z += dt * 0.8;

      // 云漂移
      for (const c of this.clouds) {
        c.position.x += c.userData.speed * dt;
        if (c.position.x > 70) c.position.x = -70;
        if (c.userData.shadow) {
          c.userData.shadow.position.x = c.position.x + c.userData.shadow.userData.offsetX;
        }
      }
      // 蝴蝶
      for (const b of this.butterflies) {
        const s = b.userData.seed + t * 0.5;
        b.position.set(Math.sin(s) * 12 + Math.sin(s * 2.3) * 3, 1.6 + Math.sin(s * 3) * 0.7, Math.cos(s * 0.8) * 8);
        b.rotation.y = Math.atan2(Math.cos(s), -Math.sin(s * 0.8));
        const flap = Math.sin(t * 18 + b.userData.seed) * 0.9;
        b.userData.w1.rotation.y = flap; b.userData.w2.rotation.y = -flap;
      }
      // 狗使用本地行为状态机随机巡游、转向和休息。
      if (this.dogGroup && this.dogBehavior) {
        this.dogBehavior.update(this.dogGroup, dt, t, this.dogGroup.userData.hungry);
      }
      // 成熟光环呼吸 + 害虫环绕 + 作物摇摆
      for (const g of this.plotGroups) {
        const u = g.userData;
        if (u.halo.visible) {
          u.halo.scale.setScalar(1 + Math.sin(t * 3 + g.position.x) * 0.08);
          u.halo.material.opacity = 0.65 + Math.sin(t * 3) * 0.2;
        }
        if (u.pestGroup) {
          u.pestGroup.children.forEach((bug, i) => {
            bug.userData.angle += dt * 3;
            const a = bug.userData.angle;
            bug.position.set(Math.cos(a) * 0.5, 0.6 + Math.sin(t * 5 + i) * 0.2, Math.sin(a) * 0.5);
          });
        }
        if (u.cropGroup) u.cropGroup.rotation.y = Math.sin(t * 0.8 + g.position.x) * 0.03;
      }
      // 粒子
      for (let i = this.particles.length - 1; i >= 0; i--) {
        const p = this.particles[i];
        p.userData.life -= dt * 1.4;
        p.position.x += p.userData.vx * dt;
        p.position.y += p.userData.vy * dt;
        p.position.z += p.userData.vz * dt;
        p.material.opacity = Math.max(0, p.userData.life);
        if (p.userData.life <= 0) {
          this.scene.remove(p);
          p.geometry.dispose(); p.material.dispose();
          this.particles.splice(i, 1);
        }
      }
      // 光尘缓慢浮动
      this.dust.rotation.y = t * 0.02;

      this.renderer.render(this.scene, this.camera);
    };
    this._rafId = this.env.requestAnimationFrame(loop);
  }

  /**
   * 幂等释放：停 RAF、卸监听、释放 controls/renderer 与场景独占 GPU 资源。
   * 不 dispose crops.mat() 共享材质。
   */
  dispose() {
    if (this._disposed) return;
    this._disposed = true;
    this._running = false;
    if (this._rafId != null) {
      this.env.cancelAnimationFrame(this._rafId);
      this._rafId = null;
    }

    if (this._onResize) {
      this.env.removeEventListener('resize', this._onResize);
      this._onResize = null;
    }
    const el = this.renderer?.domElement;
    if (el) {
      if (this._onPointerDown) el.removeEventListener('pointerdown', this._onPointerDown);
      if (this._onPointerUp) el.removeEventListener('pointerup', this._onPointerUp);
      if (this._onPointerMove) el.removeEventListener('pointermove', this._onPointerMove);
    }
    this._onPointerDown = this._onPointerUp = this._onPointerMove = null;

    if (this.controls) {
      this.controls.dispose?.();
      this.controls = null;
    }

    for (const p of this.particles) {
      this.scene?.remove(p);
      disposeObject3D(p, disposeOpts);
    }
    this.particles = [];
    this._disposeDog();

    if (this.scene) {
      disposeObject3D(this.scene, disposeOpts);
      while (this.scene.children.length) this.scene.remove(this.scene.children[0]);
    }

    if (this.renderer) {
      if (this.container?.contains?.(this.renderer.domElement)) {
        this.container.removeChild(this.renderer.domElement);
      } else if (this.renderer.domElement?.parentNode === this.container) {
        this.container.removeChild(this.renderer.domElement);
      }
      this.renderer.renderLists?.dispose?.();
      this.renderer.dispose?.();
      this.renderer = null;
    }

    this.plotGroups = [];
    this.animated = [];
    this.clouds = [];
    this.cloudShadows = [];
    this.butterflies = [];
    this.stars = null;
    this.dust = null;
    this.blades = null;
    this.houseWindow = null;
    this.hoverRing = null;
    this.scene = null;
    this.camera = null;
    this.hemi = null;
    this.sun = null;
    this.raycaster = null;
    this.pointer = null;
    this.clickCb = null;
    this.hoverCb = null;
    this.container = null;
  }
}
