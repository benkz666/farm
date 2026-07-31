import * as THREE from 'three'
import { OrbitControls } from 'three/addons/controls/OrbitControls.js'
import { createCropModel, createDogModel, isSharedMaterial } from './game/crops.js'
import { CROPS, STAGE_NAMES_3, STAGE_NAMES_4, stageCount } from './game/config.js'
import { cropIconHTML } from './game/cropIcons.js'
import './petPreview.css'

const mount = document.querySelector('#pet-preview-canvas')
const scene = new THREE.Scene()
scene.background = new THREE.Color(0xa4c583)
scene.fog = new THREE.Fog(0xa4c583, 10, 18)

const camera = new THREE.PerspectiveCamera(34, 1, 0.1, 100)
const renderer = new THREE.WebGLRenderer({ antialias: true })
renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2))
renderer.shadowMap.enabled = true
renderer.shadowMap.type = THREE.PCFSoftShadowMap
renderer.outputColorSpace = THREE.SRGBColorSpace
renderer.toneMapping = THREE.ACESFilmicToneMapping
renderer.toneMappingExposure = 1.05
mount.appendChild(renderer.domElement)

const controls = new OrbitControls(camera, renderer.domElement)
controls.enableDamping = true
controls.dampingFactor = 0.08
controls.minDistance = 3
controls.maxDistance = 15
controls.minPolarAngle = 0.02
controls.maxPolarAngle = Math.PI - 0.02
controls.zoomToCursor = true
controls.autoRotateSpeed = 1.25

scene.add(new THREE.HemisphereLight(0xfff4d6, 0x33543a, 2.25))
const keyLight = new THREE.DirectionalLight(0xffedc2, 2.6)
keyLight.position.set(4, 8, 6)
keyLight.castShadow = true
keyLight.shadow.mapSize.set(1024, 1024)
scene.add(keyLight)

const rimLight = new THREE.DirectionalLight(0xbad6ff, 1.2)
rimLight.position.set(-6, 4, -5)
scene.add(rimLight)

const ground = new THREE.Mesh(
  new THREE.CircleGeometry(8.5, 56),
  new THREE.MeshStandardMaterial({ color: 0x769f59, roughness: 1 }),
)
ground.rotation.x = -Math.PI / 2
ground.receiveShadow = true
scene.add(ground)

const breedDefinitions = [
  { id: 'tugou', color: 0xb08968 },
  { id: 'muyang', color: 0x8d99ae },
  { id: 'zangao', color: 0x4a3728 },
]
const dogs = new Map(breedDefinitions.map((definition) => {
  const dog = createDogModel(definition)
  dog.rotation.y = -0.34
  scene.add(dog)
  return [definition.id, dog]
}))

const CROP_BODY_NAMES = Object.freeze({
  root: '根茎', cabbage: '叶菜', cereal: '禾本科', corn: '高秆作物', bush: '果菜灌木',
  rose: '花卉', ground: '蔓生作物', low: '低矮浆果', tree: '果树', vine: '棚架藤本',
  palm: '热带乔木', pineapple: '热带草本', fungus: '菌类', money: '特殊果树',
})

const description = document.querySelector('#workbench-description')
const workbenchTitle = document.querySelector('#workbench-title')
const cropSwitcher = document.querySelector('#crop-switcher')
const cropWarehouseCount = document.querySelector('#crop-warehouse-count')
const stageSwitcher = document.querySelector('#stage-switcher')
const plantCaption = document.querySelector('#plant-caption')
const plantLedgerName = document.querySelector('#plant-ledger-name')
const plantLedgerCopy = document.querySelector('#plant-ledger-copy')
const plantLedgerBody = document.querySelector('#plant-ledger-body')
const plantLedgerCycle = document.querySelector('#plant-ledger-cycle')
const plantLedgerYield = document.querySelector('#plant-ledger-yield')

let activeWorkshop = 'animal'
let activeCrop = CROPS[0]
let activeStage = 0
let cropModel = null

const allCamera = new THREE.Vector3(7.6, 4.7, 8.6)
const singleCamera = new THREE.Vector3(3.5, 2.35, 4.2)
const cameraTarget = new THREE.Vector3()
const lookTarget = new THREE.Vector3(0, 0.62, 0)

