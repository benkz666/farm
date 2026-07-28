import test from 'node:test'
import assert from 'node:assert/strict'
import * as THREE from 'three'

import { mat, isSharedMaterial } from './crops.js'

test('mat() 返回的材质标记为共享，isSharedMaterial 为 true', () => {
  const a = mat(0xff0000)
  const b = mat(0xff0000)
  assert.equal(a, b, '同色应复用缓存实例')
  assert.equal(isSharedMaterial(a), true)
  assert.equal(isSharedMaterial(b), true)
})

test('普通新建材质不是共享材质', () => {
  const exclusive = new THREE.MeshLambertMaterial({ color: 0x00ff00 })
  assert.equal(isSharedMaterial(exclusive), false)
  assert.equal(isSharedMaterial(null), false)
  assert.equal(isSharedMaterial(undefined), false)
})

test('带 emissive 的 mat 也是共享，且与无 emissive 不同实例', () => {
  const plain = mat(0x112233)
  const lit = mat(0x112233, 0xffd970)
  assert.notEqual(plain, lit)
  assert.equal(isSharedMaterial(lit), true)
})
