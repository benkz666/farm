import test from 'node:test'
import assert from 'node:assert/strict'
import * as THREE from 'three'

import { FarmScene } from './farm3d.js'
import { CROP_MAP } from './config.js'
import { mat, isSharedMaterial } from './crops.js'
import { PLOT } from './state.js'

function makeFakeCanvas() {
  const listeners = new Map()
  return {
    style: {},
    width: 8,
    height: 8,
    clientWidth: 800,
    clientHeight: 600,
    getContext: () => ({
      fillStyle: '',
      font: '',
      textAlign: '',
      textBaseline: '',
      fillRect() {},
      fillText() {},
    }),
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set())
      listeners.get(type).add(fn)
    },
    removeEventListener(type, fn) {
      listeners.get(type)?.delete(fn)
    },
    dispatchEvent(type, ev = {}) {
      for (const fn of [...(listeners.get(type) || [])]) fn({ clientX: 0, clientY: 0, ...ev })
    },
    listenerCount(type) {
      return listeners.get(type)?.size || 0
    },
  }
}

function makeHarness() {
  const windowListeners = new Map()
  const canvas = makeFakeCanvas()
  let rafId = 0
  const rafPending = new Map()
  let renderCount = 0
  let rendererDisposed = 0
  let renderListsDisposed = 0
  let controlsDisposed = 0
  const removedChildren = []

  const container = {
    children: [],
    appendChild(el) {
      this.children.push(el)
      return el
    },
    removeChild(el) {
      const i = this.children.indexOf(el)
      if (i >= 0) this.children.splice(i, 1)
      removedChildren.push(el)
      return el
    },
    contains(el) {
      return this.children.includes(el)
    },
  }

  const renderer = {
    domElement: canvas,
    shadowMap: { enabled: false, type: 0 },
    toneMapping: 0,
    toneMappingExposure: 1,
    setPixelRatio() {},
    setSize() {},
    render() { renderCount++ },
    dispose() { rendererDisposed++ },
    renderLists: { dispose() { renderListsDisposed++ } },
  }

  const controls = {
    target: new THREE.Vector3(),
    enableDamping: false,
    dampingFactor: 0,
    minDistance: 0,
    maxDistance: 0,
    maxPolarAngle: 0,
    minPolarAngle: 0,
    enablePan: false,
    update() {},
    dispose() { controlsDisposed++ },
  }

  const env = {
    createRenderer: () => renderer,
    createControls: () => controls,
    requestAnimationFrame(cb) {
      const id = ++rafId
      rafPending.set(id, cb)
      return id
    },
    cancelAnimationFrame(id) {
      rafPending.delete(id)
    },
    addEventListener(type, fn) {
      if (!windowListeners.has(type)) windowListeners.set(type, new Set())
      windowListeners.get(type).add(fn)
    },
    removeEventListener(type, fn) {
      windowListeners.get(type)?.delete(fn)
    },
    createElement(tag) {
      if (tag === 'canvas') return makeFakeCanvas()
      return { style: {} }
    },
    getViewport: () => ({ width: 800, height: 600, pixelRatio: 1 }),
    flushRaf() {
      const entries = [...rafPending.entries()]
      rafPending.clear()
      for (const [, cb] of entries) cb(performance.now())
    },
    pendingRafCount() {
      return rafPending.size
    },
    windowListenerCount(type) {
      return windowListeners.get(type)?.size || 0
    },
  }

  return {
    container,
    canvas,
    env,
    stats: () => ({
      renderCount,
      rendererDisposed,
      renderListsDisposed,
      controlsDisposed,
      removedChildren,
      containerHasCanvas: container.contains(canvas),
    }),
  }
}

test('FarmScene.dispose：停止 RAF、移除监听、释放 controls/renderer，幂等', () => {
  const { container, canvas, env, stats } = makeHarness()
  const scene = new FarmScene(container, env)
  assert.equal(env.windowListenerCount('resize'), 1)
  assert.equal(canvas.listenerCount('pointerdown'), 1)
  assert.equal(canvas.listenerCount('pointerup'), 1)
  assert.equal(canvas.listenerCount('pointermove'), 1)

  scene.start()
  assert.equal(env.pendingRafCount(), 1)
  env.flushRaf()
  const afterOneFrame = stats().renderCount
  assert.ok(afterOneFrame >= 1, 'start 后应至少 render 一次')
  assert.equal(env.pendingRafCount(), 1, '每帧应再排下一帧')

  scene.start()
  assert.equal(env.pendingRafCount(), 1, '重复 start 不得双 RAF')

  scene.dispose()
  const mid = stats()
  assert.equal(env.pendingRafCount(), 0, 'dispose 后取消待执行 RAF')
  assert.equal(env.windowListenerCount('resize'), 0)
  assert.equal(canvas.listenerCount('pointerdown'), 0)
  assert.equal(canvas.listenerCount('pointerup'), 0)
  assert.equal(canvas.listenerCount('pointermove'), 0)
  assert.equal(mid.controlsDisposed, 1)
  assert.equal(mid.rendererDisposed, 1)
  assert.equal(mid.renderListsDisposed, 1)
  assert.equal(mid.containerHasCanvas, false)

  const rendersAfterDispose = mid.renderCount
  env.flushRaf()
  assert.equal(stats().renderCount, rendersAfterDispose, 'dispose 后不得再 render')

  scene.dispose()
  assert.equal(stats().rendererDisposed, 1, 'dispose 幂等')
  assert.equal(stats().controlsDisposed, 1)
})

