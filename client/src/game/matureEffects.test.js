import test from 'node:test';
import assert from 'node:assert/strict';
import * as THREE from 'three';

import { MatureEffectPool, MATURE_EFFECT_INSTANCES_PER_PLOT } from './matureEffects.js';

test('成熟特效池用单个 InstancedMesh 保留每块地六个独立实例', () => {
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(42, 4 / 3, 0.1, 300);
  camera.position.set(2, 24, 26);
  camera.lookAt(0, 0.5, 0);
  camera.updateProjectionMatrix();
  const pool = new MatureEffectPool(scene, 2);
  const first = pool.createEffect(0, { x: 2, z: 3 });
  const second = pool.createEffect(1, { x: -4, z: 5 });

  pool.update(0, camera);
  assert.equal(pool.mesh.visible, false);
  assert.equal(pool.mesh.count, 0);

  first.visible = true;
  pool.update(0, camera);
  assert.equal(pool.mesh.visible, true);
  assert.equal(pool.mesh.count, MATURE_EFFECT_INSTANCES_PER_PLOT);
  assert.equal(scene.children.filter((object) => object.isMesh).length, 1);

  second.visible = true;
  pool.update(0.4, camera);
  assert.equal(pool.mesh.count, MATURE_EFFECT_INSTANCES_PER_PLOT * 2);
  assert.ok(pool._active.every((record, index, records) => (
    index === 0 || records[index - 1].depth >= record.depth
  )), '透明实例应保持从远到近排序');

  pool.dispose();
  assert.equal(scene.children.length, 0);
});

test('实例化中心星芒保持原位置、缩放、颜色与透明度公式', () => {
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(42, 1, 0.1, 300);
  camera.position.set(0, 10, 20);
  camera.lookAt(0, 1, 0);
  camera.updateProjectionMatrix();
  const pool = new MatureEffectPool(scene, 1);
  pool.createEffect(0, { x: 2, z: 3 }).visible = true;
  pool.update(0, camera);

  const color = new THREE.Color();
  const matrix = new THREE.Matrix4();
  const position = new THREE.Vector3();
  const scale = new THREE.Vector3();
  let flareIndex = -1;
  for (let i = 0; i < pool.mesh.count; i++) {
    pool.mesh.getColorAt(i, color);
    if (color.getHex() === 0xffe797) {
      flareIndex = i;
      break;
    }
  }
  assert.notEqual(flareIndex, -1);
  pool.mesh.getMatrixAt(flareIndex, matrix);
  matrix.decompose(position, new THREE.Quaternion(), scale);
  assert.ok(position.distanceTo(new THREE.Vector3(2, 1.55, 3)) < 1e-6);
  assert.ok(scale.distanceTo(new THREE.Vector3(
    0.22 * 0.46 * 0.8,
    0.22 * 1.9 * 0.8,
    0.22 * 0.46 * 0.8,
  )) < 1e-6);
  assert.ok(Math.abs(pool.opacityAttribute.getX(flareIndex) - (0.68 + 0.8 * 0.22)) < 1e-6);
  pool.dispose();
});

test('成熟特效材质向 MeshBasic shader 注入每实例透明度', () => {
  const scene = new THREE.Scene();
  const pool = new MatureEffectPool(scene, 1);
  const shader = {
    vertexShader: '#include <common>\n#include <color_vertex>',
    fragmentShader: '#include <common>\n#include <color_fragment>',
  };

  pool.material.onBeforeCompile(shader);

  assert.match(shader.vertexShader, /attribute float instanceOpacity/);
  assert.match(shader.vertexShader, /vInstanceOpacity = instanceOpacity/);
  assert.match(shader.fragmentShader, /diffuseColor\.a \*= vInstanceOpacity/);
  assert.equal(pool.material.customProgramCacheKey(), 'farm-mature-instance-opacity-v1');
  pool.dispose();
});
