import test from 'node:test'
import assert from 'node:assert/strict'
import * as THREE from 'three'

import { mat, isSharedMaterial } from './crops.js'
import { disposeObject3D, disposeExclusiveMaterial } from './dispose3d.js'

function trackDispose(obj) {
  let n = 0
  const orig = obj.dispose.bind(obj)
  obj.dispose = () => {
    n++
    orig()
  }
  return () => n
}

test('disposeObject3D 释放独占 geometry / material / map，不碰共享 mat()', () => {
  const shared = mat(0x58a05a)
  const sharedDisposed = trackDispose(shared)

  const map = new THREE.CanvasTexture(makeCanvas())
  const mapDisposed = trackDispose(map)
  const exclusiveMat = new THREE.MeshLambertMaterial({ map })
  const matDisposed = trackDispose(exclusiveMat)
  const geo = new THREE.BoxGeometry(1, 1, 1)
  const geoDisposed = trackDispose(geo)

  const root = new THREE.Group()
  root.add(new THREE.Mesh(geo, exclusiveMat))
  root.add(new THREE.Mesh(new THREE.SphereGeometry(0.5, 4, 3), shared))

  disposeObject3D(root, { isSharedMaterial })

  assert.equal(geoDisposed(), 1)
  assert.equal(matDisposed(), 1)
  assert.equal(mapDisposed(), 1)
  assert.equal(sharedDisposed(), 0, 'matCache 共享材质不得被 dispose')
})

test('disposeObject3D 对 material 数组逐项处理，共享项跳过', () => {
  const shared = mat(0xa9825a)
  const sharedDisposed = trackDispose(shared)
  const exclusive = new THREE.MeshBasicMaterial({ color: 0xff00ff })
  const exclusiveDisposed = trackDispose(exclusive)
  const geo = new THREE.PlaneGeometry(1, 1)
  const geoDisposed = trackDispose(geo)

  const mesh = new THREE.Mesh(geo, [shared, exclusive])
  disposeObject3D(mesh, { isSharedMaterial })

  assert.equal(geoDisposed(), 1)
  assert.equal(sharedDisposed(), 0)
  assert.equal(exclusiveDisposed(), 1)
})

test('disposeObject3D 幂等：重复调用不抛错且共享材质仍可用', () => {
  const shared = mat(0xbd9268)
  const mesh = new THREE.Mesh(
    new THREE.BoxGeometry(0.5, 0.5, 0.5),
    new THREE.MeshBasicMaterial({ color: 0x111111 }),
  )
  disposeObject3D(mesh, { isSharedMaterial })
  disposeObject3D(mesh, { isSharedMaterial })
  assert.equal(isSharedMaterial(shared), true)
  // 共享材质仍可赋给新 mesh（未被销毁）
  const again = new THREE.Mesh(new THREE.BoxGeometry(0.2, 0.2, 0.2), shared)
  assert.equal(again.material, shared)
})

test('disposeExclusiveMaterial 释放独占材质与 map，跳过共享', () => {
  const shared = mat(0x9db98a)
  const sharedDisposed = trackDispose(shared)
  disposeExclusiveMaterial(shared, { isSharedMaterial })
  assert.equal(sharedDisposed(), 0)

  const map = new THREE.CanvasTexture(makeCanvas())
  const mapDisposed = trackDispose(map)
  const exclusive = new THREE.MeshLambertMaterial({ map })
  const matDisposed = trackDispose(exclusive)
  disposeExclusiveMaterial(exclusive, { isSharedMaterial })
  assert.equal(matDisposed(), 1)
  assert.equal(mapDisposed(), 1)
})

function makeCanvas() {
  // Node 无 DOM：用最小假 canvas 满足 CanvasTexture 构造
  return {
    width: 8,
    height: 8,
    getContext: () => null,
  }
}