test('dispose 后 start 不得再排程 / render', () => {
  const { container, env, stats } = makeHarness()
  const scene = new FarmScene(container, env)
  scene.dispose()
  scene.start()
  assert.equal(env.pendingRafCount(), 0)
  env.flushRaf()
  assert.equal(stats().renderCount, 0)
})

test('地块使用半埋式低矮倒角菜畦，不回退为高方块或纯平贴片', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)

  scene.plotGroups.forEach((group) => {
    const { base, rim, furrows, content, ring, matureFx } = group.userData
    assert.equal(base.geometry.type, 'PlaneGeometry')
    assert.equal(rim.geometry.type, 'BoxGeometry')
    rim.geometry.computeBoundingBox()
    const rimSize = rim.geometry.boundingBox.getSize(new THREE.Vector3())
    assert.ok(Math.abs(rimSize.y - 0.22) < 1e-6)
    assert.equal(rim.position.y, 0.05)
    const rimTop = rim.position.y + rimSize.y / 2
    assert.ok(base.position.y - rimTop >= 0.019, '土层应与菜畦顶面保持足够深度间距')
    assert.equal(base.position.y, 0.18)
    assert.equal(content.position.y, 0.19)
    assert.equal(ring.position.y, 0.29)
    assert.equal(matureFx.visible, false)
    assert.equal(matureFx.children.length, 6)
    assert.ok(matureFx.children.every((mote) => mote.geometry.type === 'OctahedronGeometry'))
    assert.ok(matureFx.userData.flare, '成熟特效应有中心星芒')
    furrows.children.forEach((furrow) => {
      assert.equal(furrow.geometry.type, 'BoxGeometry')
      furrow.geometry.computeBoundingBox()
      const size = furrow.geometry.boundingBox.getSize(new THREE.Vector3())
      assert.ok(Math.abs(size.y - 0.05) < 1e-6)
      assert.ok(furrow.position.y - size.y / 2 > base.position.y, '土垄不能与土层共面')
    })
  })

  scene.dispose()
})

test('成熟状态显示花粉光点，离开成熟状态后隐藏', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)
  const group = scene.plotGroups[0]
  const info = {
    unlocked: true,
    lockText: '',
    state: PLOT.MATURE,
    cropDef: CROP_MAP.bailuobo,
    stage: 3,
    totalStages: 3,
    dry: false,
    weed: false,
    pest: false,
  }

  scene.updatePlot(group, info)
  assert.equal(group.userData.matureFx.visible, true)

  scene.updatePlot(group, { ...info, state: PLOT.GROWING, stage: 2 })
  assert.equal(group.userData.matureFx.visible, false)
  scene.dispose()
})

test('地面云影使用柔边透明着色器，并随昼夜调整强度', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)

  assert.equal(scene.cloudShadows.length, scene.clouds.length)
  assert.equal(scene.cloudShadows.length, 5)
  scene.cloudShadows.forEach((shadow, index) => {
    assert.equal(shadow.geometry.type, 'PlaneGeometry')
    assert.equal(shadow.material.isShaderMaterial, true)
    assert.equal(shadow.material.transparent, true)
    assert.equal(shadow.material.depthWrite, false)
    assert.ok(shadow.userData.baseOpacity >= 0.09 && shadow.userData.baseOpacity <= 0.14)
    assert.equal(scene.clouds[index].userData.shadow, shadow)
  })

  scene.setDayPhase(0)
  assert.equal(scene.sun.shadow.intensity, 0)
  assert.ok(scene.cloudShadows.every((shadow) => shadow.material.uniforms.uOpacity.value === 0))
  scene.setDayPhase(0.5)
  assert.equal(scene.sun.shadow.intensity, 0.72)
  assert.ok(scene.cloudShadows.every((shadow) => shadow.material.uniforms.uOpacity.value > 0))

  scene.dispose()
})

