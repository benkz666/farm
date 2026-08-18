import * as THREE from 'three';
import { mergeGeometries } from 'three/examples/jsm/utils/BufferGeometryUtils.js';

function hasProtectedAncestor(object, protectedObjects) {
  for (let current = object; current; current = current.parent) {
    if (protectedObjects.has(current)) return true;
  }
  return false;
}

function isVisibleInHierarchy(object, root) {
  for (let current = object; current; current = current.parent) {
    if (!current.visible) return false;
    if (current === root) return true;
  }
  return false;
}

function geometrySignature(geometry) {
  if (!geometry || Object.keys(geometry.morphAttributes || {}).length > 0) return null;
  if (geometry.drawRange.start !== 0 || geometry.drawRange.count !== Infinity) return null;

  const attributes = Object.entries(geometry.attributes)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, attribute]) => [
      name,
      attribute.itemSize,
      attribute.normalized ? 1 : 0,
      attribute.array?.constructor?.name || '',
    ].join(':'))
    .join(',');
  const index = geometry.index
    ? `i:${geometry.index.array?.constructor?.name || ''}`
    : 'n';
  return `${index}|${attributes}`;
}

function pruneEmptyGroups(root, protectedObjects) {
  const visit = (parent) => {
    for (const child of [...parent.children]) {
      visit(child);
      if (
        child.isGroup &&
        child.children.length === 0 &&
        !protectedObjects.has(child)
      ) {
        parent.remove(child);
      }
    }
  };
  visit(root);
}

function freezeStaticTransforms(root, protectedObjects) {
  root.updateMatrixWorld(true);
  root.traverse((object) => {
    if (
      object === root ||
      hasProtectedAncestor(object, protectedObjects) ||
      (!object.isGroup && !object.isMesh && !object.isPoints && !object.isLine)
    ) return;
    object.updateMatrix();
    object.matrixAutoUpdate = false;
  });
}

/**
 * 将不参与局部动画的同材质 Mesh 烘焙到 root 坐标系并合批。
 * 顶点、法线、材质和阴影标记保持不变；透明物体保留原顺序，不参与合批。
 *
 * @param {THREE.Object3D} root
 * @param {{ exclude?: THREE.Object3D[], minBatchSize?: number, pruneEmpty?: boolean, freezeTransforms?: boolean, freezeWorld?: boolean }} [options]
 * @returns {{ sourceMeshes: number, batches: number, drawCallReduction: number }}
 */
export function batchStaticMeshes(root, options = {}) {
  if (!root) return { sourceMeshes: 0, batches: 0, drawCallReduction: 0 };

  const protectedObjects = new Set(options.exclude || []);
  const minBatchSize = Math.max(2, Number(options.minBatchSize) || 2);
  const geometryRefs = new Map();
  const buckets = new Map();

  root.updateMatrixWorld(true);
  root.traverse((object) => {
    if (object.geometry) {
      geometryRefs.set(object.geometry, (geometryRefs.get(object.geometry) || 0) + 1);
    }
  });

  root.traverse((object) => {
    if (
      !object.isMesh ||
      object.isSkinnedMesh ||
      object.isInstancedMesh ||
      object.children.length > 0 ||
      !object.parent ||
      !isVisibleInHierarchy(object, root) ||
      hasProtectedAncestor(object, protectedObjects) ||
      Array.isArray(object.material) ||
      !object.material ||
      object.material.transparent ||
      object.material.opacity < 1 ||
      object.userData?.preserveDrawCall
    ) return;

    const signature = geometrySignature(object.geometry);
    if (!signature || object.matrixWorld.determinant() < 0) return;

    const key = [
      object.material.uuid,
      signature,
      object.castShadow ? 1 : 0,
      object.receiveShadow ? 1 : 0,
      object.renderOrder,
      object.layers.mask,
    ].join('|');
    if (!buckets.has(key)) buckets.set(key, []);
    buckets.get(key).push(object);
  });

  const rootInverse = root.matrixWorld.clone().invert();
  const disposedGeometries = new Set();
  const batchMeshes = [];
  let sourceMeshes = 0;
  let batches = 0;

  for (const meshes of buckets.values()) {
    if (meshes.length < minBatchSize) continue;

    const baked = meshes.map((mesh) => {
      const geometry = mesh.geometry.clone();
      const matrix = new THREE.Matrix4().multiplyMatrices(rootInverse, mesh.matrixWorld);
      geometry.applyMatrix4(matrix);
      return geometry;
    });
    const merged = mergeGeometries(baked, false);
    for (const geometry of baked) geometry.dispose();
    if (!merged) continue;

    merged.computeBoundingBox();
    merged.computeBoundingSphere();
    const first = meshes[0];
    const batch = new THREE.Mesh(merged, first.material);
    batch.name = `static-batch-${batches + 1}`;
    batch.castShadow = first.castShadow;
    batch.receiveShadow = first.receiveShadow;
    batch.renderOrder = first.renderOrder;
    batch.layers.mask = first.layers.mask;
    batch.matrixAutoUpdate = false;
    batch.updateMatrix();
    root.add(batch);
    batchMeshes.push(batch);

    for (const mesh of meshes) {
      mesh.parent?.remove(mesh);
      const refs = (geometryRefs.get(mesh.geometry) || 1) - 1;
      geometryRefs.set(mesh.geometry, refs);
      if (refs === 0 && !disposedGeometries.has(mesh.geometry)) {
        disposedGeometries.add(mesh.geometry);
        mesh.geometry.dispose();
      }
    }
    sourceMeshes += meshes.length;
    batches++;
  }

  if (options.pruneEmpty) pruneEmptyGroups(root, protectedObjects);
  if (options.freezeTransforms) freezeStaticTransforms(root, protectedObjects);
  root.updateMatrixWorld(true);
  if (options.freezeWorld) {
    for (const batch of batchMeshes) {
      batch.matrixWorldAutoUpdate = false;
      batch.matrixWorldNeedsUpdate = false;
    }
  }
  return { sourceMeshes, batches, drawCallReduction: sourceMeshes - batches };
}
