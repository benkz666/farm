import * as THREE from 'three';

const MOTES_PER_PLOT = 5;
const INSTANCES_PER_PLOT = MOTES_PER_PLOT + 1;
const MOTE_RADII = [0.11, 0.13, 0.15];
const MOTE_COLORS = [0xffc83d, 0xffe681, 0xfff2b2, 0xffd45c, 0xffc83d];
const FLARE_RADIUS = 0.22;
const FLARE_COLOR = 0xffe797;
const MOTE_COLOR_VALUES = MOTE_COLORS.map((color) => new THREE.Color(color));
const FLARE_COLOR_VALUE = new THREE.Color(FLARE_COLOR);

function createInstanceOpacityMaterial() {
  const material = new THREE.MeshBasicMaterial({
    color: 0xffffff,
    transparent: true,
    opacity: 1,
    depthWrite: false,
  });
  material.onBeforeCompile = (shader) => {
    shader.vertexShader = shader.vertexShader
      .replace(
        '#include <common>',
        '#include <common>\nattribute float instanceOpacity;\nvarying float vInstanceOpacity;',
      )
      .replace(
        '#include <color_vertex>',
        '#include <color_vertex>\nvInstanceOpacity = instanceOpacity;',
      );
    shader.fragmentShader = shader.fragmentShader
      .replace(
        '#include <common>',
        '#include <common>\nvarying float vInstanceOpacity;',
      )
      .replace(
        '#include <color_fragment>',
        '#include <color_fragment>\ndiffuseColor.a *= vInstanceOpacity;',
      );
  };
  material.customProgramCacheKey = () => 'farm-mature-instance-opacity-v1';
  return material;
}

/**
 * 将所有地块的成熟光点放进一个透明 InstancedMesh。
 * 每个实例仍保留独立颜色、透明度、缩放和旋转，并按相机深度排序。
 */
export class MatureEffectPool {
  constructor(scene, plotCount) {
    this.scene = scene;
    this.effects = new Array(plotCount).fill(null);
    this.maxInstances = plotCount * INSTANCES_PER_PLOT;
    this.geometry = new THREE.OctahedronGeometry(1, 0);
    this.opacityAttribute = new THREE.InstancedBufferAttribute(
      new Float32Array(this.maxInstances),
      1,
    );
    this.opacityAttribute.setUsage(THREE.DynamicDrawUsage);
    this.geometry.setAttribute('instanceOpacity', this.opacityAttribute);
    this.material = createInstanceOpacityMaterial();
    this.mesh = new THREE.InstancedMesh(this.geometry, this.material, this.maxInstances);
    this.mesh.name = 'mature-effect-pool';
    this.mesh.count = 0;
    this.mesh.visible = false;
    this.mesh.frustumCulled = false;
    this.mesh.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
    this.mesh.instanceColor = new THREE.InstancedBufferAttribute(
      new Float32Array(this.maxInstances * 3),
      3,
    );
    this.mesh.instanceColor.setUsage(THREE.DynamicDrawUsage);
    this.mesh.updateMatrix();
    this.mesh.matrixAutoUpdate = false;
    scene.add(this.mesh);

    this._dummy = new THREE.Object3D();
    this._projectionView = new THREE.Matrix4();
    this._projected = new THREE.Vector3();
    this._records = Array.from({ length: this.maxInstances }, () => ({
      x: 0,
      y: 0,
      z: 0,
      rotationY: 0,
      scaleX: 1,
      scaleY: 1,
      scaleZ: 1,
      opacity: 1,
      color: null,
      depth: 0,
    }));
    this._active = [];
  }

  createEffect(plotIndex, position) {
    const effect = {
      visible: false,
      userData: {
        plotIndex,
        x: Number(position?.x) || 0,
        z: Number(position?.z) || 0,
      },
    };
    this.effects[plotIndex] = effect;
    return effect;
  }

