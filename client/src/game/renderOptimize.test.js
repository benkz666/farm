import test from 'node:test';
import assert from 'node:assert/strict';
import * as THREE from 'three';

import { batchStaticMeshes } from './renderOptimize.js';
import { createCropModel } from './crops.js';
import { CROPS, stageCount } from './config.js';

function renderableCount(root) {
  let count = 0;
  root.traverse((object) => {
    if (object.isMesh || object.isPoints || object.isLine) count++;
  });
  return count;
}

function triangleCount(root) {
  let count = 0;
  root.traverse((object) => {
    if (!object.isMesh) return;
    count += object.geometry.index
      ? object.geometry.index.count / 3
      : object.geometry.attributes.position.count / 3;
  });
  return count;
}

function vertexBounds(root) {
  root.updateMatrixWorld(true);
  const box = new THREE.Box3();
  const point = new THREE.Vector3();
  root.traverse((object) => {
    if (!object.isMesh) return;
    const position = object.geometry.attributes.position;
    for (let i = 0; i < position.count; i++) {
      point.fromBufferAttribute(position, i).applyMatrix4(object.matrixWorld);
      box.expandByPoint(point);
    }
  });
  return box;
}

function materialVertexCounts(root) {
  const counts = {};
  root.traverse((object) => {
    if (!object.isMesh) return;
    const key = object.material.uuid;
    counts[key] = (counts[key] || 0) + object.geometry.attributes.position.count;
  });
  return counts;
}

test('静态合批保持世界包围盒与三角形数量，并减少同材质 draw call', () => {
  const root = new THREE.Group();
  root.position.set(3, 2, -4);
  root.rotation.y = 0.37;
  const material = new THREE.MeshStandardMaterial({ color: 0x55aa44 });
  for (let i = 0; i < 4; i++) {
    const holder = new THREE.Group();
    holder.position.set(i * 1.4, i * 0.2, -i * 0.3);
    holder.rotation.z = i * 0.13;
    const mesh = new THREE.Mesh(new THREE.BoxGeometry(1, 0.5, 0.7), material);
    mesh.castShadow = true;
    holder.add(mesh);
    root.add(holder);
  }
  root.updateMatrixWorld(true);
  const beforeBox = vertexBounds(root);
  const beforeTriangles = triangleCount(root);

  const result = batchStaticMeshes(root, { pruneEmpty: true });
  const afterBox = vertexBounds(root);

  assert.deepEqual(result, { sourceMeshes: 4, batches: 1, drawCallReduction: 3 });
  assert.equal(renderableCount(root), 1);
  assert.equal(triangleCount(root), beforeTriangles);
  assert.ok(beforeBox.min.distanceTo(afterBox.min) < 1e-6);
  assert.ok(beforeBox.max.distanceTo(afterBox.max) < 1e-6);
  assert.equal(root.children[0].castShadow, true);
});

test('透明 Mesh 与受保护动画子树不参与静态合批', () => {
  const root = new THREE.Group();
  const opaque = new THREE.MeshStandardMaterial({ color: 0x446633 });
  const transparent = new THREE.MeshBasicMaterial({ transparent: true, opacity: 0.5 });
  for (let i = 0; i < 3; i++) {
    const mesh = new THREE.Mesh(new THREE.SphereGeometry(0.4, 6, 4), opaque);
    mesh.position.x = i;
    root.add(mesh);
  }
  const animated = new THREE.Group();
  animated.add(new THREE.Mesh(new THREE.SphereGeometry(0.4, 6, 4), opaque));
  root.add(animated);
  const glass = new THREE.Mesh(new THREE.PlaneGeometry(2, 2), transparent);
  root.add(glass);

  const result = batchStaticMeshes(root, { exclude: [animated], pruneEmpty: true });

  assert.deepEqual(result, { sourceMeshes: 3, batches: 1, drawCallReduction: 2 });
  assert.equal(animated.children.length, 1);
  assert.equal(glass.parent, root);
  assert.equal(renderableCount(root), 3);
});

test('清理只移除空 Group，不误删灯光 target 等语义 Object3D', () => {
  const root = new THREE.Group();
  const lightTarget = new THREE.Object3D();
  const emptyHolder = new THREE.Group();
  root.add(lightTarget, emptyHolder);

  batchStaticMeshes(root, { pruneEmpty: true });

  assert.equal(lightTarget.parent, root);
  assert.equal(emptyHolder.parent, null);
});

test('可冻结静态局部/世界矩阵，同时保留受保护动画节点自动更新', () => {
  const root = new THREE.Group();
  const material = new THREE.MeshStandardMaterial({ color: 0x557744 });
  for (let i = 0; i < 3; i++) {
    const mesh = new THREE.Mesh(new THREE.BoxGeometry(1, 1, 1), material);
    mesh.position.x = i * 2;
    root.add(mesh);
  }
  const animated = new THREE.Group();
  animated.add(new THREE.Mesh(new THREE.BoxGeometry(0.5, 0.5, 0.5), material));
  root.add(animated);

  batchStaticMeshes(root, {
    exclude: [animated],
    freezeTransforms: true,
    freezeWorld: true,
  });

  const batch = root.children.find((child) => child.name.startsWith('static-batch-'));
  assert.ok(batch);
  assert.equal(batch.matrixAutoUpdate, false);
  assert.equal(batch.matrixWorldAutoUpdate, false);
  assert.equal(animated.matrixAutoUpdate, true);
  assert.equal(animated.children[0].matrixAutoUpdate, true);
});

test('全部成熟作物合批前后保持材质顶点、三角形与世界顶点边界', () => {
  for (const crop of CROPS) {
    const totalStages = stageCount(crop);
    const options = { stage: totalStages, totalStages, mature: true };
    const optimized = createCropModel(crop, options);
    const beforeBounds = vertexBounds(optimized);
    const beforeTriangles = triangleCount(optimized);
    const beforeMaterials = materialVertexCounts(optimized);

    batchStaticMeshes(optimized, { pruneEmpty: true });

    const afterBounds = vertexBounds(optimized);
    assert.equal(triangleCount(optimized), beforeTriangles, `${crop.id} 三角形数应保持不变`);
    assert.deepEqual(materialVertexCounts(optimized), beforeMaterials, `${crop.id} 材质顶点应保持不变`);
    assert.ok(beforeBounds.min.distanceTo(afterBounds.min) < 1e-5, `${crop.id} 最小边界应保持不变`);
    assert.ok(beforeBounds.max.distanceTo(afterBounds.max) < 1e-5, `${crop.id} 最大边界应保持不变`);
  }
});
