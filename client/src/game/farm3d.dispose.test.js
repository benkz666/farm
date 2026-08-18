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

test('stop 暂停 RAF 且允许无重复循环地恢复', () => {
  const { container, env, stats } = makeHarness()
  const scene = new FarmScene(container, env)
  scene.start()
  assert.equal(env.pendingRafCount(), 1)

  scene.stop()
  assert.equal(env.pendingRafCount(), 0)
  env.flushRaf()
  assert.equal(stats().renderCount, 0)

  scene.start()
  scene.start()
  assert.equal(env.pendingRafCount(), 1)
  env.flushRaf()
  assert.equal(stats().renderCount, 1)
  scene.dispose()
})

test('Shader 预热在空闲回调执行，不阻塞构造并可随 dispose 取消', async () => {
  const first = makeHarness()
  const firstRenderer = first.env.createRenderer()
  let idleCallback = null
  let compileCount = 0
  firstRenderer.compileAsync = async (scene, camera) => {
    assert.ok(scene?.isScene)
    assert.ok(camera?.isCamera)
    compileCount++
  }
  first.env.createRenderer = () => firstRenderer
  first.env.requestIdleCallback = (callback) => {
    idleCallback = callback
    return 71
  }
  const scene = new FarmScene(first.container, first.env)
  assert.equal(compileCount, 0)
  scene.start()
  first.env.flushRaf()
  assert.equal(typeof idleCallback, 'function')
  idleCallback()
  await Promise.resolve()
  assert.equal(compileCount, 1)
  scene.dispose()

  const second = makeHarness()
  const secondRenderer = second.env.createRenderer()
  let cancelledId = null
  secondRenderer.compileAsync = async () => { throw new Error('不应执行') }
  second.env.createRenderer = () => secondRenderer
  second.env.requestIdleCallback = () => 93
  second.env.cancelIdleCallback = (id) => { cancelledId = id }
  const disposable = new FarmScene(second.container, second.env)
  disposable.start()
  second.env.flushRaf()
  disposable.dispose()
  assert.equal(cancelledId, 93)
})

test('野生动物 GLB 在首帧完成后的空闲阶段才开始加载', () => {
  const harness = makeHarness()
  const idleCallbacks = []
  harness.env.loadWildlifeAssets = true
  harness.env.requestIdleCallback = (callback) => {
    idleCallbacks.push(callback)
    return idleCallbacks.length
  }
  const scene = new FarmScene(harness.container, harness.env)
  let loadCalls = 0
  scene.wildlife.loadAssetReplacements = () => { loadCalls++ }

  assert.equal(loadCalls, 0)
  assert.equal(idleCallbacks.length, 0)
  scene.start()
  harness.env.flushRaf()
  assert.equal(loadCalls, 0)
  assert.equal(idleCallbacks.length, 1)
  idleCallbacks[0]()
  assert.equal(loadCalls, 1)
  scene.dispose()
})

test('环境与成熟作物保持原材质画质并合批，粒子一次操作只占一个 draw call', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)
  assert.ok(scene.staticBatchStats.sourceMeshes > 800)
  assert.ok(scene.staticBatchStats.drawCallReduction > 700)
  assert.equal(scene.scene.matrixAutoUpdate, false)
  const staticBatches = scene.scene.children.filter((object) => object.name.startsWith('static-batch-'))
  assert.ok(staticBatches.length > 40)
  assert.ok(staticBatches.every((object) => object.matrixWorldAutoUpdate === false))

  assert.equal(scene.fountainDroplets.isInstancedMesh, true)
  assert.equal(scene.fountainDroplets.count, 16)
  assert.equal(scene.fountainDroplets.geometry.type, 'SphereGeometry')
  assert.equal(scene.fountainDroplets.material.isMeshPhysicalMaterial, true)
  const dropletIndex = 7
  const dropletTime = 0.37
  scene.updateFountainDroplets(dropletTime)
  const dropletMatrix = new THREE.Matrix4()
  const dropletPosition = new THREE.Vector3()
  const dropletScale = new THREE.Vector3()
  scene.fountainDroplets.getMatrixAt(dropletIndex, dropletMatrix)
  dropletMatrix.decompose(dropletPosition, new THREE.Quaternion(), dropletScale)
  const expectedAngle = Math.PI * 3 / 4
  const expectedCycle = (dropletTime * 0.72 + 0.81) % 1
  const expectedRadius = 1.45 * expectedCycle
  const expectedHeight = 0.78 * (1 - expectedCycle) + 0.08 * expectedCycle + 2.65 * expectedCycle * (1 - expectedCycle)
  const expectedSize = 0.58 + Math.sin(expectedCycle * Math.PI) * 0.62
  assert.ok(dropletPosition.distanceTo(new THREE.Vector3(
    Math.cos(expectedAngle) * expectedRadius,
    expectedHeight,
    Math.sin(expectedAngle) * expectedRadius,
  )) < 1e-6, '实例化水滴应保持原抛物线轨迹')
  assert.ok(dropletScale.distanceTo(new THREE.Vector3(expectedSize, expectedSize, expectedSize)) < 1e-6)

  const group = scene.plotGroups[0]
  scene.updatePlot(group, {
    unlocked: true,
    lockText: '',
    state: PLOT.MATURE,
    cropDef: CROP_MAP.caomei,
    stage: 3,
    totalStages: 4,
    dry: false,
    weed: false,
    pest: false,
  })
  let cropMeshes = 0
  group.userData.cropGroup.traverse((object) => { if (object.isMesh) cropMeshes++ })
  assert.ok(cropMeshes <= 12, `成熟草莓合批后 Mesh 数应不超过 12，实际 ${cropMeshes}`)

  scene.burst(0, 0xffd54f, 24, true)
  assert.equal(scene.particles.length, 1)
  assert.equal(scene.particles[0].isInstancedMesh, true)
  assert.equal(scene.particles[0].count, 24)
  assert.equal(scene.particles[0].matrixAutoUpdate, false)
  scene.dispose()
})