test('updatePlot 重建内容时释放旧独占资源；共享 mat 仍可用', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)
  const g = scene.plotGroups[0]
  const shared = mat(0xb5977a)
  let sharedDisposed = 0
  const origShared = shared.dispose.bind(shared)
  shared.dispose = () => { sharedDisposed++; origShared() }

  scene.updatePlot(g, {
    unlocked: true,
    lockText: '',
    state: PLOT.WASTELAND,
    cropDef: null,
    stage: 0,
    totalStages: 3,
    dry: false,
    weed: false,
    pest: false,
  })
  assert.ok(g.userData.content.children.length > 0)

  // 给 content 挂一个独占 mesh，模拟重建前的旧独占资源
  const map = new THREE.CanvasTexture(env.createElement('canvas'))
  let mapDisposed = 0
  const origMap = map.dispose.bind(map)
  map.dispose = () => { mapDisposed++; origMap() }
  const exclusiveMat = new THREE.MeshBasicMaterial({ map })
  let matDisposed = 0
  const origMat = exclusiveMat.dispose.bind(exclusiveMat)
  exclusiveMat.dispose = () => { matDisposed++; origMat() }
  const geo = new THREE.BoxGeometry(0.3, 0.3, 0.3)
  let geoDisposed = 0
  const origGeo = geo.dispose.bind(geo)
  geo.dispose = () => { geoDisposed++; origGeo() }
  g.userData.content.add(new THREE.Mesh(geo, exclusiveMat))
  g.userData.key = 'force-rebuild'

  scene.updatePlot(g, {
    unlocked: true,
    lockText: '',
    state: PLOT.TILLED,
    cropDef: null,
    stage: 0,
    totalStages: 3,
    dry: false,
    weed: false,
    pest: false,
  })

  assert.equal(geoDisposed, 1)
  assert.equal(matDisposed, 1)
  assert.equal(mapDisposed, 1)
  assert.equal(sharedDisposed, 0)
  assert.equal(isSharedMaterial(shared), true)
})

test('setDog 替换/移除时释放旧 dog 的独占 geometry，不 dispose 共享材质', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)
  const shared = mat(0xffaa00)
  let sharedDisposed = 0
  const orig = shared.dispose.bind(shared)
  shared.dispose = () => { sharedDisposed++; orig() }

  scene.setDog({ id: 'a', color: 0xffaa00 }, false)
  assert.ok(scene.dogGroup)
  assert.ok(scene.dogBehavior)
  assert.ok(scene.dogGroup.userData.rig)
  assert.equal(scene.dogGroup.userData.rig.frontLegs.length, 2)
  assert.equal(scene.dogGroup.userData.rig.hindLegs.length, 2)
  const first = scene.dogGroup
  const geos = []
  first.traverse((o) => {
    if (o.geometry) {
      let n = 0
      const od = o.geometry.dispose.bind(o.geometry)
      o.geometry.dispose = () => { n++; od() }
      geos.push(() => n)
    }
  })

  scene.setDog({ id: 'b', color: 0x00aaff }, false)
  assert.notEqual(scene.dogGroup, first)
  assert.ok(geos.some((c) => c() === 1), '旧 dog geometry 应被释放')

  scene.setDog(null, false)
  assert.equal(scene.dogGroup, null)
  assert.equal(scene.dogBehavior, null)
  assert.equal(sharedDisposed, 0)
})

test('setDog 按宠物定义创建对应品种模型', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)

  scene.setDog({ id: 'muyang', color: 0x8d99ae }, false)
  assert.equal(scene.dogGroup.userData.breedId, 'muyang')
  assert.ok(scene.dogGroup.userData.rig.markings.saddle)

  scene.setDog({ id: 'zangao', color: 0x4a3728 }, false)
  assert.equal(scene.dogGroup.userData.breedId, 'zangao')
  assert.ok(scene.dogGroup.userData.rig.markings.mane)

  scene.dispose()
})

test('锁定牌材质/CanvasTexture 替换时释放旧独占资源', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)
  const g = scene.plotGroups[0]
  const board = g.userData.signBoard
  const sharedBoardMat = board.material
  assert.equal(isSharedMaterial(sharedBoardMat), true)

  scene.updatePlot(g, {
    unlocked: false,
    lockText: 'Lv.2',
    state: PLOT.WASTELAND,
    cropDef: null,
    stage: 0,
    totalStages: 3,
    dry: false,
    weed: false,
    pest: false,
  })
  const firstMat = board.material
  assert.notEqual(firstMat, sharedBoardMat)
  assert.ok(firstMat.map)
  let firstMatDisposed = 0
  let firstMapDisposed = 0
  const om = firstMat.dispose.bind(firstMat)
  firstMat.dispose = () => { firstMatDisposed++; om() }
  const omap = firstMat.map.dispose.bind(firstMat.map)
  firstMat.map.dispose = () => { firstMapDisposed++; omap() }

  g.userData.key = 'force'
  scene.updatePlot(g, {
    unlocked: false,
    lockText: 'Lv.5',
    state: PLOT.WASTELAND,
    cropDef: null,
    stage: 0,
    totalStages: 3,
    dry: false,
    weed: false,
    pest: false,
  })

  assert.equal(firstMatDisposed, 1)
  assert.equal(firstMapDisposed, 1)
  assert.notEqual(board.material, firstMat)
  // 初始共享材质未被误 dispose
  assert.equal(isSharedMaterial(sharedBoardMat), true)
})
