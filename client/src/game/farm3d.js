// ============================================================
// 3D 农场场景：低多边形田园 + 日夜循环 + 交互动画
// ============================================================
import * as THREE from 'three';
import { OrbitControls } from 'three/examples/jsm/controls/OrbitControls.js';
import { RoundedBoxGeometry } from 'three/examples/jsm/geometries/RoundedBoxGeometry.js';
import { mat, isSharedMaterial, createCropModel, createWeedModel, createPestModel, createResidueModel, createDogModel } from './crops.js';
import { DogBehaviorController } from './dogBehavior.js';
import { WildlifeController } from './wildlife.js';
import { clearAndDispose, disposeExclusiveMaterial, disposeObject3D } from './dispose3d.js';
import { batchStaticMeshes } from './renderOptimize.js';
import { MatureEffectPool } from './matureEffects.js';
import { PLOT } from './state.js';

const disposeOpts = { isSharedMaterial };

const COLS = 6, ROWS = 3, GAP = 5;
const DAY_SHADOW_INTENSITY = 0.72;
const CLOUD_SHADOW_MIN_OPACITY = 0.09;
const CLOUD_SHADOW_OPACITY_RANGE = 0.05;
const DOG_SCENE_SCALE = 0.72;
const BIRD_SCENE_SCALE = 0.6;
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

function freezeLocalTransform(object) {
  object.updateMatrix();
  object.matrixAutoUpdate = false;
  return object;
}

function createPlotResources() {
  return {
    rimGeometry: new RoundedBoxGeometry(4.3, PLOT_RIM_HEIGHT, 4.3, 2, 0.08),
    baseGeometry: new THREE.PlaneGeometry(4.08, 4.08),
    furrowGeometry: new RoundedBoxGeometry(3.78, PLOT_FURROW_HEIGHT, 0.68, 1, 0.025),
    ringGeometry: new THREE.TorusGeometry(2.75, 0.1, 6, 4),
    ringMaterial: new THREE.MeshBasicMaterial({ color: 0xffe28a, transparent: true, opacity: 0.95 }),
    signPostGeometry: new THREE.BoxGeometry(0.12, 0.8, 0.12),
    signBoardGeometry: new THREE.BoxGeometry(1.7, 0.8, 0.08),
  };
}

function optimizeStaticModel(model) {
  batchStaticMeshes(model, { pruneEmpty: true });
  return model;
}

// 成熟提示采用围绕作物上浮的金色闪耀与星芒，不再用覆盖整块土地的高亮圆环。
// 日夜关键色
const SKY = [
  { t: 0.00, sky: new THREE.Color(0x1b2a4a), sun: new THREE.Color(0x93a8e8), sunI: 0.55, hemi: 0.5 },  // 午夜
  { t: 0.20, sky: new THREE.Color(0x1b2a4a), sun: new THREE.Color(0x93a8e8), sunI: 0.55, hemi: 0.5 },
  { t: 0.28, sky: new THREE.Color(0xffb37e), sun: new THREE.Color(0xffcc88), sunI: 0.8,  hemi: 0.55 },  // 日出
  { t: 0.38, sky: new THREE.Color(0x8fd3ff), sun: new THREE.Color(0xfff3d6), sunI: 1.15, hemi: 0.8  },  // 上午
  { t: 0.55, sky: new THREE.Color(0x9adcff), sun: new THREE.Color(0xffffff), sunI: 1.25, hemi: 0.9  },  // 午后
  { t: 0.72, sky: new THREE.Color(0xffab6e), sun: new THREE.Color(0xffb066), sunI: 0.85, hemi: 0.58 },  // 黄昏
  { t: 0.80, sky: new THREE.Color(0x45548c), sun: new THREE.Color(0xa8b8f0), sunI: 0.58, hemi: 0.5 },
  { t: 1.00, sky: new THREE.Color(0x1b2a4a), sun: new THREE.Color(0x93a8e8), sunI: 0.55, hemi: 0.5 },
];

function lerpKey(phase, skyTarget, sunTarget) {
  let a = SKY[0], b = SKY[SKY.length - 1];
  for (let i = 0; i < SKY.length - 1; i++) {
    if (phase >= SKY[i].t && phase <= SKY[i + 1].t) { a = SKY[i]; b = SKY[i + 1]; break; }
  }
  const f = (phase - a.t) / Math.max(1e-6, b.t - a.t);
  skyTarget.lerpColors(a.sky, b.sky, f);
  sunTarget.lerpColors(a.sun, b.sun, f);
  return { sky: skyTarget, sun: sunTarget, sunI: a.sunI + (b.sunI - a.sunI) * f, hemi: a.hemi + (b.hemi - a.hemi) * f };
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

// 中央农田位于平缓盆地底部；超出建筑区后逐渐抬升，并叠加低频丘陵起伏。
function terrainHeight(x, z) {
  const dx = Math.max(0, Math.abs(x) - 27) / 43;
  const dz = Math.max(0, Math.abs(z) - 21) / 49;
  const edge = Math.min(1, Math.hypot(dx, dz));
  const rise = edge * edge * (3 - 2 * edge);
  const undulation =
    Math.sin(x * 0.105) * 0.7 +
    Math.cos(z * 0.09) * 0.55 +
    Math.sin((x - z) * 0.055) * 0.45;
  return Math.max(0, rise * (5.8 + undulation));
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

// 叠加多组可平铺波形生成自然法线，避免规则正弦纹和贴图接缝。
function makeWaterNormalMap(size, phase) {
  const data = new Uint8Array(size * size * 4);
  const waves = [
    [1, 2, 0.42], [2, -3, 0.3], [4, 1, 0.22], [-3, 5, 0.18],
    [7, 4, 0.12], [9, -6, 0.09], [-13, 8, 0.065], [17, 11, 0.045],
  ];
  const tau = Math.PI * 2;
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const u = x / size;
      const v = y / size;
      let dx = 0;
      let dy = 0;
      waves.forEach(([fx, fy, amplitude], index) => {
        const angle = tau * (fx * u + fy * v) + phase * (index * 0.73 + 1);
        const slope = Math.cos(angle) * amplitude;
        dx += slope * fx;
        dy += slope * fy;
      });
      const nx = -dx * 0.16;
      const ny = -dy * 0.16;
      const invLength = 1 / Math.hypot(nx, ny, 1);
      const offset = (y * size + x) * 4;
      data[offset] = Math.round((nx * invLength * 0.5 + 0.5) * 255);
      data[offset + 1] = Math.round((ny * invLength * 0.5 + 0.5) * 255);
      data[offset + 2] = Math.round((invLength * 0.5 + 0.5) * 255);
      data[offset + 3] = 255;
    }
  }
  const texture = new THREE.DataTexture(data, size, size, THREE.RGBAFormat);
  texture.wrapS = texture.wrapT = THREE.RepeatWrapping;
  texture.needsUpdate = true;
  return texture;
}

function makeWaterMaterial(normalMap, detailNormalMap) {
  return new THREE.MeshPhysicalMaterial({
    color: 0xffffff,
    vertexColors: true,
    transparent: true,
    opacity: 0.76,
    depthWrite: false,
    roughness: 0.2,
    metalness: 0,
    ior: 1.333,
    transmission: 0.08,
    thickness: 0.35,
    clearcoat: 1,
    clearcoatRoughness: 0.1,
    normalMap,
    normalScale: new THREE.Vector2(0.24, 0.34),
    clearcoatNormalMap: detailNormalMap,
    clearcoatNormalScale: new THREE.Vector2(0.12, 0.18),
    side: THREE.DoubleSide,
  });
}