test('pointermove 只在帧内处理最后一次坐标', () => {
  const { container, canvas, env } = makeHarness()
  const scene = new FarmScene(container, env)
  const hovered = []
  scene.hoverCb = (plotId, x, y) => hovered.push([plotId, x, y])

  canvas.dispatchEvent('pointermove', { clientX: 10, clientY: 20 })
  canvas.dispatchEvent('pointermove', { clientX: 30, clientY: 40 })
  assert.equal(hovered.length, 0)
  scene.flushPointerMove()
  assert.equal(hovered.length, 1)
  assert.deepEqual(hovered[0].slice(1), [30, 40])
  scene.dispose()
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
    assert.equal(group.matrixAutoUpdate, false)
    assert.equal(base.matrixAutoUpdate, false)
    assert.equal(rim.matrixAutoUpdate, false)
    assert.equal(content.matrixAutoUpdate, false)
    assert.equal(matureFx.visible, false)
    assert.equal(matureFx.userData.plotIndex, group.userData.base.userData.plotId)
    assert.equal(furrows.isInstancedMesh, true)
    assert.equal(furrows.count, 3)
    assert.equal(furrows.geometry.type, 'BoxGeometry')
    furrows.geometry.computeBoundingBox()
    const furrowSize = furrows.geometry.boundingBox.getSize(new THREE.Vector3())
    assert.ok(Math.abs(furrowSize.y - 0.05) < 1e-6)
    const furrowMatrix = new THREE.Matrix4()
    for (let instanceId = 0; instanceId < furrows.count; instanceId++) {
      furrows.getMatrixAt(instanceId, furrowMatrix)
      const furrowPosition = new THREE.Vector3().setFromMatrixPosition(furrowMatrix)
      assert.ok(Math.abs(furrowPosition.y - 0.207) < 1e-6)
      assert.ok(Math.abs(furrowPosition.z - (instanceId - 1) * 1.22) < 1e-6)
      assert.ok(furrowPosition.y - furrowSize.y / 2 > base.position.y, '土垄不能与土层共面')
    }
  })

  const first = scene.plotGroups[0].userData
  scene.plotGroups.slice(1).forEach((group) => {
    const current = group.userData
    assert.equal(current.rim.geometry, first.rim.geometry)
    assert.equal(current.base.geometry, first.base.geometry)
    assert.equal(current.furrows.geometry, first.furrows.geometry)
    assert.equal(current.ring.geometry, first.ring.geometry)
    assert.equal(current.ring.material, first.ring.material)
    assert.equal(current.sign.children[0].geometry, first.sign.children[0].geometry)
    assert.equal(current.signBoard.geometry, first.signBoard.geometry)
  })

  assert.equal(scene.matureEffectPool.mesh.isInstancedMesh, true)
  assert.equal(scene.matureEffectPool.mesh.geometry.type, 'OctahedronGeometry')
  assert.equal(scene.matureEffectPool.mesh.material.transparent, true)
  assert.equal(scene.matureEffectPool.mesh.material.depthWrite, false)
  assert.equal(scene.matureEffectPool.mesh.count, 0)

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
  scene.matureEffectPool.update(1, scene.camera)
  assert.equal(scene.matureEffectPool.mesh.visible, true)
  assert.equal(scene.matureEffectPool.mesh.count, 6)

  scene.updatePlot(group, { ...info, state: PLOT.GROWING, stage: 2 })
  assert.equal(group.userData.matureFx.visible, false)
  scene.matureEffectPool.update(1, scene.camera)
  assert.equal(scene.matureEffectPool.mesh.visible, false)
  assert.equal(scene.matureEffectPool.mesh.count, 0)
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

test('高空飞鸟保持飞行轨迹并使用缩小后的模型比例', () => {
  const { container, env } = makeHarness()
  const scene = new FarmScene(container, env)

  assert.equal(scene.birds.length, 3)
  scene.birds.forEach((bird, index) => {
    const expectedScale = 0.6 * (0.98 + index * 0.05)
    assert.ok(Math.abs(bird.scale.x - expectedScale) < 1e-9)
    assert.equal(bird.scale.x, bird.scale.y)
    assert.equal(bird.scale.y, bird.scale.z)
    assert.equal(bird.userData.radius, 13 + index * 3.5)
    assert.equal(bird.userData.height, 12 + index * 1.8)
    assert.ok(bird.userData.leftWing && bird.userData.rightWing)
    assert.deepEqual(bird.userData.batchStats, {
      sourceMeshes: 6,
      batches: 3,
      drawCallReduction: 3,
    })
    let renderableCount = 0
    bird.traverse((object) => { if (object.isMesh) renderableCount++ })
    assert.equal(renderableCount, 9)
  })

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
