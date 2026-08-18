import * as THREE from 'three';

import { isSharedMaterial, mat } from './crops.js';
import { disposeObject3D } from './dispose3d.js';
import { batchStaticMeshes } from './renderOptimize.js';

const MODEL_URLS = {
  rabbit: '/models/wildlife/bunny.glb',
  caterpillar: '/models/wildlife/caterpillar.glb',
};

function mesh(geometry, color, position, scale = null, rotation = null) {
  const result = new THREE.Mesh(geometry, mat(color));
  result.position.set(...position);
  if (scale) result.scale.set(...scale);
  if (rotation) result.rotation.set(...rotation);
  result.castShadow = true;
  result.receiveShadow = true;
  return result;
}

function createRabbit(color = 0xb89a7a) {
  const rabbit = new THREE.Group();
  const body = mesh(new THREE.SphereGeometry(0.48, 7, 5), color, [0, 0.48, 0], [0.78, 0.72, 1.05]);
  const chest = mesh(new THREE.SphereGeometry(0.3, 7, 5), color, [0, 0.62, 0.38], [0.86, 1, 0.8]);
  const head = mesh(new THREE.SphereGeometry(0.29, 7, 5), color, [0, 0.82, 0.58], [0.92, 1, 0.9]);
  rabbit.add(body, chest, head);

  const ears = [];
  for (const side of [-1, 1]) {
    const ear = mesh(
      new THREE.CapsuleGeometry(0.075, 0.36, 3, 6),
      color,
      [side * 0.13, 1.2, 0.51],
      [0.8, 1, 0.72],
      [0.08, 0, -side * 0.12],
    );
    const inner = mesh(
      new THREE.CapsuleGeometry(0.035, 0.26, 2, 5),
      0xdca7a3,
      [side * 0.13, 1.21, 0.57],
      [0.75, 1, 0.55],
      [0.08, 0, -side * 0.12],
    );
    rabbit.add(ear, inner);
    ears.push(ear, inner);
  }
  for (const side of [-1, 1]) {
    rabbit.add(mesh(new THREE.SphereGeometry(0.035, 6, 4), 0x1d1c1a, [side * 0.16, 0.87, 0.82]));
  }
  rabbit.add(
    mesh(new THREE.SphereGeometry(0.06, 6, 4), 0x76554b, [0, 0.77, 0.86]),
    mesh(new THREE.SphereGeometry(0.19, 7, 5), 0xe7dfd3, [0, 0.52, -0.5]),
  );
  const feet = [];
  for (const side of [-1, 1]) {
    const foot = mesh(new THREE.SphereGeometry(0.14, 6, 4), color, [side * 0.25, 0.16, -0.18], [1, 0.55, 1.5]);
    rabbit.add(foot);
    feet.push(foot);
  }
  rabbit.userData.parts = { body, chest, head, ears, feet };
  return rabbit;
}

function createDeer(color = 0xa76f43, antlers = true) {
  const deer = new THREE.Group();
  const body = mesh(new THREE.SphereGeometry(0.68, 8, 6), color, [0, 1.2, 0], [0.68, 0.72, 1.22]);
  const chest = mesh(new THREE.SphereGeometry(0.48, 7, 5), color, [0, 1.38, 0.55], [0.72, 1, 0.78]);
  const neck = mesh(new THREE.CylinderGeometry(0.2, 0.28, 0.9, 6), color, [0, 1.75, 0.72], null, [-0.38, 0, 0]);
  const head = mesh(new THREE.SphereGeometry(0.34, 7, 5), color, [0, 2.15, 1.03], [0.62, 0.72, 1.02]);
  const muzzle = mesh(new THREE.SphereGeometry(0.2, 7, 5), 0xc39670, [0, 2.08, 1.32], [0.72, 0.58, 0.9]);
  deer.add(body, chest, neck, head, muzzle);
  for (const side of [-1, 1]) {
    deer.add(
      mesh(new THREE.ConeGeometry(0.13, 0.4, 5), color, [side * 0.25, 2.45, 0.98], null, [0.08, 0, -side * 0.55]),
      mesh(new THREE.SphereGeometry(0.035, 6, 4), 0x171818, [side * 0.17, 2.22, 1.31]),
    );
  }
  deer.add(
    mesh(new THREE.SphereGeometry(0.055, 6, 4), 0x33251f, [0, 2.06, 1.5]),
    mesh(new THREE.ConeGeometry(0.15, 0.42, 5), 0xe9dfcf, [0, 1.35, -0.72], null, [-0.7, 0, 0]),
  );

  const legs = [];
  for (const x of [-0.31, 0.31]) {
    for (const z of [-0.43, 0.47]) {
      const pivot = new THREE.Group();
      pivot.position.set(x, 1.0, z);
      const leg = mesh(new THREE.CylinderGeometry(0.055, 0.075, 0.92, 5), color, [0, -0.45, 0]);
      const hoof = mesh(new THREE.BoxGeometry(0.13, 0.1, 0.2), 0x392d27, [0, -0.9, 0.04]);
      pivot.add(leg, hoof);
      deer.add(pivot);
      legs.push(pivot);
    }
  }
  if (antlers) {
    for (const side of [-1, 1]) {
      const antler = mesh(
        new THREE.CylinderGeometry(0.025, 0.04, 0.68, 5),
        0x70513b,
        [side * 0.15, 2.72, 0.96],
        null,
        [0.1, 0, -side * 0.24],
      );
      const tine = mesh(
        new THREE.CylinderGeometry(0.018, 0.025, 0.34, 5),
        0x70513b,
        [side * 0.28, 2.82, 0.99],
        null,
        [0.2, 0, -side * 0.7],
      );
      deer.add(antler, tine);
    }
  }
  deer.userData.parts = { body, head, legs };
  return deer;
}