function setPreview(breedId) {
  const showingAll = breedId === 'all'
  const spacing = 2.75
  breedDefinitions.forEach((definition, index) => {
    const dog = dogs.get(definition.id)
    dog.visible = showingAll || definition.id === breedId
    dog.position.x = showingAll ? (index - 1) * spacing : 0
  })
  cameraTarget.copy(showingAll ? allCamera : singleCamera)
  lookTarget.set(0, showingAll ? 0.62 : 0.68, 0)
  camera.position.copy(cameraTarget)
  controls.target.copy(lookTarget)
  controls.update()
  document.querySelectorAll('.breed-button').forEach((button) => {
    button.classList.toggle('is-active', button.dataset.breed === breedId)
  })
}

document.querySelector('.breed-switcher').addEventListener('click', (event) => {
  const button = event.target.closest('[data-breed]')
  if (button) setPreview(button.dataset.breed)
})

function stageOptions(crop) {
  const names = stageCount(crop) === 3 ? STAGE_NAMES_3 : STAGE_NAMES_4
  return [
    ...names.map((name, stage) => ({ id: String(stage), label: name, stage, mature: false })),
    { id: 'mature', label: '成熟', stage: names.length, mature: true },
  ]
}

function disposeCropModel(model) {
  model?.traverse((node) => {
    if (!node.isMesh) return
    node.geometry?.dispose()
    const materials = Array.isArray(node.material) ? node.material : [node.material]
    materials.forEach((material) => {
      if (material && !isSharedMaterial(material)) material.dispose()
    })
  })
}

function updatePlantLedger(crop) {
  plantLedgerName.textContent = crop.name
  plantLedgerCopy.textContent = `${crop.hidden ? '特殊' : '常规'}作物，从破土幼苗到成熟收获，逐阶段检查三维轮廓。`
  plantLedgerBody.textContent = CROP_BODY_NAMES[crop.body] || '作物'
  plantLedgerCycle.textContent = `${crop.cycleH}h`
  plantLedgerYield.textContent = `${crop.yield} / 季`
}

function renderCropButtons() {
  cropWarehouseCount.textContent = `${CROPS.length} 种`
  cropSwitcher.replaceChildren(...CROPS.map((crop) => {
    const button = document.createElement('button')
    button.type = 'button'
    button.className = 'crop-button'
    button.dataset.crop = crop.id
    button.innerHTML = `${cropIconHTML(crop)}<span class="crop-button-label">${crop.name}</span>`
    button.title = `${crop.name} · ${crop.hidden ? '特殊作物' : `Lv.${crop.unlock} 解锁`}`
    return button
  }))
}

function renderStageButtons() {
  stageSwitcher.replaceChildren(...stageOptions(activeCrop).map((option) => {
    const button = document.createElement('button')
    button.type = 'button'
    button.className = 'stage-button'
    button.dataset.stage = option.id
    button.textContent = option.mature ? '✦ 成熟' : option.label
    return button
  }))
}

function frameCrop(model) {
  const bounds = new THREE.Box3().setFromObject(model)
  const size = bounds.getSize(new THREE.Vector3())
  const center = bounds.getCenter(new THREE.Vector3())
  const maxDimension = Math.max(size.x, size.y, size.z, 0.75)
  const distance = maxDimension / (2 * Math.tan(THREE.MathUtils.degToRad(camera.fov / 2))) * 1.65
  camera.position.set(center.x + distance * 0.72, center.y + distance * 0.46, center.z + distance)
  controls.target.copy(center)
  controls.minDistance = Math.max(0.42, maxDimension * 0.42)
  controls.maxDistance = Math.max(6, maxDimension * 5.2)
  controls.update()
}

function updateCropPreview({ resetCamera = true } = {}) {
  disposeCropModel(cropModel)
  if (cropModel) scene.remove(cropModel)
  const totalStages = stageCount(activeCrop)
  const mature = activeStage === 'mature'
  cropModel = createCropModel(activeCrop, {
    stage: mature ? totalStages : Number(activeStage),
    totalStages,
    mature,
    withered: false,
  })
  cropModel.rotation.y = -0.42
  cropModel.userData.baseRotationY = cropModel.rotation.y
  scene.add(cropModel)
  if (resetCamera) frameCrop(cropModel)

  cropSwitcher.querySelectorAll('.crop-button').forEach((button) => {
    button.classList.toggle('is-active', button.dataset.crop === activeCrop.id)
  })
  stageSwitcher.querySelectorAll('.stage-button').forEach((button) => {
    button.classList.toggle('is-active', button.dataset.stage === String(activeStage))
  })
  const stage = stageOptions(activeCrop).find((option) => option.id === String(activeStage))
  plantCaption.textContent = `${activeCrop.name} · ${stage?.label || '发芽'}阶段`
  updatePlantLedger(activeCrop)
}