  update(time, camera) {
    if (!this.mesh || !camera) return;
    camera.updateMatrixWorld();
    this._projectionView.multiplyMatrices(camera.projectionMatrix, camera.matrixWorldInverse);
    this._active.length = 0;
    let recordIndex = 0;

    const addRecord = (
      x, y, z, rotationY,
      scaleX, scaleY, scaleZ,
      opacity, color,
    ) => {
      const record = this._records[recordIndex++];
      record.x = x;
      record.y = y;
      record.z = z;
      record.rotationY = rotationY;
      record.scaleX = scaleX;
      record.scaleY = scaleY;
      record.scaleZ = scaleZ;
      record.opacity = opacity;
      record.color = color;
      this._projected.set(record.x, record.y, record.z).applyMatrix4(this._projectionView);
      record.depth = this._projected.z;
      this._active.push(record);
    };

    for (const effect of this.effects) {
      if (!effect?.visible) continue;
      const plotIndex = effect.userData.plotIndex;
      const plotX = effect.userData.x;
      const plotZ = effect.userData.z;
      for (let i = 0; i < MOTES_PER_PLOT; i++) {
        const basePhase = plotIndex * 0.71 + i * (Math.PI * 2 / MOTES_PER_PLOT);
        const speed = 0.55 + (i % 3) * 0.1;
        const phase = basePhase + time * speed;
        const breathe = 0.82 + Math.sin(time * 2.8 + basePhase) * 0.25;
        const orbitRadius = 0.7 + (i % 3) * 0.2;
        const geometryRadius = MOTE_RADII[i % MOTE_RADII.length];
        addRecord(
          plotX + Math.cos(phase) * orbitRadius,
          0.95 + (i % 4) * 0.3 + Math.sin(time * 1.9 + basePhase) * 0.26,
          plotZ + Math.sin(phase) * orbitRadius,
          phase,
          geometryRadius * 0.6 * breathe,
          geometryRadius * 1.35 * breathe,
          geometryRadius * 0.6 * breathe,
          0.66 + breathe * 0.28,
          MOTE_COLOR_VALUES[i],
        );
      }

      const flarePhase = plotIndex * 0.71;
      const flarePulse = 0.8 + Math.sin(time * 3.2 + flarePhase) * 0.25;
      addRecord(
        plotX,
        1.55,
        plotZ,
        time * 1.4 + flarePhase,
        FLARE_RADIUS * 0.46 * flarePulse,
        FLARE_RADIUS * 1.9 * flarePulse,
        FLARE_RADIUS * 0.46 * flarePulse,
        0.68 + flarePulse * 0.22,
        FLARE_COLOR_VALUE,
      );
    }

    this._active.sort((a, b) => b.depth - a.depth);
    for (let i = 0; i < this._active.length; i++) {
      const record = this._active[i];
      this._dummy.position.set(record.x, record.y, record.z);
      this._dummy.rotation.set(0, record.rotationY, 0);
      this._dummy.scale.set(record.scaleX, record.scaleY, record.scaleZ);
      this._dummy.updateMatrix();
      this.mesh.setMatrixAt(i, this._dummy.matrix);
      this.mesh.setColorAt(i, record.color);
      this.opacityAttribute.setX(i, record.opacity);
    }

    this.mesh.count = this._active.length;
    this.mesh.visible = this.mesh.count > 0;
    if (this.mesh.count > 0) {
      this.mesh.instanceMatrix.needsUpdate = true;
      this.mesh.instanceColor.needsUpdate = true;
      this.opacityAttribute.needsUpdate = true;
    }
  }

  dispose() {
    if (!this.mesh) return;
    this.scene?.remove(this.mesh);
    this.geometry.dispose();
    this.material.dispose();
    this.effects = [];
    this._records = [];
    this._active = [];
    this.mesh = null;
    this.geometry = null;
    this.material = null;
    this.opacityAttribute = null;
    this.scene = null;
  }
}

export const MATURE_EFFECT_INSTANCES_PER_PLOT = INSTANCES_PER_PLOT;