function defaultEnv() {
  return {
    createRenderer: () => new THREE.WebGLRenderer({ antialias: true, powerPreference: 'high-performance' }),
    createControls: (camera, dom) => new OrbitControls(camera, dom),
    requestAnimationFrame: (cb) => requestAnimationFrame(cb),
    cancelAnimationFrame: (id) => cancelAnimationFrame(id),
    requestIdleCallback: (cb) => typeof globalThis.requestIdleCallback === 'function'
      ? globalThis.requestIdleCallback(cb, { timeout: 1200 })
      : globalThis.setTimeout(cb, 0),
    cancelIdleCallback: (id) => typeof globalThis.cancelIdleCallback === 'function'
      ? globalThis.cancelIdleCallback(id)
      : globalThis.clearTimeout(id),
    addEventListener: (type, fn) => addEventListener(type, fn),
    removeEventListener: (type, fn) => removeEventListener(type, fn),
    createElement: (tag) => document.createElement(tag),
    random: Math.random,
    loadWildlifeAssets: typeof window !== 'undefined',
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
    this._elapsedTime = 0;
    this._shaderWarmupId = null;
    this._shaderWarmupPromise = null;
    this._wildlifeLoadId = null;
    this._postFirstFrameScheduled = false;

    const vp = this.env.getViewport();
    this.renderer = this.env.createRenderer();
    this.renderer.setPixelRatio(Math.min(vp.pixelRatio, 2));
    this.renderer.setSize(vp.width, vp.height);
    this.renderer.shadowMap.enabled = true;
    this.renderer.shadowMap.type = THREE.PCFSoftShadowMap;
    this.renderer.toneMapping = THREE.ACESFilmicToneMapping;
    this.renderer.toneMappingExposure = 1.05;
    container.appendChild(this.renderer.domElement);

    this._daySkyColor = new THREE.Color(0x8fd3ff);
    this._daySunColor = new THREE.Color(0xfff3d6);
    this._lightsOn = null;
    this.scene = new THREE.Scene();
    // Scene 本身永远保持单位矩阵，避免它每帧强制整棵树重算 matrixWorld。
    this.scene.matrixAutoUpdate = false;
    this.scene.background = this._daySkyColor;
    this.scene.fog = new THREE.Fog(0x8fd3ff, 65, 170);

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
    sc.left = -45; sc.right = 45; sc.top = 45; sc.bottom = -45; sc.far = 140;
    this.sun.shadow.bias = -0.0008;
    this.sun.shadow.intensity = DAY_SHADOW_INTENSITY;
    this.scene.add(this.sun, this.sun.target);

    this.plotGroups = [];
    this.animated = [];      // 每帧动画回调
    this.particles = [];
    this._particleMatrix = new THREE.Matrix4();
    this._fountainDropletMatrix = new THREE.Matrix4();
    this.hoverRing = null;
    this.dogGroup = null;
    this.dogBehavior = null;
    this.dayPhase = 0.35;
    this.staticBatchStats = null;
    this.plotResources = createPlotResources();
    this.matureEffectPool = null;

    this.buildEnvironment();
    this.buildPlotsBase();
    this.setupPicking();
    this.scene.updateMatrixWorld(true);

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
  scheduleShaderWarmup() {
    if (typeof this.renderer?.compileAsync !== 'function') return;
    this._shaderWarmupId = this.env.requestIdleCallback(() => {
      this._shaderWarmupId = null;
      if (this._disposed || !this.renderer || !this.scene || !this.camera) return;
      this._shaderWarmupPromise = this.renderer
        .compileAsync(this.scene, this.camera)
        .catch(() => null)
        .finally(() => { this._shaderWarmupPromise = null; });
    });
  }

  schedulePostFirstFrameWork() {
    if (this._disposed || this._postFirstFrameScheduled) return;
    this._postFirstFrameScheduled = true;
    this.scheduleShaderWarmup();
    if (!this.env.loadWildlifeAssets) return;
    this._wildlifeLoadId = this.env.requestIdleCallback(() => {
      this._wildlifeLoadId = null;
      if (this._disposed) return;
      void this.wildlife?.loadAssetReplacements();
    });
  }

  buildEnvironment() {
    // 盆地地形：中心平坦，外围缓慢抬升为丘陵。
    const groundGeo = new THREE.PlaneGeometry(160, 160, 64, 64);
    const groundPos = groundGeo.attributes.position;
    for (let i = 0; i < groundPos.count; i++) {
      const x = groundPos.getX(i);
      const z = -groundPos.getY(i);
      groundPos.setZ(i, terrainHeight(x, z));
    }
    groundPos.needsUpdate = true;
    groundGeo.computeVertexNormals();
    const ground = new THREE.Mesh(
      groundGeo,
      new THREE.MeshLambertMaterial({ map: makeGrassTexture(this.env.createElement) })
    );
    ground.rotation.x = -Math.PI / 2;
    ground.receiveShadow = true;
    this.scene.add(ground);

    // 远山：多环不规则山体，顶点偏移形成岩脊，顶层按高度混入积雪。
    const mountainVegetationSpots = [];
    const mountainShrubSpots = [];
    const mountainFootprints = [];
    const mountainSurfaceRaycaster = new THREE.Raycaster();
    const mountainRayDown = new THREE.Vector3(0, -1, 0);
    const mountainAt = (x, z, radius, height, color, seed, snow = false) => {
      const segments = 11;
      const rings = 5;
      const vertices = [];
      const colors = [];
      const indices = [];
      const baseColor = new THREE.Color(color);
      const addVertex = (vx, vy, vz, angleSeed) => {
        vertices.push(vx, vy, vz);
        const normalizedHeight = vy / height;
        const snowLine = 0.72 + Math.sin(angleSeed * 2.7 + seed) * 0.08;
        let vertexColor;
        if (snow && normalizedHeight > snowLine) {
          vertexColor = new THREE.Color(0xe6ece8).offsetHSL(0, 0, Math.sin(angleSeed + seed) * 0.035);
        } else {
          const lightness = Math.sin(angleSeed * 1.9 + seed * 0.7) * 0.07 - (1 - normalizedHeight) * 0.035;
          vertexColor = baseColor.clone().offsetHSL(0, 0, lightness);
        }
        colors.push(vertexColor.r, vertexColor.g, vertexColor.b);
      };
      const peakX = Math.sin(seed * 1.7) * radius * 0.12;
      const peakZ = Math.cos(seed * 1.3) * radius * 0.1;
      addVertex(peakX, height, peakZ, seed);
      for (let ring = 1; ring <= rings; ring++) {
        const fraction = ring / rings;
        const ringHeight = height * (1 - Math.pow(fraction, 1.28));
        for (let i = 0; i < segments; i++) {
          const angle = (i / segments) * Math.PI * 2;
          const radialNoise =
            1 +
            Math.sin((i + 1) * 2.17 + seed * 3.1 + ring) * 0.14 +
            Math.cos((i + 2) * 1.33 + seed + ring * 0.7) * 0.08;
          const r = radius * fraction * radialNoise;
          const yNoise = Math.sin(i * 2.4 + ring * 1.7 + seed) * height * 0.025 * (1 - fraction);
          addVertex(
            Math.cos(angle) * r + peakX * (1 - fraction),
            Math.max(0, ringHeight + yNoise),
            Math.sin(angle) * r + peakZ * (1 - fraction),
            angle + ring * 0.43,
          );
        }
      }
      for (let i = 0; i < segments; i++) {
        indices.push(0, 1 + ((i + 1) % segments), 1 + i);
      }
      for (let ring = 1; ring < rings; ring++) {
        const innerStart = 1 + (ring - 1) * segments;
        const outerStart = 1 + ring * segments;
        for (let i = 0; i < segments; i++) {
          const next = (i + 1) % segments;
          indices.push(
            innerStart + i, innerStart + next, outerStart + i,
            innerStart + next, outerStart + next, outerStart + i,
          );
        }
      }
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.Float32BufferAttribute(vertices, 3));
      geometry.setAttribute('color', new THREE.Float32BufferAttribute(colors, 3));
      geometry.setIndex(indices);
      geometry.computeVertexNormals();
      const mountain = new THREE.Mesh(
        geometry,
        new THREE.MeshStandardMaterial({
          vertexColors: true,
          flatShading: true,
          roughness: 0.97,
          metalness: 0,
          emissive: 0x15241a,
          emissiveIntensity: 0.28,
        }),
      );
      mountain.position.set(x, terrainHeight(x, z) - 0.25, z);
      mountain.rotation.y = seed * 0.61;
      mountain.castShadow = true;
      mountain.receiveShadow = true;
      this.scene.add(mountain);
      mountain.updateMatrixWorld(true);
      mountainFootprints.push({ x, z, radius });

      // 按真实山体网格向下投射落点；过陡或未命中的位置不再生成植被。
      const mountainBaseY = terrainHeight(x, z) - 0.25;
      const mountainRotation = seed * 0.61;
      const projectToMountainSurface = (localX, localZ, minNormalY) => {
        const candidate = mountain.localToWorld(new THREE.Vector3(localX, 0, localZ));
        mountainSurfaceRaycaster.set(
          new THREE.Vector3(candidate.x, mountainBaseY + height + 8, candidate.z),
          mountainRayDown,
        );
        const hit = mountainSurfaceRaycaster.intersectObject(mountain, false)[0];
        if (!hit?.face) return null;
        const normal = hit.face.normal.clone().applyNormalMatrix(
          new THREE.Matrix3().getNormalMatrix(mountain.matrixWorld),
        );
        return normal.y >= minNormalY ? hit.point : null;
      };
      const treeCount = height >= 20 ? 4 : 3;
      for (let i = 0; i < treeCount; i++) {
        const angle = 0.42 + (i / Math.max(1, treeCount - 1)) * 2.25;
        const fraction = 0.63 + ((i * 5 + Math.floor(seed * 3)) % 4) * 0.04;
        const localX = Math.cos(angle) * radius * fraction;
        const localZ = Math.sin(angle) * radius * fraction;
        const worldX = x + localX * Math.cos(mountainRotation) + localZ * Math.sin(mountainRotation);
        const worldZ = z - localX * Math.sin(mountainRotation) + localZ * Math.cos(mountainRotation);
        const surface = projectToMountainSurface(localX, localZ, 0.58);
        if (!surface) continue;
        const kind = (i + Math.floor(seed)) % 3 === 0 ? 'oak' : ((i + Math.floor(seed)) % 4 === 0 ? 'birch' : 'pine');
        const scale = 0.55 + ((i * 3 + Math.floor(seed)) % 4) * 0.07;
        mountainVegetationSpots.push([kind, worldX, worldZ, scale, surface.y + 0.08]);
      }
      for (let i = 0; i < 3; i++) {
        const angle = 0.7 + i * 0.82 + seed * 0.07;
        const fraction = 0.76 + i * 0.045;
        const localX = Math.cos(angle) * radius * fraction;
        const localZ = Math.sin(angle) * radius * fraction;
        const worldX = x + localX * Math.cos(mountainRotation) + localZ * Math.sin(mountainRotation);
        const worldZ = z - localX * Math.sin(mountainRotation) + localZ * Math.cos(mountainRotation);
        const surface = projectToMountainSurface(localX, localZ, 0.68);
        if (!surface) continue;
        mountainShrubSpots.push([worldX, worldZ, 0.58 + i * 0.08, surface.y + 0.12]);
      }
    };
    // 后排主峰与前排低矮山脊错位叠放，避免规则的一字排开。
    mountainAt(-70, -69, 23, 21, 0x647967, 1.2);
    mountainAt(-48, -72, 25, 24, 0x6b7e6d, 2.6, true);
    mountainAt(-25, -70, 23, 22, 0x607563, 3.8);
    mountainAt(13, -72, 27, 26, 0x687b6e, 5.1, true);
    mountainAt(40, -70, 24, 23, 0x5d7361, 6.4);
    mountainAt(68, -68, 26, 22, 0x667b68, 7.9);
    // 前排仅用低矮山肩，中央留出溪流源头的山谷。
    mountainAt(-58, -55, 18, 11, 0x748975, 10.4);
    mountainAt(-35, -57, 17, 13, 0x6d836c, 11.7);
    mountainAt(-16, -55, 16, 10, 0x748975, 12.9);
    mountainAt(22, -57, 18, 12, 0x6a806a, 14.3);
    mountainAt(46, -55, 17, 11, 0x758a72, 15.8);
    mountainAt(66, -57, 19, 12, 0x6c826b, 17.1);

    // 溪流：变宽河道、泥土外岸、卵石浅滩与水面流纹。
    const streamCurve = new THREE.CatmullRomCurve3([
      new THREE.Vector3(-5, 0, -67),
      new THREE.Vector3(-11, 0, -58),
      new THREE.Vector3(-6, 0, -49),
      new THREE.Vector3(-12, 0, -41),
      new THREE.Vector3(-7, 0, -33),
      new THREE.Vector3(-1, 0, -26),
      new THREE.Vector3(2, 0, -21.5),
    ]);
    const streamHalfWidth = (u) =>
      0.98 +
      u * 0.34 +
      Math.sin(u * Math.PI * 5.5) * 0.2 +
      Math.sin(u * Math.PI * 13 + 0.7) * 0.09;
    const makeStreamGeometry = (widthAt, lift) => {
      const samples = 64;
      const crossSegments = 4;
      const vertices = [];
      const uvs = [];
      const colors = [];
      const indices = [];
      const shallow = new THREE.Color(0x76b8ba);
      const deep = new THREE.Color(0x2f7483);
      for (let i = 0; i <= samples; i++) {
        const u = i / samples;
        const point = streamCurve.getPoint(u);
        const tangent = streamCurve.getTangent(u);
        const width = typeof widthAt === 'function' ? widthAt(u) : widthAt;
        const normalX = -tangent.z;
        const normalZ = tangent.x;
        const normalLength = Math.hypot(normalX, normalZ) || 1;
        const nx = normalX / normalLength;
        const nz = normalZ / normalLength;
        for (let j = 0; j <= crossSegments; j++) {
          const v = j / crossSegments;
          const side = v * 2 - 1;
          const x = point.x + nx * width * side;
          const z = point.z + nz * width * side;
          vertices.push(x, terrainHeight(x, z) + lift, z);
          uvs.push(u, v);
          const depth = Math.sin(v * Math.PI) ** 0.7;
          const color = shallow.clone().lerp(deep, depth * 0.88);
          colors.push(color.r, color.g, color.b);
        }
        if (i < samples) {
          const row = crossSegments + 1;
          for (let j = 0; j < crossSegments; j++) {
            const a = i * row + j;
            indices.push(a, a + 1, a + row, a + 1, a + row + 1, a + row);
          }
        }
      }
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.Float32BufferAttribute(vertices, 3));
      geometry.setAttribute('uv', new THREE.Float32BufferAttribute(uvs, 2));
      geometry.setAttribute('color', new THREE.Float32BufferAttribute(colors, 3));
      geometry.setIndex(indices);
      geometry.computeVertexNormals();
      return geometry;
    };
    const makeStreamRibbon = (widthAt, lift, material) => {
      const ribbon = new THREE.Mesh(makeStreamGeometry(widthAt, lift), material);
      ribbon.receiveShadow = true;
      this.scene.add(ribbon);
      return ribbon;
    };
    makeStreamRibbon((u) => streamHalfWidth(u) + 0.68, 0.025, new THREE.MeshLambertMaterial({ color: 0x716b50 }));
    makeStreamRibbon((u) => streamHalfWidth(u) + 0.34, 0.045, new THREE.MeshLambertMaterial({ color: 0xa49b79 }));
    const streamNormal = makeWaterNormalMap(256, 0.35);
    const streamDetailNormal = makeWaterNormalMap(128, 2.1);
    streamNormal.repeat.set(12, 1.7);
    streamDetailNormal.repeat.set(24, 2.8);
    this.stream = makeStreamRibbon(
      streamHalfWidth,
      0.085,
      makeWaterMaterial(streamNormal, streamDetailNormal),
    );
    this.streamCurve = streamCurve;
    this.streamRipples = [];
    this.waterNormalMaps = [streamNormal, streamDetailNormal];
    // 河岸卵石、香蒲与草丛交替分布，保持中间水道通畅。
    for (let i = 5; i < 61; i += 4) {
      const u = i / 64;
      const point = streamCurve.getPoint(u);
      const tangent = streamCurve.getTangent(u);
      const side = i % 8 === 1 ? -1 : 1;
      const nx = -tangent.z;
      const nz = tangent.x;
      const normalLength = Math.hypot(nx, nz) || 1;
      const bankOffset = streamHalfWidth(u) + 0.62 + (i % 3) * 0.16;
      const x = point.x + (nx / normalLength) * bankOffset * side;
      const z = point.z + (nz / normalLength) * bankOffset * side;
      const rock = new THREE.Mesh(
        new THREE.DodecahedronGeometry(0.22 + (i % 4) * 0.055, 0),
        mat(i % 3 === 0 ? 0x8c897d : 0xa6a294),
      );
      rock.position.set(x, terrainHeight(x, z) + 0.16, z);
      rock.scale.y = 0.55;
      rock.rotation.set(i * 0.17, i * 0.39, i * 0.11);
      rock.castShadow = true;
      this.scene.add(rock);
      if (i % 8 === 5) {
        for (let r = -1; r <= 1; r++) {
          const stemHeight = 0.62 + (r + 1) * 0.09;
          const stem = new THREE.Mesh(new THREE.CylinderGeometry(0.018, 0.025, stemHeight, 5), mat(0x5f823e));
          stem.position.set(x + r * 0.13, terrainHeight(x, z) + stemHeight / 2, z + Math.abs(r) * 0.08);
          stem.rotation.z = r * 0.06;
          stem.castShadow = true;
          this.scene.add(stem);
          const cattail = new THREE.Mesh(new THREE.CylinderGeometry(0.042, 0.042, 0.16, 6), mat(0x70452f));
          cattail.position.set(x + r * 0.13, terrainHeight(x, z) + stemHeight + 0.04, z + Math.abs(r) * 0.08);
          cattail.rotation.z = r * 0.06;
          cattail.castShadow = true;
          this.scene.add(cattail);
          const blade = new THREE.Mesh(new THREE.ConeGeometry(0.055, 0.42 + (r + 1) * 0.05, 4), mat(0x75984d));
          blade.position.set(x - r * 0.1, terrainHeight(x, z) + 0.22, z - 0.08);
          blade.rotation.z = -r * 0.15;
          this.scene.add(blade);
        }
      }
    }
    // 浅水中的露头石与岸边漂木。
    for (const [u, side, size] of [[0.24, -0.22, 0.3], [0.43, 0.28, 0.24], [0.61, -0.3, 0.34], [0.78, 0.18, 0.26]]) {
      const point = streamCurve.getPoint(u);
      const tangent = streamCurve.getTangent(u);
      const nx = -tangent.z;
      const nz = tangent.x;
      const normalLength = Math.hypot(nx, nz) || 1;
      const x = point.x + (nx / normalLength) * side;
      const z = point.z + (nz / normalLength) * side;
      const steppingStone = new THREE.Mesh(new THREE.DodecahedronGeometry(size, 0), mat(0x8f918b));
      steppingStone.position.set(x, terrainHeight(x, z) + 0.11, z);
      steppingStone.scale.y = 0.38;
      steppingStone.rotation.y = u * 9;
      steppingStone.castShadow = true;
      this.scene.add(steppingStone);
    }
    const logU = 0.52;
    const logPoint = streamCurve.getPoint(logU);
    const logTangent = streamCurve.getTangent(logU);
    const driftwood = new THREE.Mesh(new THREE.CylinderGeometry(0.07, 0.11, 1.45, 6), mat(0x71513c));
    driftwood.rotation.z = Math.PI / 2;
    driftwood.rotation.y = Math.atan2(logTangent.z, logTangent.x) + 0.45;
    driftwood.position.set(logPoint.x - 1.65, terrainHeight(logPoint.x - 1.65, logPoint.z) + 0.13, logPoint.z + 0.75);
    driftwood.castShadow = true;
    this.scene.add(driftwood);

    // 溪流末端汇入不规则林间水塘，避免河道以平直截面突然消失。
    const pondCenter = { x: 2.8, z: -20.2 };
    const makePondGeometry = (radiusX, radiusZ, lift) => {
      const segments = 20;
      const vertices = [pondCenter.x, terrainHeight(pondCenter.x, pondCenter.z) + lift, pondCenter.z];
      const uvs = [0.5, 0.5];
      const deep = new THREE.Color(0x326f7d);
      const shallow = new THREE.Color(0x78b8b4);
      const colors = [deep.r, deep.g, deep.b];
      const indices = [];
      for (let i = 0; i < segments; i++) {
        const angle = (i / segments) * Math.PI * 2;
        const irregularity =
          1 +
          Math.sin(i * 2.17 + 0.8) * 0.075 +
          Math.cos(i * 1.31 + 1.7) * 0.045;
        const x = pondCenter.x + Math.cos(angle) * radiusX * irregularity;
        const z = pondCenter.z + Math.sin(angle) * radiusZ * irregularity;
        vertices.push(x, terrainHeight(x, z) + lift, z);
        uvs.push(0.5 + Math.cos(angle) * 0.5, 0.5 + Math.sin(angle) * 0.5);
        colors.push(shallow.r, shallow.g, shallow.b);
        const next = (i + 1) % segments;
        indices.push(0, 1 + next, 1 + i);
      }
      const geometry = new THREE.BufferGeometry();
      geometry.setAttribute('position', new THREE.Float32BufferAttribute(vertices, 3));
      geometry.setAttribute('uv', new THREE.Float32BufferAttribute(uvs, 2));
      geometry.setAttribute('color', new THREE.Float32BufferAttribute(colors, 3));
      geometry.setIndex(indices);
      geometry.computeVertexNormals();
      return geometry;
    };
    const makePondLayer = (radiusX, radiusZ, lift, material) => {
      const pondLayer = new THREE.Mesh(makePondGeometry(radiusX, radiusZ, lift), material);
      pondLayer.receiveShadow = true;
      this.scene.add(pondLayer);
      return pondLayer;
    };
    makePondLayer(4.15, 3.15, 0.024, new THREE.MeshLambertMaterial({ color: 0x716b50 }));
    makePondLayer(3.65, 2.72, 0.044, new THREE.MeshLambertMaterial({ color: 0xa49b79 }));
    const pondNormal = makeWaterNormalMap(192, 1.15);
    const pondDetailNormal = makeWaterNormalMap(96, 3.4);
    pondNormal.repeat.set(3.2, 2.4);
    pondDetailNormal.repeat.set(6.4, 4.8);
    this.pond = makePondLayer(
      3.15,
      2.3,
      0.082,
      makeWaterMaterial(pondNormal, pondDetailNormal),
    );
    this.pond.renderOrder = 2;
    this.stream.renderOrder = 2;
    this.waterNormalMaps.push(pondNormal, pondDetailNormal);

    // 水塘中央的小型喷泉：石质底座、四向弧形水柱与循环飞溅水珠。
    const pondSurfaceY = terrainHeight(pondCenter.x, pondCenter.z) + 0.09;
    this.fountain = new THREE.Group();
    this.fountain.position.set(pondCenter.x, pondSurfaceY, pondCenter.z);
    this.fountain.scale.set(1.28, 1.35, 1.28);
    const fountainBase = new THREE.Mesh(
      new THREE.CylinderGeometry(0.68, 0.78, 0.2, 12),
      mat(0x858982),
    );
    fountainBase.position.y = 0.1;
    fountainBase.castShadow = true;
    fountainBase.receiveShadow = true;
    const fountainPedestal = new THREE.Mesh(
      new THREE.CylinderGeometry(0.31, 0.43, 0.36, 10),
      mat(0x9da098),
    );
    fountainPedestal.position.y = 0.36;
    fountainPedestal.castShadow = true;
    const fountainRim = new THREE.Mesh(
      new THREE.TorusGeometry(0.4, 0.085, 7, 18),
      mat(0xb0b1a9),
    );
    fountainRim.rotation.x = Math.PI / 2;
    fountainRim.position.y = 0.54;
    fountainRim.castShadow = true;
    const fountainNozzle = new THREE.Mesh(
      new THREE.CylinderGeometry(0.055, 0.1, 0.4, 8),
      mat(0x66736d),
    );
    fountainNozzle.position.y = 0.73;
    fountainNozzle.castShadow = true;
    this.fountain.add(fountainBase, fountainPedestal, fountainRim, fountainNozzle);

    const fountainWaterMaterial = new THREE.MeshPhysicalMaterial({
      color: 0x9edee9,
      emissive: 0x245d6a,
      emissiveIntensity: 0.1,
      roughness: 0.08,
      transmission: 0.25,
      transparent: true,
      opacity: 0.76,
      depthWrite: false,
    });
    this.fountainJet = new THREE.Mesh(
      new THREE.CylinderGeometry(0.03, 0.06, 1.25, 7),
      fountainWaterMaterial,
    );
    this.fountainJet.position.y = 1.25;
    this.fountain.add(this.fountainJet);

    for (let direction = 0; direction < 4; direction++) {
      const angle = direction * Math.PI / 2 + Math.PI / 4;
      const dx = Math.cos(angle);
      const dz = Math.sin(angle);
      const arc = new THREE.QuadraticBezierCurve3(
        new THREE.Vector3(dx * 0.08, 0.78, dz * 0.08),
        new THREE.Vector3(dx * 0.78, 1.62, dz * 0.78),
        new THREE.Vector3(dx * 1.45, 0.08, dz * 1.45),
      );
      const arcMesh = new THREE.Mesh(
        new THREE.TubeGeometry(arc, 14, 0.022, 5, false),
        fountainWaterMaterial,
      );
      this.fountain.add(arcMesh);
    }

    const dropletCount = 16;
    const dropletGeometry = new THREE.SphereGeometry(0.045, 6, 4);
    this.fountainDroplets = new THREE.InstancedMesh(
      dropletGeometry,
      fountainWaterMaterial,
      dropletCount,
    );
    this.fountainDroplets.name = 'fountain-droplets';
    this.fountainDroplets.frustumCulled = false;
    this.fountainDroplets.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
    const dropletAngles = new Array(dropletCount);
    const dropletPhases = new Array(dropletCount);
    let dropletIndex = 0;
    for (let direction = 0; direction < 4; direction++) {
      const angle = direction * Math.PI / 2 + Math.PI / 4;
      for (let i = 0; i < 4; i++) {
        dropletAngles[dropletIndex] = angle;
        dropletPhases[dropletIndex] = i / 4 + direction * 0.06;
        dropletIndex++;
      }
    }
    this.fountainDroplets.userData = { angles: dropletAngles, phases: dropletPhases };
    freezeLocalTransform(this.fountainDroplets);
    this.fountain.add(this.fountainDroplets);
    this.updateFountainDroplets(0);
    this.scene.add(this.fountain);

    // 水塘边缘用不规则卵石与成簇芦苇收口。
    for (let i = 0; i < 12; i++) {
      const angle = (i / 12) * Math.PI * 2 + 0.18;
      const radiusX = 3.55 + (i % 3) * 0.18;
      const radiusZ = 2.62 + ((i + 1) % 3) * 0.14;
      const x = pondCenter.x + Math.cos(angle) * radiusX;
      const z = pondCenter.z + Math.sin(angle) * radiusZ;
      const stone = new THREE.Mesh(
        new THREE.DodecahedronGeometry(0.2 + (i % 4) * 0.045, 0),
        mat(i % 2 === 0 ? 0x969388 : 0xb0aa9a),
      );
      stone.position.set(x, terrainHeight(x, z) + 0.14, z);
      stone.scale.y = 0.55;
      stone.rotation.set(i * 0.13, i * 0.41, i * 0.08);
      stone.castShadow = true;
      this.scene.add(stone);
      if (i % 4 === 1) {
        for (let r = -1; r <= 1; r++) {
          const reed = new THREE.Mesh(new THREE.ConeGeometry(0.045, 0.72 + (r + 1) * 0.08, 5), mat(0x5f823e));
          reed.position.set(x + r * 0.14, terrainHeight(x, z) + 0.38, z + Math.abs(r) * 0.08);
          reed.castShadow = true;
          this.scene.add(reed);
        }
      }
    }
    this.pondRipples = [];
    for (const [radius, opacity, phase] of [[0.88, 0.56, 0], [1.48, 0.38, Math.PI]]) {
      const pondRipple = new THREE.Mesh(
        new THREE.TorusGeometry(radius, 0.018, 4, 20),
        new THREE.MeshBasicMaterial({ color: 0xa7e5ee, transparent: true, opacity }),
      );
      pondRipple.rotation.x = Math.PI / 2;
      pondRipple.scale.y = 0.68;
      pondRipple.position.set(
        pondCenter.x,
        terrainHeight(pondCenter.x, pondCenter.z) + 0.115,
        pondCenter.z,
      );
      pondRipple.userData = { phase, baseOpacity: opacity };
      this.pondRipples.push(pondRipple);
      this.scene.add(pondRipple);
    }

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
    // 前栅栏在正门处留出入口
    for (let x = -fx; x <= fx; x += 2.5) { addPost(x, -fz); if (Math.abs(x) > 1.3) addPost(x, fz); }
    for (let z = -fz; z <= fz; z += 2.5) { addPost(-fx, z); addPost(fx, z); }
    addRail(0, -fz, fx * 2 + 0.4, 0);
    addRail(-9.5, fz, 16.4, 0); addRail(9.5, fz, 16.4, 0);
    addRail(-fx, 0, fz * 2 + 0.4, Math.PI / 2); addRail(fx, 0, fz * 2 + 0.4, Math.PI / 2);
    // 门柱：略高 + 圆球柱头
    for (const gx of [-1.3, 1.3]) {
      const gp = new THREE.Mesh(new THREE.BoxGeometry(0.28, 1.5, 0.28), mat(0x8d6e63));
      gp.position.set(gx, 0.75, fz); gp.castShadow = true; fence.add(gp);
      const capBall = new THREE.Mesh(new THREE.SphereGeometry(0.2, 6, 5), mat(0xbd9268));
      capBall.position.set(gx, 1.6, fz); capBall.castShadow = true; fence.add(capBall);
    }
    this.scene.add(fence);

    // 石板小路：正门蜿蜒到水井
    for (let z = 10.2; z >= -8.2; z -= 1.15) {
      const stone = new THREE.Mesh(new THREE.CylinderGeometry(0.3, 0.34, 0.08, 7), mat(0xb8b0a4));
      stone.position.set(Math.sin(z * 1.3) * 0.12, 0.045, z);
      stone.rotation.y = z * 1.7;
      stone.receiveShadow = true;
      this.scene.add(stone);
    }

    // 小屋：石基座 + 双坡屋顶 + 门窗框 + 烟囱炊烟 + 门廊灯
    const house = new THREE.Group();
    const foundation = new THREE.Mesh(new THREE.BoxGeometry(5.0, 0.5, 4.2), mat(0x9a9a92));
    foundation.position.y = 0.25; foundation.castShadow = true; house.add(foundation);
    const walls = new THREE.Mesh(new THREE.BoxGeometry(4.4, 3, 3.6), mat(0xf0e0c8));
    walls.position.y = 2.0; walls.castShadow = true; house.add(walls);
    // 山墙（屋顶下的三角封口）
    const gableShape = new THREE.Shape();
    gableShape.moveTo(-2.2, 0); gableShape.lineTo(2.2, 0); gableShape.lineTo(0, 1.5); gableShape.closePath();
    const gableGeo = new THREE.ExtrudeGeometry(gableShape, { depth: 0.12, bevelEnabled: false });
    for (const sz of [-1, 1]) {
      const gable = new THREE.Mesh(gableGeo, mat(0xf0e0c8));
      gable.position.set(0, 3.5, sz * 1.74);
      if (sz < 0) gable.rotation.y = Math.PI;
      gable.castShadow = true;
      house.add(gable);
    }
    // 双坡屋面，带一点出檐
    for (const sx of [-1, 1]) {
      const slope = new THREE.Mesh(new THREE.BoxGeometry(2.95, 0.14, 4.3), mat(0xc96f4a));
      slope.position.set(sx * 1.25, 4.25, 0);
      slope.rotation.z = -sx * 0.54;
      slope.castShadow = true;
      house.add(slope);
    }
    const ridge = new THREE.Mesh(new THREE.BoxGeometry(0.24, 0.18, 4.35), mat(0xa85638));
    ridge.position.y = 5.0; ridge.castShadow = true; house.add(ridge);
    // 门 + 门框 + 门前石阶
    const doorFrame = new THREE.Mesh(new THREE.BoxGeometry(1.2, 1.9, 0.12), mat(0x6d4c3d));
    doorFrame.position.set(0.6, 1.35, 1.83); house.add(doorFrame);
    const door = new THREE.Mesh(new THREE.BoxGeometry(0.95, 1.7, 0.14), mat(0x8d6e63));
    door.position.set(0.6, 1.25, 1.86); house.add(door);
    const knob = new THREE.Mesh(new THREE.SphereGeometry(0.06, 6, 5), mat(0xf5b83d));
    knob.position.set(0.28, 1.25, 1.95); house.add(knob);
    const step1 = new THREE.Mesh(new THREE.BoxGeometry(1.5, 0.18, 0.6), mat(0xb8b0a4));
    step1.position.set(0.6, 0.34, 2.1); step1.castShadow = true; house.add(step1);
    const step2 = new THREE.Mesh(new THREE.BoxGeometry(1.7, 0.16, 0.9), mat(0x9a9a92));
    step2.position.set(0.6, 0.1, 2.25); step2.receiveShadow = true; house.add(step2);
    // 两扇带框窗；左侧窗为昼夜亮灯窗
    const winFrameL = new THREE.Mesh(new THREE.BoxGeometry(1.15, 1.15, 0.1), mat(0x6d4c3d));
    winFrameL.position.set(-1.1, 2.2, 1.83); house.add(winFrameL);
    const win = new THREE.Mesh(new THREE.BoxGeometry(0.95, 0.95, 0.12), mat(0xffe9a8, 0xffd970));
    win.position.set(-1.1, 2.2, 1.86); house.add(win);
    const winBarV = new THREE.Mesh(new THREE.BoxGeometry(0.07, 0.95, 0.14), mat(0x6d4c3d));
    winBarV.position.set(-1.1, 2.2, 1.87); house.add(winBarV);
    const winBarH = new THREE.Mesh(new THREE.BoxGeometry(0.95, 0.07, 0.14), mat(0x6d4c3d));
    winBarH.position.set(-1.1, 2.2, 1.87); house.add(winBarH);
    const winFrameR = new THREE.Mesh(new THREE.BoxGeometry(0.95, 0.95, 0.1), mat(0x6d4c3d));
    winFrameR.position.set(2.22, 2.3, -0.4); winFrameR.rotation.y = Math.PI / 2; house.add(winFrameR);
    const winR = new THREE.Mesh(new THREE.BoxGeometry(0.75, 0.75, 0.12), mat(0xd8cfc0));
    winR.position.set(2.25, 2.3, -0.4); winR.rotation.y = Math.PI / 2; house.add(winR);
    // 烟囱 + 压顶石
    const chimney = new THREE.Mesh(new THREE.BoxGeometry(0.55, 1.5, 0.55), mat(0xb08968));
    chimney.position.set(1.3, 4.9, -0.6); chimney.castShadow = true; house.add(chimney);
    const chimneyCap = new THREE.Mesh(new THREE.BoxGeometry(0.75, 0.16, 0.75), mat(0x9a9a92));
    chimneyCap.position.set(1.3, 5.7, -0.6); house.add(chimneyCap);
    // 门廊灯（夜间发光）
    const lanternArm = new THREE.Mesh(new THREE.BoxGeometry(0.07, 0.07, 0.5), mat(0x4a3325));
    lanternArm.position.set(1.45, 2.6, 2.0); house.add(lanternArm);
    const lantern = new THREE.Mesh(new THREE.BoxGeometry(0.22, 0.3, 0.22), mat(0xffe9a8, 0xffd970));
    lantern.position.set(1.45, 2.42, 2.25); house.add(lantern);
    house.scale.setScalar(1.35);
    house.position.set(-23, 0, -8);
    house.rotation.y = 0.5;
    this.scene.add(house);
    this.houseWindow = win;
    this.houseLantern = lantern;
    // 炊烟：从烟囱口循环升起的半透明烟团
    this.smoke = [];
    const smokeMat = new THREE.MeshLambertMaterial({ color: 0xe8e4dc, transparent: true, opacity: 0.5 });
    for (let i = 0; i < 6; i++) {
      const puff = new THREE.Mesh(new THREE.IcosahedronGeometry(0.22, 0), smokeMat.clone());
      puff.userData.phase = i / 6;
      this.smoke.push(puff);
      this.scene.add(puff);
    }
    this.scene.updateMatrixWorld(true);
    this.smokeOrigin = new THREE.Vector3();
    chimneyCap.getWorldPosition(this.smokeOrigin);
    this.smokeOrigin.y += 0.15;

    // 风车：石基座 + 门窗 + 瞭望台 + 帆式叶片 + 尾舵
    const mill = new THREE.Group();
    const millBase = new THREE.Mesh(new THREE.CylinderGeometry(1.7, 1.9, 0.8, 8), mat(0x9a9a92));
    millBase.position.y = 0.4; millBase.castShadow = true; mill.add(millBase);
    const tower = new THREE.Mesh(new THREE.CylinderGeometry(0.9, 1.4, 6, 6), mat(0xe8d5b7));
    tower.position.y = 3.4; tower.castShadow = true; mill.add(tower);
    // 环形木箍装饰（贴合塔身锥度）
    for (const [by, br] of [[1.6, 1.32], [4.6, 1.06]]) {
      const band = new THREE.Mesh(new THREE.TorusGeometry(br, 0.06, 5, 12), mat(0xbd9268));
      band.rotation.x = Math.PI / 2;
      band.position.y = by;
      mill.add(band);
    }
    const millDoor = new THREE.Mesh(new THREE.BoxGeometry(0.7, 1.3, 0.12), mat(0x8d6e63));
    millDoor.position.set(0, 1.45, 1.32); mill.add(millDoor);
    const millWin = new THREE.Mesh(new THREE.BoxGeometry(0.45, 0.45, 0.1), mat(0xffe9a8, 0xffd970));
    millWin.position.set(0, 3.6, 1.22); mill.add(millWin);
    // 瞭望台
    const deck = new THREE.Mesh(new THREE.CylinderGeometry(1.25, 1.25, 0.12, 6), mat(0xbd9268));
    deck.position.y = 5.6; deck.castShadow = true; mill.add(deck);
    for (let i = 0; i < 6; i++) {
      const railPost = new THREE.Mesh(new THREE.BoxGeometry(0.07, 0.5, 0.07), mat(0x8d6e63));
      const ra = (i / 6) * Math.PI * 2 + Math.PI / 6;
      railPost.position.set(Math.cos(ra) * 1.15, 5.9, Math.sin(ra) * 1.15);
      mill.add(railPost);
    }
    const cap = new THREE.Mesh(new THREE.ConeGeometry(1.3, 1.2, 6), mat(0xc96f4a));
    cap.position.y = 7.0; cap.castShadow = true; mill.add(cap);
    const capTip = new THREE.Mesh(new THREE.SphereGeometry(0.14, 6, 5), mat(0xf5b83d));
    capTip.position.y = 7.68; mill.add(capTip);
    // 帆式叶片：木梁 + 梯形帆布
    this.blades = new THREE.Group();
    for (let i = 0; i < 4; i++) {
      const holder = new THREE.Group();
      const spar = new THREE.Mesh(new THREE.BoxGeometry(0.14, 3.6, 0.1), mat(0x8d6e63));
      spar.position.y = 1.8; spar.castShadow = true; holder.add(spar);
      const sail = new THREE.Mesh(new THREE.BoxGeometry(0.85, 2.6, 0.05), mat(0xf5efe0));
      sail.position.set(0.42, 2.1, 0); sail.castShadow = true; holder.add(sail);
      for (const sy of [1.1, 2.0, 2.9]) {
        const batten = new THREE.Mesh(new THREE.BoxGeometry(0.95, 0.08, 0.08), mat(0xbd9268));
        batten.position.set(0.42, sy, 0.02);
        holder.add(batten);
      }
      holder.rotation.z = (i * Math.PI) / 2;
      this.blades.add(holder);
    }
    const hub = new THREE.Mesh(new THREE.CylinderGeometry(0.28, 0.28, 0.3, 8), mat(0x6d4c3d));
    hub.rotation.x = Math.PI / 2; this.blades.add(hub);
    this.blades.position.set(0, 6.3, 1.7);
    mill.add(this.blades);
    // 尾舵
    const vanePole = new THREE.Mesh(new THREE.BoxGeometry(0.08, 0.08, 1.6), mat(0x8d6e63));
    vanePole.position.set(0, 6.3, -1.4); mill.add(vanePole);
    const vane = new THREE.Mesh(new THREE.BoxGeometry(0.06, 0.9, 0.7), mat(0xc96f4a));
    vane.position.set(0, 6.3, -2.1); mill.add(vane);
    mill.scale.setScalar(1.3);
    mill.position.set(23, 0, -10);
    mill.rotation.y = -0.5;
    this.scene.add(mill);

    // 水井：双层石砌井壁 + 深井内壁 + 绞盘摇把 + 木桶 + 双坡顶棚
    const well = new THREE.Group();
    // 井边铺石让井体自然落在草地上。
    for (let i = 0; i < 10; i++) {
      const angle = (i / 10) * Math.PI * 2;
      const slab = new THREE.Mesh(new THREE.BoxGeometry(0.62, 0.09, 0.42), mat(i % 3 === 0 ? 0xaaa69b : 0x96948d));
      slab.position.set(Math.cos(angle) * 1.48, 0.045, Math.sin(angle) * 1.48);
      slab.rotation.y = -angle - Math.PI / 2 + Math.sin(i * 1.7) * 0.08;
      slab.receiveShadow = true;
      slab.castShadow = true;
      well.add(slab);
    }
    // 两层错缝砌石，每块石头略有色差和旋转。
    const stoneColors = [0x8e908d, 0xa2a39d, 0x858986, 0xb0ada3];
    for (let course = 0; course < 2; course++) {
      for (let i = 0; i < 12; i++) {
        const angle = (i / 12) * Math.PI * 2 + course * Math.PI / 12;
        const stone = new THREE.Mesh(
          new THREE.BoxGeometry(0.56, 0.32, 0.38),
          mat(stoneColors[(i + course * 2) % stoneColors.length]),
        );
        stone.position.set(Math.cos(angle) * 0.98, 0.31 + course * 0.32, Math.sin(angle) * 0.98);
        stone.rotation.y = -angle - Math.PI / 2 + Math.sin(i * 2.1 + course) * 0.035;
        stone.scale.y = 0.9 + ((i * 7 + course) % 3) * 0.05;
        stone.castShadow = true;
        stone.receiveShadow = true;
        well.add(stone);
      }
    }
    // 深色内壁和较低水面表现井口深度。
    const innerWall = new THREE.Mesh(
      new THREE.CylinderGeometry(0.78, 0.78, 0.72, 14, 1, true),
      new THREE.MeshStandardMaterial({
        color: 0x454a48,
        roughness: 1,
        metalness: 0,
        side: THREE.BackSide,
      }),
    );
    innerWall.position.y = 0.5;
    innerWall.receiveShadow = true;
    well.add(innerWall);
    const wellWater = new THREE.Mesh(
      new THREE.CircleGeometry(0.76, 16),
      new THREE.MeshStandardMaterial({
        color: 0x347c9a,
        emissive: 0x123746,
        emissiveIntensity: 0.18,
        roughness: 0.18,
        metalness: 0.08,
      }),
    );
    wellWater.rotation.x = -Math.PI / 2;
    wellWater.position.y = 0.22;
    well.add(wellWater);
    // 顶部整圈压边石。
    const capRing = new THREE.Mesh(new THREE.TorusGeometry(0.94, 0.17, 6, 16), mat(0xb2afa5));
    capRing.rotation.x = Math.PI / 2;
    capRing.position.y = 0.83;
    capRing.castShadow = true;
    well.add(capRing);

    // 木架底座、立柱及斜撑。
    for (const sx of [-1.12, 1.12]) {
      const foot = new THREE.Mesh(new THREE.BoxGeometry(0.38, 0.18, 0.42), mat(0x817f78));
      foot.position.set(sx, 0.92, 0);
      foot.castShadow = true;
      well.add(foot);
      const post = new THREE.Mesh(new THREE.BoxGeometry(0.2, 2.05, 0.2), mat(0x79553d));
      post.position.set(sx, 1.86, 0);
      post.castShadow = true;
      well.add(post);
      const brace = new THREE.Mesh(new THREE.BoxGeometry(0.12, 1.05, 0.12), mat(0x906849));
      brace.position.set(sx * 0.83, 1.35, 0);
      brace.rotation.z = sx > 0 ? -0.45 : 0.45;
      brace.castShadow = true;
      well.add(brace);
    }
    const topBeam = new THREE.Mesh(new THREE.BoxGeometry(2.65, 0.18, 0.22), mat(0x79553d));
    topBeam.position.y = 2.85;
    topBeam.castShadow = true;
    well.add(topBeam);

    // 横向木轴、卷绳轮和外侧 L 形摇把。
    const axle = new THREE.Mesh(new THREE.CylinderGeometry(0.09, 0.09, 2.65, 8), mat(0x644631));
    axle.rotation.z = Math.PI / 2;
    axle.position.y = 1.82;
    axle.castShadow = true;
    well.add(axle);
    const reel = new THREE.Mesh(new THREE.CylinderGeometry(0.18, 0.18, 0.48, 10), mat(0x8b6345));
    reel.rotation.z = Math.PI / 2;
    reel.position.y = 1.82;
    reel.castShadow = true;
    well.add(reel);
    for (const rx of [-0.16, -0.08, 0, 0.08, 0.16]) {
      const ropeCoil = new THREE.Mesh(new THREE.TorusGeometry(0.19, 0.022, 4, 10), mat(0xc3a56f));
      ropeCoil.rotation.y = Math.PI / 2;
      ropeCoil.position.set(rx, 1.82, 0);
      well.add(ropeCoil);
    }
    const crankArm = new THREE.Mesh(new THREE.BoxGeometry(0.08, 0.08, 0.58), mat(0x644631));
    crankArm.position.set(1.42, 1.82, 0.25);
    crankArm.castShadow = true;
    well.add(crankArm);
    const crankGrip = new THREE.Mesh(new THREE.CylinderGeometry(0.055, 0.055, 0.32, 7), mat(0x8b6345));
    crankGrip.rotation.z = Math.PI / 2;
    crankGrip.position.set(1.55, 1.82, 0.54);
    crankGrip.castShadow = true;
    well.add(crankGrip);

    // 绳索与带金属箍的锥形木桶。
    const rope = new THREE.Mesh(new THREE.CylinderGeometry(0.022, 0.022, 0.78, 6), mat(0xc3a56f));
    rope.position.y = 1.39;
    rope.castShadow = true;
    well.add(rope);
    const bucket = new THREE.Group();
    const bucketBody = new THREE.Mesh(new THREE.CylinderGeometry(0.24, 0.18, 0.4, 10, 1, true), mat(0x9a6d49));
    bucketBody.castShadow = true;
    bucket.add(bucketBody);
    const bucketBottom = new THREE.Mesh(new THREE.CircleGeometry(0.18, 10), mat(0x79553d));
    bucketBottom.rotation.x = Math.PI / 2;
    bucketBottom.position.y = -0.2;
    bucket.add(bucketBottom);
    for (const by of [-0.14, 0.14]) {
      const hoop = new THREE.Mesh(new THREE.TorusGeometry(by > 0 ? 0.225 : 0.19, 0.018, 4, 10), mat(0x5f6462));
      hoop.rotation.x = Math.PI / 2;
      hoop.position.y = by;
      bucket.add(hoop);
    }
    const handle = new THREE.Mesh(new THREE.TorusGeometry(0.25, 0.018, 4, 12, Math.PI), mat(0x5f6462));
    handle.position.y = 0.2;
    bucket.add(handle);
    bucket.position.set(0, 0.78, 0);
    well.add(bucket);

    // 双坡瓦顶，比规则锥顶更像真实井棚。
    for (const sz of [-1, 1]) {
      const roofPanel = new THREE.Mesh(new THREE.BoxGeometry(2.9, 0.12, 1.25), mat(sz > 0 ? 0xb95f43 : 0xc96f4a));
      roofPanel.position.set(0, 3.05, sz * 0.5);
      roofPanel.rotation.x = sz * 0.43;
      roofPanel.castShadow = true;
      well.add(roofPanel);
      for (const rz of [-0.35, 0, 0.35]) {
        const batten = new THREE.Mesh(new THREE.BoxGeometry(2.92, 0.045, 0.055), mat(0x9e513c));
        batten.position.set(0, 3.07 - Math.abs(rz) * 0.16, sz * (0.5 + rz));
        batten.rotation.x = sz * 0.43;
        well.add(batten);
      }
    }
    const roofRidge = new THREE.Mesh(new THREE.BoxGeometry(3.0, 0.17, 0.18), mat(0x8f4938));
    roofRidge.position.y = 3.33;
    roofRidge.castShadow = true;
    well.add(roofRidge);
    // 少量苔藓点缀井壁，打破过于干净的石材。
    for (const [mx, my, mz, ms] of [[-0.75, 0.45, 0.72, 0.13], [0.62, 0.68, 0.76, 0.1], [0.85, 0.32, -0.52, 0.12]]) {
      const moss = new THREE.Mesh(new THREE.IcosahedronGeometry(ms, 0), mat(0x628348));
      moss.position.set(mx, my, mz);
      well.add(moss);
    }
    well.scale.setScalar(1.5);
    well.position.set(0, 0, -14.5);
    this.scene.add(well);

    // 售卖架：木桌 + 条纹布棚 + 台面货品
    const stallAt = (x, z, rot, awningA, awningB) => {
      const stall = new THREE.Group();
      const counter = new THREE.Mesh(new THREE.BoxGeometry(2.3, 0.85, 1.1), mat(0xa9825a));
      counter.position.y = 0.45; counter.castShadow = true; stall.add(counter);
      const top = new THREE.Mesh(new THREE.BoxGeometry(2.5, 0.1, 1.3), mat(0xbd9268));
      top.position.y = 0.92; top.castShadow = true; stall.add(top);
      for (const px of [-1.05, 1.05]) {
        const pole = new THREE.Mesh(new THREE.BoxGeometry(0.12, 1.9, 0.12), mat(0x8d6e63));
        pole.position.set(px, 0.95, -0.5); pole.castShadow = true; stall.add(pole);
      }
      const awning = new THREE.Group();
      for (let s = 0; s < 5; s++) {
        const strip = new THREE.Mesh(
          new THREE.BoxGeometry(0.52, 0.06, 1.5),
          mat(s % 2 === 0 ? awningA : awningB)
        );
        strip.position.set(-1.04 + s * 0.52, 0, 0);
        strip.castShadow = true;
        awning.add(strip);
      }
      awning.position.set(0, 1.95, 0.1);
      awning.rotation.x = -0.18;
      stall.add(awning);
      const crate = new THREE.Mesh(new THREE.BoxGeometry(0.55, 0.4, 0.55), mat(0xbd9268));
      crate.position.set(-0.6, 1.17, 0); crate.castShadow = true; stall.add(crate);
      const goods = new THREE.Mesh(new THREE.IcosahedronGeometry(0.22, 0), mat(0xff8c42));
      goods.position.set(-0.6, 1.5, 0); goods.castShadow = true; stall.add(goods);
      const jar = new THREE.Mesh(new THREE.CylinderGeometry(0.16, 0.2, 0.42, 6), mat(0xe8d5b7));
      jar.position.set(0.55, 1.18, 0.1); jar.castShadow = true; stall.add(jar);
      stall.scale.setScalar(1.45);
      stall.position.set(x, 0, z);
      stall.rotation.y = rot;
      this.scene.add(stall);
    };
    stallAt(-5.8, -14.5, 0.28, 0xe7653f, 0xf7fbf2);
    stallAt(5.8, -14.5, -0.28, 0x2f7d4a, 0xf7fbf2);

    // 阔叶树
    const treeAt = (x, z, s = 1, baseY = terrainHeight(x, z)) => {
      const t = new THREE.Group();
      const trunk = new THREE.Mesh(new THREE.CylinderGeometry(0.18 * s, 0.28 * s, 1.6 * s, 5), mat(0x8d6e63));
      trunk.position.y = 0.8 * s; trunk.castShadow = true; t.add(trunk);
      const c1 = new THREE.Mesh(new THREE.IcosahedronGeometry(1.3 * s, 0), mat(0x58a05a));
      c1.position.y = 2.4 * s; c1.castShadow = true; t.add(c1);
      const c2 = new THREE.Mesh(new THREE.IcosahedronGeometry(0.9 * s, 0), mat(0x69b45f));
      c2.position.set(0.7 * s, 1.9 * s, 0.4 * s); c2.castShadow = true; t.add(c2);
      t.position.set(x, baseY, z);
      t.rotation.y = (x * 0.73 + z * 0.41) % (Math.PI * 2);
      this.scene.add(t);
    };
    // 松树：层叠锥形树冠
    const pineAt = (x, z, s = 1, baseY = terrainHeight(x, z)) => {
      const t = new THREE.Group();
      const trunk = new THREE.Mesh(new THREE.CylinderGeometry(0.14 * s, 0.22 * s, 1.0 * s, 5), mat(0x6d4c3d));
      trunk.position.y = 0.5 * s; trunk.castShadow = true; t.add(trunk);
      const layers = [[1.15, 1.5, 1.35], [0.88, 1.2, 2.15], [0.58, 0.95, 2.85]];
      for (const [r, h, y] of layers) {
        const cone = new THREE.Mesh(new THREE.ConeGeometry(r * s, h * s, 7), mat(0x3f7d46));
        cone.position.y = y * s; cone.castShadow = true; t.add(cone);
      }
      t.position.set(x, baseY, z);
      this.scene.add(t);
    };
    // 白桦：白色细树干与分层浅色树冠。
    const birchAt = (x, z, s = 1, baseY = terrainHeight(x, z)) => {
      const t = new THREE.Group();
      const trunk = new THREE.Mesh(new THREE.CylinderGeometry(0.12 * s, 0.18 * s, 2.5 * s, 6), mat(0xe7e1d3));
      trunk.position.y = 1.25 * s; trunk.castShadow = true; t.add(trunk);
      for (const [bx, by, bz, br, color] of [
        [0, 2.65, 0, 0.82, 0x82b85d],
        [0.48, 2.35, 0.08, 0.58, 0x9ac96f],
        [-0.42, 2.25, -0.12, 0.54, 0x75aa52],
      ]) {
        const crown = new THREE.Mesh(new THREE.IcosahedronGeometry(br * s, 0), mat(color));
        crown.position.set(bx * s, by * s, bz * s);
        crown.castShadow = true;
        t.add(crown);
      }
      t.position.set(x, baseY, z);
      t.rotation.y = (x * 0.51 + z * 0.29) % (Math.PI * 2);
      this.scene.add(t);
    };

    // 外围林海只保留三种主要轮廓：阔叶树、松树、白桦。
    // 两层林带围住整个地图，并通过正弦错位避免整齐排成直线。
    const treeBuilders = {
      oak: treeAt,
      pine: pineAt,
      birch: birchAt,
    };
    const forestLayout = [];
    const speciesPattern = ['oak', 'pine', 'oak', 'birch', 'pine', 'oak'];
    let forestIndex = 0;
    const addForestTree = (x, z, heightBoost = 0) => {
      const kind = speciesPattern[forestIndex % speciesPattern.length];
      const variation = ((forestIndex * 37) % 9) / 8;
      const scale = 1.55 + variation * 0.65 + heightBoost;
      forestLayout.push([kind, x, z, scale]);
      forestIndex++;
    };
    // 盆地内缘：后、前、左、右四面连续环绕。
    for (let x = -38; x <= 38; x += 7.5) {
      if (Math.abs(x) > 11) {
        addForestTree(x + Math.sin(x * 0.31) * 1.2, -25 + Math.cos(x * 0.23) * 1.8, 0.12);
      }
      addForestTree(x + Math.cos(x * 0.27) * 1.4, 23 + Math.sin(x * 0.19) * 1.8, -0.08);
    }
    for (let z = -18; z <= 22; z += 7.5) {
      addForestTree(-32 + Math.sin(z * 0.37) * 1.6, z + Math.cos(z * 0.21), 0.06);
      addForestTree(32 + Math.cos(z * 0.33) * 1.6, z + Math.sin(z * 0.24));
    }
    // 外层林带位于更高的丘陵上，形成远近层次。
    for (let x = -60; x <= 60; x += 9.5) {
      if (Math.abs(x) > 13) {
        addForestTree(x + Math.sin(x * 0.17) * 1.8, -48 + Math.cos(x * 0.2) * 2.0, 0.2);
      }
    }
    // 农场正面远景补两排疏密错落的树，避免围栏外直接露出大片空地。
    for (let x = -52; x <= 52; x += 10.5) {
      addForestTree(x + Math.sin(x * 0.21) * 2.1, 44 + Math.cos(x * 0.16) * 2.4, 0.18);
    }
    for (let x = -39; x <= 39; x += 13) {
      addForestTree(x + Math.cos(x * 0.24) * 1.8, 33 + Math.sin(x * 0.18) * 2.1, 0.04);
    }
    // 售卖架与水塘朝向的远端保留溪流通道，同时填补原先中央林带的明显缺口。
    [
      [-18, -36, 0.08], [4, -35, -0.04], [13, -39, 0.12],
      [-14, -45, 0.18], [-2, -48, 0.06], [10, -47, 0.2],
    ].forEach(([x, z, boost]) => addForestTree(x, z, boost));
    for (let z = -35; z <= 31; z += 9.5) {
      addForestTree(-58 + Math.cos(z * 0.23) * 2.2, z, 0.16);
      addForestTree(58 + Math.sin(z * 0.2) * 2.2, z + 1.5, 0.12);
    }
    const overlapsMountainInterior = (x, z) => mountainFootprints.some((mountain) => (
      Math.hypot(x - mountain.x, z - mountain.z) < mountain.radius * 0.9
    ));
    forestLayout
      .filter(([, x, z]) => !overlapsMountainInterior(x, z))
      .forEach(([kind, x, z, s]) => treeBuilders[kind](x, z, s));
    mountainVegetationSpots.forEach(([kind, x, z, s, y]) => treeBuilders[kind](x, z, s, y));

    // 灌木丛：低矮圆球簇
    const bushAt = (x, z, s = 1, baseY = terrainHeight(x, z)) => {
      const b = new THREE.Group();
      const blobs = [[0, 0.42, 0, 0.55], [0.45, 0.34, 0.2, 0.4], [-0.4, 0.32, 0.15, 0.38]];
      for (const [bx, by, bz, br] of blobs) {
        const blob = new THREE.Mesh(new THREE.IcosahedronGeometry(br * s, 0), mat(0x4e8f4a));
        blob.position.set(bx * s, by * s, bz * s);
        blob.castShadow = true;
        b.add(blob);
      }
      b.position.set(x, baseY, z);
      this.scene.add(b);
    };
    mountainShrubSpots.forEach(([x, z, s, y]) => bushAt(x, z, s, y));
    bushAt(-10, 12.5); bushAt(8, 12.8, 0.85); bushAt(-16, -13, 1.1);
    bushAt(12, -13, 0.9); bushAt(19, 11, 0.8); bushAt(-16, 9, 0.9); bushAt(16, 9, 0.75);
    [
      [-37, -20, 1.1], [-34, -6, 0.9], [-39, 13, 1.0],
      [-29, 25, 0.85], [-18, -23, 0.95], [-8, -25, 0.8],
      [9, -25, 0.9], [20, -23, 0.82], [38, -15, 1.0],
      [39, 4, 0.9], [36, 20, 1.05], [27, 25, 0.85],
    ].forEach(([x, z, s]) => bushAt(x, z, s));

    // 草丛：散布的小草锥
    for (let i = 0; i < 46; i++) {
      const gx = (Math.random() - 0.5) * 58;
      const gz = (Math.random() - 0.5) * 36;
      if (Math.abs(gx) < 15.5 && Math.abs(gz) < 8) continue;            // 避开地块
      if (gx < -18 && gz > -13 && gz < -3) continue;                     // 避开小屋
      if (gx > 18 && gz > -15 && gz < -5) continue;                      // 避开风车
      if (Math.abs(gx) < 10 && gz > -17.5 && gz < -11.5) continue;       // 避开栅栏外的水井与售卖架
      const tuft = new THREE.Mesh(new THREE.ConeGeometry(0.16 + Math.random() * 0.1, 0.4 + Math.random() * 0.3, 4), mat(0x5da854));
      tuft.position.set(gx, terrainHeight(gx, gz) + 0.2, gz);
      tuft.rotation.y = Math.random() * Math.PI;
      this.scene.add(tuft);
    }

    // 干草堆
    const hayAt = (x, z, s = 1) => {
      const hay = new THREE.Mesh(new THREE.ConeGeometry(1.0 * s, 1.5 * s, 8), mat(0xe0c070));
      hay.position.set(x, 0.75 * s, z);
      hay.castShadow = true;
      this.scene.add(hay);
      const band = new THREE.Mesh(new THREE.TorusGeometry(0.62 * s, 0.05 * s, 4, 10), mat(0xb89a4a));
      band.rotation.x = Math.PI / 2;
      band.position.set(x, 0.62 * s, z);
      this.scene.add(band);
    };
    hayAt(14.5, 8.5); hayAt(15.6, 7.0, 0.65);

    // 南瓜角
    const pumpkinAt = (x, z, s = 1) => {
      const p = new THREE.Group();
      const body = new THREE.Mesh(new THREE.SphereGeometry(0.42 * s, 8, 6), mat(0xe87f2e));
      body.scale.y = 0.75; body.position.y = 0.32 * s; body.castShadow = true; p.add(body);
      const stem = new THREE.Mesh(new THREE.CylinderGeometry(0.05 * s, 0.07 * s, 0.22 * s, 5), mat(0x4e7d3a));
      stem.position.y = 0.68 * s; p.add(stem);
      p.position.set(x, 0, z);
      p.rotation.y = Math.random() * Math.PI;
      this.scene.add(p);
    };
    pumpkinAt(-15.2, 8.8); pumpkinAt(-16.1, 8.0, 0.7); pumpkinAt(-14.6, 7.8, 0.85); pumpkinAt(-15.6, 9.6, 0.6);

    // 飞鸟：带头部、胸腹、尾羽与弧形翅膀的低多边形模型。
    this.birds = [];
    const wingShape = new THREE.Shape();
    wingShape.moveTo(0, 0);
    wingShape.quadraticCurveTo(0.24, 0.16, 0.48, 0.18);
    wingShape.quadraticCurveTo(0.75, 0.18, 0.9, 0.02);
    wingShape.quadraticCurveTo(0.72, -0.09, 0.48, -0.12);
    wingShape.lineTo(0.12, -0.07);
    wingShape.quadraticCurveTo(0.03, -0.04, 0, 0);
    const birdWingGeometry = new THREE.ShapeGeometry(wingShape, 3);
    const birdBodyGeometry = new THREE.SphereGeometry(0.3, 8, 6);
    const birdPalettes = [
      { body: 0x516477, wing: 0x6f8498, feather: 0xa8bac7, chest: 0xdce3df },
      { body: 0x8a765f, wing: 0x9e896c, feather: 0xc7b28f, chest: 0xeee1c8 },
      { body: 0x445564, wing: 0x667986, feather: 0xe0e6e3, chest: 0xf2f0e8 },
    ];
    for (let i = 0; i < 3; i++) {
      const palette = birdPalettes[i];
      const bird = new THREE.Group();
      // 头部和尾羽会被烘焙进该鸟自己的静态批次，不与其它鸟共享待释放 Geometry。
      const birdHeadGeometry = new THREE.SphereGeometry(0.19, 8, 6);
      const birdTailGeometry = new THREE.ConeGeometry(0.12, 0.42, 3);
      const bodyMaterial = new THREE.MeshStandardMaterial({
        color: palette.body,
        roughness: 0.84,
        metalness: 0,
        flatShading: true,
      });
      const wingMaterial = new THREE.MeshStandardMaterial({
        color: palette.wing,
        roughness: 0.88,
        metalness: 0,
        flatShading: true,
        side: THREE.DoubleSide,
      });
      const featherMaterial = new THREE.MeshStandardMaterial({
        color: palette.feather,
        roughness: 0.9,
        metalness: 0,
        flatShading: true,
        side: THREE.DoubleSide,
      });
      const body = new THREE.Mesh(birdBodyGeometry, bodyMaterial);
      body.scale.set(0.66, 0.55, 1.38);
      bird.add(body);
      const chest = new THREE.Mesh(birdBodyGeometry, new THREE.MeshStandardMaterial({
        color: palette.chest,
        roughness: 0.92,
        flatShading: true,
      }));
      chest.scale.set(0.46, 0.32, 0.72);
      chest.position.set(0, -0.13, 0.16);
      bird.add(chest);
      const head = new THREE.Mesh(birdHeadGeometry, bodyMaterial);
      head.position.set(0, 0.08, 0.4);
      bird.add(head);
      const beak = new THREE.Mesh(new THREE.ConeGeometry(0.075, 0.22, 4), mat(0xe1a344));
      beak.rotation.x = Math.PI / 2;
      beak.position.set(0, 0.05, 0.61);
      bird.add(beak);
      for (const side of [-1, 1]) {
        const eye = new THREE.Mesh(new THREE.SphereGeometry(0.032, 6, 4), mat(0x18232c));
        eye.position.set(side * 0.155, 0.12, 0.46);
        bird.add(eye);
        const tail = new THREE.Mesh(birdTailGeometry, featherMaterial);
        tail.rotation.x = -Math.PI / 2;
        tail.rotation.z = side * 0.16;
        tail.position.set(side * 0.1, 0, -0.52);
        bird.add(tail);
      }
      const mkWing = (side) => {
        const pivot = new THREE.Group();
        pivot.position.set(side * 0.13, 0.07, 0.02);
        const wing = new THREE.Mesh(birdWingGeometry, wingMaterial);
        wing.rotation.x = -Math.PI / 2;
        wing.scale.x = side;
        pivot.add(wing);
        const feather = new THREE.Mesh(birdWingGeometry, featherMaterial);
        feather.rotation.x = -Math.PI / 2;
        feather.position.y = 0.012;
        feather.scale.set(side * 0.7, 0.7, 0.7);
        pivot.add(feather);
        bird.add(pivot);
        return pivot;
      };
      bird.scale.setScalar(BIRD_SCENE_SCALE * (0.98 + i * 0.05));
      bird.userData = {
        leftWing: mkWing(-1), rightWing: mkWing(1),
        seed: i * 2.4, speed: 0.16 + i * 0.03,
        radius: 13 + i * 3.5, height: 12 + i * 1.8,
      };
      bird.userData.batchStats = batchStaticMeshes(bird, {
        exclude: [bird.userData.leftWing, bird.userData.rightWing],
        pruneEmpty: true,
        freezeTransforms: true,
      });
      this.birds.push(bird);
      this.scene.add(bird);
    }

    // 萤火虫：夜间在植被间漂移
    const fireflyCount = 24;
    const fireflyPos = new Float32Array(fireflyCount * 3);
    this.firefliesBase = new Float32Array(fireflyCount);
    for (let i = 0; i < fireflyCount; i++) {
      fireflyPos[i * 3] = (Math.random() - 0.5) * 52;
      fireflyPos[i * 3 + 1] = 0.6 + Math.random() * 1.6;
      fireflyPos[i * 3 + 2] = (Math.random() - 0.5) * 30;
      this.firefliesBase[i] = fireflyPos[i * 3 + 1];
    }
    const fireflyGeo = new THREE.BufferGeometry();
    fireflyGeo.setAttribute('position', new THREE.BufferAttribute(fireflyPos, 3));
    this.fireflies = new THREE.Points(fireflyGeo, new THREE.PointsMaterial({
      color: 0xd0ff70, size: 0.22, transparent: true, opacity: 0,
    }));
    this.scene.add(this.fireflies);

    // 石头与花
    for (let i = 0; i < 10; i++) {
      const rock = new THREE.Mesh(new THREE.IcosahedronGeometry(0.25 + Math.random() * 0.3, 0), mat(0xb8b0a4));
      rock.position.set((Math.random() - 0.5) * 44, 0.15, (Math.random() < 0.5 ? -1 : 1) * (12 + Math.random() * 5));
      rock.castShadow = true; this.scene.add(rock);
    }
    for (let i = 0; i < 24; i++) {
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

    // 不改变材质与顶点，只把静态、不透明、同渲染状态的环境 Mesh 合并。
    // 动画节点与透明物体保留原层级，避免影响动画、透明排序和昼夜材质切换。
    this.staticBatchStats = batchStaticMeshes(this.scene, {
      exclude: [
        this.stream,
        this.pond,
        this.blades,
        this.houseWindow,
        this.houseLantern,
        this.fountainJet,
        this.fountainDroplets,
        ...this.pondRipples,
        ...this.clouds,
        ...this.cloudShadows,
        ...this.butterflies,
        ...this.smoke,
        ...this.birds,
        this.fireflies,
        this.stars,
        this.dust,
      ].filter(Boolean),
      pruneEmpty: true,
      freezeTransforms: true,
      freezeWorld: true,
    });

    this.wildlife = new WildlifeController(this.scene, {
      heightAt: terrainHeight,
      // Keep the procedural animals for the first frame. GLB replacements are
      // loaded from schedulePostFirstFrameWork once the farm is already visible.
      loadAssets: false,
    });
  }

  // ---------------- 地块 ----------------
  buildPlotsBase() {
    const resources = this.plotResources;
    const furrowMatrix = new THREE.Matrix4();
    this.matureEffectPool = new MatureEffectPool(this.scene, COLS * ROWS);
    for (let i = 0; i < COLS * ROWS; i++) {
      const g = new THREE.Group();
      const { x, z } = plotPos(i);
      g.position.set(x, 0, z);

      // 半埋式低矮菜畦：恢复真实厚度与阴影，但用圆润斜边避免“积木台座”。
      const rim = new THREE.Mesh(resources.rimGeometry, mat(0x8b6e50));
      rim.position.y = PLOT_RIM_CENTER_Y;
      rim.receiveShadow = true;
      rim.castShadow = true;
      freezeLocalTransform(rim);
      g.add(rim);
      g.userData.rim = rim;

      // 顶部土壤保持平整，覆盖在低矮土畦上并承担拾取。
      const base = new THREE.Mesh(resources.baseGeometry, mat(0xb5977a));
      base.rotation.x = -Math.PI / 2;
      base.position.y = PLOT_SURFACE_Y;
      base.receiveShadow = true; base.castShadow = false;
      base.userData.plotId = i;
      freezeLocalTransform(base);
      g.add(base);
      g.userData.base = base;

      // 三条土垄共享一份 Geometry，并在单个 draw call 中实例化。
      const furrows = new THREE.InstancedMesh(resources.furrowGeometry, mat(0x684936), 3);
      for (let r = -1; r <= 1; r++) {
        furrowMatrix.makeTranslation(0, PLOT_FURROW_Y, r * 1.22);
        furrows.setMatrixAt(r + 1, furrowMatrix);
      }
      furrows.instanceMatrix.needsUpdate = true;
      furrows.receiveShadow = true;
      furrows.castShadow = true;
      furrows.computeBoundingBox();
      furrows.computeBoundingSphere();
      furrows.visible = false;
      freezeLocalTransform(furrows);
      g.add(furrows);
      g.userData.furrows = furrows;

      // 悬停高亮环
      const ring = new THREE.Mesh(
        resources.ringGeometry,
        resources.ringMaterial,
      );
      ring.rotation.x = -Math.PI / 2;
      ring.rotation.z = Math.PI / 4;
      ring.position.y = 0.29;
      ring.visible = false;
      freezeLocalTransform(ring);
      g.add(ring);
      g.userData.ring = ring;

      const matureFx = this.matureEffectPool.createEffect(i, { x, z });
      g.userData.matureFx = matureFx;

      // 内容容器（作物/杂草/害虫等）
      const content = new THREE.Group();
      content.position.y = PLOT_SURFACE_Y + 0.01;
      freezeLocalTransform(content);
      g.add(content);
      g.userData.content = content;
      g.userData.key = '';

      // 锁定牌
      const sign = new THREE.Group();
      const post = new THREE.Mesh(resources.signPostGeometry, mat(0xa9825a));
      post.position.y = 0.4; freezeLocalTransform(post); sign.add(post);
      const board = new THREE.Mesh(resources.signBoardGeometry, mat(0xbd9268));
      board.position.y = 0.95; freezeLocalTransform(board); sign.add(board);
      freezeLocalTransform(sign);
      g.add(sign);
      g.userData.sign = sign;
      g.userData.signBoard = board;

      freezeLocalTransform(g);
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
    const key = [
      info.unlocked ? 1 : 0,
      info.lockText || '',
      info.state,
      info.cropDef?.id || '',
      info.stage,
      info.totalStages,
      info.dry ? 1 : 0,
      info.weed ? 1 : 0,
      info.pest ? 1 : 0,
    ].join('|');
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
      u.matureFx.visible = false;
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
    u.matureFx.visible = false;
    if (info.state === PLOT.GROWING || info.state === PLOT.MATURE) {
      const crop = optimizeStaticModel(createCropModel(info.cropDef, {
        stage: info.stage, totalStages: info.totalStages, mature: info.state === PLOT.MATURE,
      }));
      u.content.add(crop);
      u.cropGroup = crop;
      if (info.weed) { const w = optimizeStaticModel(createWeedModel()); w.position.set(1.1, 0, 0.9); u.content.add(w); }
      if (info.weed) { const w2 = optimizeStaticModel(createWeedModel()); w2.position.set(-1.0, 0, -0.8); w2.scale.setScalar(0.7); u.content.add(w2); }
      if (info.pest) { const p = createPestModel(); p.position.y = 1.0; u.content.add(p); u.pestGroup = p; }
      else u.pestGroup = null;
      if (info.state === PLOT.MATURE) u.matureFx.visible = true;
    } else if (info.state === PLOT.WITHERED) {
      if (info.cropDef) {
        const dead = optimizeStaticModel(createCropModel(info.cropDef, { stage: 2, totalStages: 3, mature: true, withered: true }));
        u.content.add(dead);
      } else {
        // 服务端清空 crop_id 的枯萎地块：无作物定义，渲染为通用枯萎残株，不伪造具体作物
        u.content.add(optimizeStaticModel(createResidueModel()));
      }
    } else if (info.state === PLOT.RESIDUE) {
      u.content.add(optimizeStaticModel(createResidueModel()));
    } else if (info.state === PLOT.WASTELAND) {
      // 荒地上的野草石
      const w = optimizeStaticModel(createWeedModel()); w.scale.setScalar(0.6); w.position.set(0.8, 0, -0.6); u.content.add(w);
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
      this.dogGroup.scale.multiplyScalar(DOG_SCENE_SCALE);
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
    this.plotBases = this.plotGroups.map(g => g.userData.base);
    this._pendingPointer = null;
    this._hoveredPlotId = null;
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
      this._pendingPointer = { clientX: e.clientX, clientY: e.clientY };
    };
    el.addEventListener('pointerdown', this._onPointerDown);
    el.addEventListener('pointerup', this._onPointerUp);
    el.addEventListener('pointermove', this._onPointerMove);
  }

  flushPointerMove() {
    if (!this._pendingPointer) return;
    const pointer = this._pendingPointer;
    this._pendingPointer = null;
    const hit = this.pick(pointer);
    if (hit !== this._hoveredPlotId) {
      this._hoveredPlotId = hit;
      this.plotGroups.forEach(g => { g.userData.ring.visible = g.userData.base.userData.plotId === hit; });
      this.renderer.domElement.style.cursor = hit !== null ? 'pointer' : 'grab';
    }
    if (this.hoverCb) this.hoverCb(hit, pointer.clientX, pointer.clientY);
  }

  pick(e) {
    const vp = this.env.getViewport();
    this.pointer.set((e.clientX / vp.width) * 2 - 1, -(e.clientY / vp.height) * 2 + 1);
    this.raycaster.setFromCamera(this.pointer, this.camera);
    const hits = this.raycaster.intersectObjects(this.plotBases, false);
    return hits.length ? hits[0].object.userData.plotId : null;
  }

  // ---------------- 粒子 ----------------
  burst(plotId, color, count = 18, up = true) {
    const { x, z } = plotPos(plotId);
    const geometry = new THREE.SphereGeometry(1, 4, 3);
    const material = new THREE.MeshBasicMaterial({ color, transparent: true });
    const burst = new THREE.InstancedMesh(geometry, material, count);
    burst.frustumCulled = false;
    burst.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
    freezeLocalTransform(burst);
    const positions = new Float32Array(count * 3);
    const velocities = new Float32Array(count * 3);
    const sizes = new Float32Array(count);
    for (let i = 0; i < count; i++) {
      const offset = i * 3;
      sizes[i] = 0.08 + this.env.random() * 0.1;
      positions[offset] = x + (this.env.random() - 0.5) * 2.4;
      positions[offset + 1] = up ? 0.8 + this.env.random() : 3.5 + this.env.random();
      positions[offset + 2] = z + (this.env.random() - 0.5) * 2.4;
      velocities[offset] = (this.env.random() - 0.5) * 1.4;
      velocities[offset + 1] = up ? 1.5 + this.env.random() * 1.6 : -(2 + this.env.random() * 2);
      velocities[offset + 2] = (this.env.random() - 0.5) * 1.4;
      this._particleMatrix.makeScale(sizes[i], sizes[i], sizes[i]);
      this._particleMatrix.setPosition(positions[offset], positions[offset + 1], positions[offset + 2]);
      burst.setMatrixAt(i, this._particleMatrix);
    }
    burst.instanceMatrix.needsUpdate = true;
    burst.userData = { life: 1, positions, velocities, sizes };
    this.particles.push(burst);
    this.scene.add(burst);
  }
  waterAnim(id) { this.burst(id, 0x5cb3ff, 22, false); }
  harvestAnim(id) { this.burst(id, 0xffd54f, 20, true); }
  magicAnim(id) { this.burst(id, 0xc77dff, 16, true); }
  matureAnim(id) {
    this.burst(id, 0xffd54f, 24, true);
    this.burst(id, 0x9be15d, 14, true);
  }

  // ---------------- 日夜 ----------------
  setDayPhase(phase) {
    this.dayPhase = phase;
    const k = lerpKey(phase, this._daySkyColor, this._daySunColor);
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
    if (this.fireflies) this.fireflies.material.opacity = night ? 0.85 : 0;
    const lightsOn = h < 0.12;
    if (lightsOn !== this._lightsOn) {
      this._lightsOn = lightsOn;
      this.houseWindow.material = mat(lightsOn ? 0xffe9a8 : 0xd8cfc0, lightsOn ? 0xffd970 : 0);
      if (this.houseLantern) {
        this.houseLantern.material = mat(lightsOn ? 0xffe9a8 : 0xd8cfc0, lightsOn ? 0xffd970 : 0);
      }
    }
  }

  updateFountainDroplets(t) {
    const droplets = this.fountainDroplets;
    if (!droplets) return;
    const { angles, phases } = droplets.userData;
    for (let i = 0; i < droplets.count; i++) {
      const cycle = (t * 0.72 + phases[i]) % 1;
      const radius = 1.45 * cycle;
      const arcHeight = 0.78 * (1 - cycle) + 0.08 * cycle + 2.65 * cycle * (1 - cycle);
      const size = 0.58 + Math.sin(cycle * Math.PI) * 0.62;
      this._fountainDropletMatrix.makeScale(size, size, size);
      this._fountainDropletMatrix.setPosition(
        Math.cos(angles[i]) * radius,
        arcHeight,
        Math.sin(angles[i]) * radius,
      );
      droplets.setMatrixAt(i, this._fountainDropletMatrix);
    }
    droplets.instanceMatrix.needsUpdate = true;
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
      this._elapsedTime += dt;
      const t = this._elapsedTime;

      this.controls.update();
      this.flushPointerMove();
      this.blades.rotation.z += dt * 0.8;

      // 云漂移
      for (const c of this.clouds) {
        c.position.x += c.userData.speed * dt;
        if (c.position.x > 70) c.position.x = -70;
        if (c.userData.shadow) {
          c.userData.shadow.position.x = c.position.x + c.userData.shadow.userData.offsetX;
        }
      }
      // 河流法线沿 UV.x（曲线下游方向）滚动，水塘使用更慢的交叉扰动。
      if (this.waterNormalMaps?.length === 4) {
        const [streamNormal, streamDetail, pondNormal, pondDetail] = this.waterNormalMaps;
        streamNormal.offset.set((t * 0.032) % 1, Math.sin(t * 0.13) * 0.035);
        streamDetail.offset.set((t * 0.057) % 1, Math.cos(t * 0.17) * 0.025);
        pondNormal.offset.set((t * 0.008) % 1, (t * 0.004) % 1);
        pondDetail.offset.set((-t * 0.012) % 1, (t * 0.007) % 1);
      }
      if (this.fountainJet) {
        const pulse = 0.98 + Math.sin(t * 9) * 0.035;
        this.fountainJet.scale.set(1 / pulse, pulse, 1 / pulse);
      }
      this.updateFountainDroplets(t);
      for (const pondRipple of this.pondRipples) {
        const cycle = (t * 0.2 + pondRipple.userData.phase / (Math.PI * 2)) % 1;
        const scale = 0.7 + cycle * 0.9;
        pondRipple.scale.set(scale, scale * 0.68, 1);
        pondRipple.material.opacity = pondRipple.userData.baseOpacity * (1 - cycle);
      }
      // 蝴蝶
      for (const b of this.butterflies) {
        const s = b.userData.seed + t * 0.5;
        b.position.set(Math.sin(s) * 12 + Math.sin(s * 2.3) * 3, 1.6 + Math.sin(s * 3) * 0.7, Math.cos(s * 0.8) * 8);
        b.rotation.y = Math.atan2(Math.cos(s), -Math.sin(s * 0.8));
        const flap = Math.sin(t * 18 + b.userData.seed) * 0.9;
        b.userData.w1.rotation.y = flap; b.userData.w2.rotation.y = -flap;
      }
      // 炊烟：升起、扩散、变淡，被风往 +x 吹
      for (const puff of this.smoke) {
        const cycle = (t * 0.14 + puff.userData.phase) % 1;
        puff.position.set(
          this.smokeOrigin.x + cycle * 1.4 + Math.sin(t * 0.8 + puff.userData.phase * 9) * 0.15,
          this.smokeOrigin.y + cycle * 3.2,
          this.smokeOrigin.z + Math.cos(t * 0.6 + puff.userData.phase * 7) * 0.12,
        );
        puff.scale.setScalar(0.5 + cycle * 1.7);
        puff.material.opacity = 0.5 * (1 - cycle) * Math.min(1, cycle / 0.08);
      }
      // 飞鸟盘旋
      for (const bird of this.birds) {
        const a = t * bird.userData.speed + bird.userData.seed;
        bird.position.set(
          Math.cos(a) * bird.userData.radius,
          bird.userData.height + Math.sin(t * 0.9 + bird.userData.seed) * 0.6,
          Math.sin(a) * bird.userData.radius - 6,
        );
        bird.rotation.y = -a;
        bird.rotation.x = Math.sin(t * 0.7 + bird.userData.seed) * 0.035;
        bird.rotation.z = Math.sin(a * 1.8 + bird.userData.seed) * 0.1;
        const flapStrength = Math.sin(t * 0.72 + bird.userData.seed) > 0.48 ? 0.2 : 1;
        const flap = Math.sin(t * 8.2 + bird.userData.seed) * 0.62 * flapStrength;
        bird.userData.leftWing.rotation.z = 0.16 + flap;
        bird.userData.rightWing.rotation.z = -0.16 - flap;
      }
      // 萤火虫上下漂移
      if (this.fireflies) {
        this.fireflies.rotation.y = t * 0.05;
        const pos = this.fireflies.geometry.attributes.position;
        for (let i = 0; i < pos.count; i++) {
          pos.setY(i, this.firefliesBase[i] + Math.sin(t * 1.3 + i * 1.7) * 0.35);
        }
        pos.needsUpdate = true;
      }
      // 狗使用本地行为状态机随机巡游、转向和休息。
      if (this.dogGroup && this.dogBehavior) {
        this.dogBehavior.update(this.dogGroup, dt, t, this.dogGroup.userData.hungry);
      }
      this.wildlife?.update(dt, t);
      // 成熟花粉统一实例化；害虫与作物继续保留各自局部动画。
      this.matureEffectPool?.update(t, this.camera);
      for (const g of this.plotGroups) {
        const u = g.userData;
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
        const { positions, velocities, sizes } = p.userData;
        for (let j = 0; j < p.count; j++) {
          const offset = j * 3;
          positions[offset] += velocities[offset] * dt;
          positions[offset + 1] += velocities[offset + 1] * dt;
          positions[offset + 2] += velocities[offset + 2] * dt;
          this._particleMatrix.makeScale(sizes[j], sizes[j], sizes[j]);
          this._particleMatrix.setPosition(positions[offset], positions[offset + 1], positions[offset + 2]);
          p.setMatrixAt(j, this._particleMatrix);
        }
        p.instanceMatrix.needsUpdate = true;
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
      this.schedulePostFirstFrameWork();
    };
    this._rafId = this.env.requestAnimationFrame(loop);
  }

  stop() {
    this._running = false;
    if (this._rafId != null) {
      this.env.cancelAnimationFrame(this._rafId);
      this._rafId = null;
    }
  }

  /**
   * 幂等释放：停 RAF、卸监听、释放 controls/renderer 与场景独占 GPU 资源。
   * 不 dispose crops.mat() 共享材质。
   */
  dispose() {
    if (this._disposed) return;
    this._disposed = true;
    this.stop();
    if (this._shaderWarmupId != null) {
      this.env.cancelIdleCallback(this._shaderWarmupId);
      this._shaderWarmupId = null;
    }
    if (this._wildlifeLoadId != null) {
      this.env.cancelIdleCallback(this._wildlifeLoadId);
      this._wildlifeLoadId = null;
    }
    this._shaderWarmupPromise = null;

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
    this.wildlife?.dispose();
    this.wildlife = null;
    this.matureEffectPool?.dispose();
    this.matureEffectPool = null;

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
    this.plotBases = [];
    this._pendingPointer = null;
    this._hoveredPlotId = null;
    this.animated = [];
    this.clouds = [];
    this.cloudShadows = [];
    this.butterflies = [];
    this.smoke = [];
    this.smokeOrigin = null;
    this.birds = [];
    this.fireflies = null;
    this.firefliesBase = null;
    this.stream = null;
    this.streamCurve = null;
    this.streamRipples = [];
    this.pond = null;
    this.pondRipples = [];
    this.fountain = null;
    this.fountainJet = null;
    this.fountainDroplets = null;
    this.waterNormalMaps = null;
    this.stars = null;
    this.dust = null;
    this.blades = null;
    this.houseWindow = null;
    this.houseLantern = null;
    this._daySkyColor = null;
    this._daySunColor = null;
    this.staticBatchStats = null;
    this._particleMatrix = null;
    this._fountainDropletMatrix = null;
    this.plotResources = null;
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