function createBeetle(color = 0xcc3d35) {
  const beetle = new THREE.Group();
  const shell = mesh(new THREE.SphereGeometry(0.18, 7, 5), color, [0, 0.16, 0], [0.78, 0.55, 1.08]);
  const head = mesh(new THREE.SphereGeometry(0.11, 6, 4), 0x242321, [0, 0.14, 0.18], [0.9, 0.72, 0.9]);
  const seam = mesh(new THREE.BoxGeometry(0.015, 0.018, 0.3), 0x272523, [0, 0.25, -0.015]);
  beetle.add(shell, head, seam);
  for (const [x, z] of [[-0.07, -0.06], [0.07, -0.06], [-0.08, 0.07], [0.08, 0.07]]) {
    beetle.add(mesh(new THREE.SphereGeometry(0.024, 5, 3), 0x2a2825, [x, 0.245, z]));
  }
  for (const side of [-1, 1]) {
    for (const z of [-0.11, 0, 0.11]) {
      beetle.add(mesh(
        new THREE.BoxGeometry(0.18, 0.018, 0.022),
        0x282623,
        [side * 0.12, 0.09, z],
        null,
        [0, side * (0.35 + z), 0],
      ));
    }
  }
  beetle.userData.parts = { shell, head };
  return beetle;
}

function collectAnimatedParts(parts) {
  const objects = [];
  const visit = (value) => {
    if (!value) return;
    if (value.isObject3D) {
      objects.push(value);
      return;
    }
    if (Array.isArray(value)) {
      value.forEach(visit);
      return;
    }
    if (typeof value === 'object') Object.values(value).forEach(visit);
  };
  visit(parts);
  return objects;
}

export class WildlifeController {
  constructor(scene, { heightAt, loadAssets = true }) {
    this.scene = scene;
    this.heightAt = heightAt;
    this.animals = [];
    this._disposed = false;
    this._assetLoadStarted = false;
    this._assetLoadPromise = null;

    const rabbits = [
      [-26, 14, 6.0, 4.5, 0.25, 0.2, 0xb89a7a],
      [26, 15, 6.5, 4.8, 0.23, 2.3, 0xd8d0c3],
      [-20, -21, 5.5, 5.8, 0.24, 4.1, 0x9b755b],
    ];
    for (const [cx, cz, rx, rz, speed, seed, color] of rabbits) {
      this.addAnimal(createRabbit(color), 'rabbit', { cx, cz, rx, rz, speed, seed });
    }

    const deer = [
      [-39, 0, 8.5, 16.0, 0.075, 1.1, 0xa76f43, true],
      [39, -4, 8.0, 15.0, 0.07, 4.4, 0x96653f, false],
    ];
    for (const [cx, cz, rx, rz, speed, seed, color, antlers] of deer) {
      this.addAnimal(createDeer(color, antlers), 'deer', { cx, cz, rx, rz, speed, seed });
    }

    const beetles = [
      [-15.5, 11.4, 0xd64a42], [-10, -12.4, 0xd98c32], [15.5, 11.5, 0x4b7660],
      [17.2, -11.8, 0xcc3d35], [-18.2, -10.8, 0x375c4a], [9.5, 12.1, 0xd1a035],
    ];
    beetles.forEach(([cx, cz, color], index) => {
      this.addAnimal(createBeetle(color), 'beetle', {
        cx,
        cz,
        rx: 2.4 + (index % 3) * 0.55,
        rz: 2.0 + ((index + 1) % 3) * 0.45,
        speed: 0.24 + index * 0.018,
        seed: index * 1.4,
      });
    });

    if (loadAssets) this.loadAssetReplacements();
  }

  addAnimal(group, type, movement) {
    group.userData.batchStats = batchStaticMeshes(group, {
      exclude: collectAnimatedParts(group.userData.parts),
      pruneEmpty: true,
    });
    group.userData.wildlife = { type, ...movement };
    this.animals.push(group);
    this.scene.add(group);
  }

