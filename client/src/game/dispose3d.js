// ============================================================
// Three.js 独占资源释放：跳过 mat() 共享材质
// ============================================================

const TEX_SLOTS = [
  'map', 'lightMap', 'bumpMap', 'normalMap', 'specularMap', 'envMap', 'alphaMap',
  'emissiveMap', 'metalnessMap', 'roughnessMap', 'aoMap', 'clearcoatMap',
  'clearcoatNormalMap', 'clearcoatRoughnessMap', 'sheenColorMap', 'sheenRoughnessMap',
  'iridescenceMap', 'iridescenceThicknessMap', 'transmissionMap', 'thicknessMap',
  'specularIntensityMap', 'specularColorMap', 'anisotropyMap',
];

function disposeTextureSlot(mat, slot, seenTex) {
  const tex = mat[slot];
  if (!tex || typeof tex.dispose !== 'function') return;
  if (seenTex.has(tex)) return;
  seenTex.add(tex);
  tex.dispose();
}

function disposeUniformTexture(value, seenTex) {
  if (value?.isTexture && typeof value.dispose === 'function') {
    if (seenTex.has(value)) return;
    seenTex.add(value);
    value.dispose();
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) disposeUniformTexture(item, seenTex);
  }
}

/**
 * 释放独占材质及其贴图；共享材质（isSharedMaterial）跳过。
 * @param {import('three').Material|null|undefined} material
 * @param {{ isSharedMaterial?: (m: unknown) => boolean }} [opts]
 */
export function disposeExclusiveMaterial(material, opts = {}) {
  if (!material) return;
  const isShared = opts.isSharedMaterial || (() => false);
  if (isShared(material)) return;
  const seenTex = opts._seenTex || new Set();
  for (const slot of TEX_SLOTS) disposeTextureSlot(material, slot, seenTex);
  for (const uniform of Object.values(material.uniforms || {})) {
    disposeUniformTexture(uniform?.value, seenTex);
  }
  if (typeof material.dispose === 'function') material.dispose();
}

/**
 * 遍历 Object3D 树，释放独占 geometry / material / texture。
 * 同一资源只 dispose 一次；共享材质跳过。
 * @param {import('three').Object3D|null|undefined} root
 * @param {{ isSharedMaterial?: (m: unknown) => boolean }} [opts]
 */
export function disposeObject3D(root, opts = {}) {
  if (!root) return;
  const isShared = opts.isSharedMaterial || (() => false);
  const seenGeo = new Set();
  const seenMat = new Set();
  const seenTex = new Set();

  root.traverse((obj) => {
    if (obj.geometry && !seenGeo.has(obj.geometry)) {
      seenGeo.add(obj.geometry);
      if (typeof obj.geometry.dispose === 'function') obj.geometry.dispose();
    }
    const mats = obj.material == null
      ? []
      : (Array.isArray(obj.material) ? obj.material : [obj.material]);
    for (const m of mats) {
      if (!m || seenMat.has(m) || isShared(m)) continue;
      seenMat.add(m);
      disposeExclusiveMaterial(m, { isSharedMaterial: isShared, _seenTex: seenTex });
    }
  });
}

/**
 * 清空 Group 子节点并释放独占资源。
 * @param {import('three').Object3D} group
 * @param {{ isSharedMaterial?: (m: unknown) => boolean }} [opts]
 */
export function clearAndDispose(group, opts = {}) {
  if (!group) return;
  while (group.children.length) {
    const child = group.children[0];
    group.remove(child);
    disposeObject3D(child, opts);
  }
}