function setCrop(cropId) {
  const crop = CROPS.find((candidate) => candidate.id === cropId)
  if (!crop) return
  activeCrop = crop
  activeStage = 0
  renderStageButtons()
  updateCropPreview()
}

cropSwitcher.addEventListener('click', (event) => {
  const button = event.target.closest('[data-crop]')
  if (button) setCrop(button.dataset.crop)
})

stageSwitcher.addEventListener('click', (event) => {
  const button = event.target.closest('[data-stage]')
  if (!button) return
  activeStage = button.dataset.stage === 'mature' ? 'mature' : Number(button.dataset.stage)
  updateCropPreview()
})

function setWorkshop(workshop) {
  if (workshop !== 'animal' && workshop !== 'plant') return
  activeWorkshop = workshop
  const isPlant = workshop === 'plant'
  dogs.forEach((dog) => { dog.visible = !isPlant })
  if (cropModel) cropModel.visible = isPlant
  document.querySelectorAll('[data-controls]').forEach((element) => {
    element.classList.toggle('is-hidden', element.dataset.controls !== workshop)
  })
  document.querySelectorAll('[data-ledger]').forEach((element) => {
    element.classList.toggle('is-hidden', element.dataset.ledger !== workshop)
  })
  document.querySelectorAll('.workshop-button').forEach((button) => {
    button.classList.toggle('is-active', button.dataset.workshop === workshop)
  })
  description.textContent = isPlant
    ? '按作物与阶段检查破土、长叶、开花到成熟的立体轮廓。'
    : '自由环绕模型，滚轮缩放，检查轮廓、毛色与动作骨架。'
  workbenchTitle.textContent = isPlant ? '植物模型工坊' : '动物模型工坊'
  document.title = `${workbenchTitle.textContent} · 经典农场`
  if (isPlant) {
    updateCropPreview()
  } else {
    controls.minDistance = 3
    controls.maxDistance = 15
    const active = document.querySelector('.breed-button.is-active')
    setPreview(active?.dataset.breed || 'all')
  }
}

document.querySelector('.workshop-switcher').addEventListener('click', (event) => {
  const button = event.target.closest('[data-workshop]')
  if (button) setWorkshop(button.dataset.workshop)
})

const autoRotateButton = document.querySelector('#auto-rotate')
function setAutoRotate(enabled) {
  controls.autoRotate = enabled
  autoRotateButton.setAttribute('aria-pressed', String(enabled))
  autoRotateButton.querySelector('.auto-rotate-label').textContent = enabled ? '自动旋转：开' : '自动旋转：关'
}
autoRotateButton.addEventListener('click', () => setAutoRotate(!controls.autoRotate))

renderer.domElement.addEventListener('dblclick', () => {
  if (activeWorkshop === 'plant') updateCropPreview()
  else {
    const active = document.querySelector('.breed-button.is-active')
    setPreview(active?.dataset.breed || 'all')
  }
})

const clock = new THREE.Clock()
function resize() {
  const width = mount.clientWidth
  const height = mount.clientHeight
  camera.aspect = width / Math.max(1, height)
  camera.updateProjectionMatrix()
  renderer.setSize(width, height, false)
}

new ResizeObserver(resize).observe(mount)
renderCropButtons()
renderStageButtons()
updateCropPreview({ resetCamera: false })
cropModel.visible = false
setPreview('all')
camera.position.copy(cameraTarget)

renderer.setAnimationLoop(() => {
  const elapsed = clock.getElapsedTime()
  // 相机绕到地面以下时隐藏承影圆盘，确保模型底部也能完整检查。
  ground.visible = camera.position.y >= -0.02
  if (activeWorkshop === 'animal') {
    for (const [index, dog] of [...dogs.values()].entries()) {
      if (!dog.visible) continue
      const rig = dog.userData.rig
      rig.root.position.y = Math.sin(elapsed * 1.45 + index) * 0.008
      rig.head.rotation.y = Math.sin(elapsed * 0.7 + index * 1.2) * 0.08
      rig.tail.rotation.y = Math.sin(elapsed * 2.3 + index) * 0.14
    }
  } else if (cropModel) {
    cropModel.rotation.y = cropModel.userData.baseRotationY + Math.sin(elapsed * 0.42) * 0.035
    cropModel.position.y = Math.sin(elapsed * 1.15) * 0.008
  }
  controls.update()
  renderer.render(scene, camera)
})