  loadAssetReplacements() {
    if (this._disposed || this._assetLoadStarted) return this._assetLoadPromise;
    this._assetLoadStarted = true;
    // GLTFLoader / SkeletonUtils are intentionally split out of the startup
    // chunk. Procedural animals are already visible while these enhancements
    // load after the first rendered frame.
    this._assetLoadPromise = import('./wildlifeAssets.js')
      .then(async ({ createAnimatedWildlifeModel, loadWildlifeModel }) => {
        if (this._disposed) return;
        const replacements = [
          ['rabbit', MODEL_URLS.rabbit, 1.15, Infinity],
          ['beetle', MODEL_URLS.caterpillar, 0.32, 3],
        ];
        const results = await Promise.allSettled(replacements.map(async ([type, url, height, limit]) => {
          const gltf = await loadWildlifeModel(url);
          this.replaceTypeWithModel(type, gltf, height, limit, createAnimatedWildlifeModel);
        }));
        const failed = results.find((result) => result.status === 'rejected');
        if (failed && !this._disposed) {
          console.warn('Wildlife model loading failed; using procedural fallback.', failed.reason);
        }
      })
      .catch((error) => {
        if (!this._disposed) console.warn('Wildlife model loading failed; using procedural fallback.', error);
      });
    return this._assetLoadPromise;
  }

  replaceTypeWithModel(type, gltf, targetHeight, limit = Infinity, createModel) {
    if (this._disposed) {
      disposeObject3D(gltf.scene);
      return;
    }
    const matches = this.animals.filter((animal) => animal.userData.wildlife.type === type).slice(0, limit);
    matches.forEach((oldAnimal, replacementIndex) => {
      const replacement = createModel(gltf, targetHeight, replacementIndex);
      replacement.userData.wildlife = oldAnimal.userData.wildlife;
      replacement.position.copy(oldAnimal.position);
      replacement.rotation.copy(oldAnimal.rotation);
      const animalIndex = this.animals.indexOf(oldAnimal);
      this.animals[animalIndex] = replacement;
      this.scene.add(replacement);
      this.scene.remove(oldAnimal);
      disposeObject3D(oldAnimal, { isSharedMaterial });
    });
  }

  update(dt, time) {
    for (const animal of this.animals) {
      const state = animal.userData.wildlife;
      animal.userData.mixer?.update(dt);
      const angle = time * state.speed + state.seed;
      const x = state.cx + Math.cos(angle) * state.rx + Math.sin(angle * 2.1 + state.seed * 0.3) * state.rx * 0.16;
      const z = state.cz + Math.sin(angle) * state.rz + Math.cos(angle * 1.7 + state.seed * 0.5) * state.rz * 0.12;
      const dx =
        -Math.sin(angle) * state.rx * state.speed +
        Math.cos(angle * 2.1 + state.seed * 0.3) * state.rx * 0.336 * state.speed;
      const dz =
        Math.cos(angle) * state.rz * state.speed -
        Math.sin(angle * 1.7 + state.seed * 0.5) * state.rz * 0.204 * state.speed;
      animal.rotation.y = Math.atan2(dx, dz);

      if (state.type === 'rabbit') {
        const hopPhase = Math.sin(time * 5.4 + state.seed);
        const hop = Math.max(0, hopPhase) ** 2 * 0.38;
        animal.position.set(x, this.heightAt(x, z) + hop, z);
        const parts = animal.userData.parts;
        if (parts) {
          parts.body.rotation.x = -hopPhase * 0.08;
          parts.chest.position.y = 0.62 + hop * 0.08;
          parts.ears.forEach((ear, index) => {
            ear.rotation.x = 0.08 + Math.sin(time * 5.4 + state.seed + index * 0.45) * 0.05;
          });
          parts.feet.forEach((foot, index) => {
            foot.position.z = -0.18 - hopPhase * 0.08 * (index ? -1 : 1);
          });
        }
      } else if (state.type === 'deer') {
        animal.position.set(x, this.heightAt(x, z), z);
        const parts = animal.userData.parts;
        parts.legs.forEach((leg, index) => {
          leg.rotation.x = Math.sin(time * 3.2 + state.seed + (index % 2) * Math.PI) * 0.22;
        });
        parts.head.rotation.x = Math.sin(time * 1.1 + state.seed) * 0.07;
        parts.body.position.y = 1.2 + Math.sin(time * 3.2 + state.seed) * 0.025;
      } else {
        animal.position.set(x, this.heightAt(x, z) + 0.015, z);
        const parts = animal.userData.parts;
        if (parts) {
          parts.shell.rotation.z = Math.sin(time * 8 + state.seed) * 0.025;
          parts.head.position.y = 0.14 + Math.sin(time * 7 + state.seed) * 0.008;
        }
      }
    }
  }

  dispose() {
    this._disposed = true;
    for (const animal of this.animals) {
      const mixer = animal.userData.mixer;
      if (!mixer) continue;
      mixer.stopAllAction();
      mixer.uncacheRoot(animal.userData.assetRoot);
    }
    this.animals = [];
    this._assetLoadPromise = null;
    this.scene = null;
    this.heightAt = null;
  }
}
