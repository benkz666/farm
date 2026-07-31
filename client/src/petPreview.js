import * as THREE from 'three'
import { OrbitControls } from 'three/addons/controls/OrbitControls.js'
import { createDogModel } from './game/crops.js'
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

const autoRotateButton = document.querySelector('#auto-rotate')
function setAutoRotate(enabled) {
  controls.autoRotate = enabled
  autoRotateButton.setAttribute('aria-pressed', String(enabled))
  autoRotateButton.querySelector('.auto-rotate-label').textContent = enabled ? '自动旋转：开' : '自动旋转：关'
}
autoRotateButton.addEventListener('click', () => setAutoRotate(!controls.autoRotate))

renderer.domElement.addEventListener('dblclick', () => {
  const active = document.querySelector('.breed-button.is-active')
  setPreview(active?.dataset.breed || 'all')
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
setPreview('all')
camera.position.copy(cameraTarget)

renderer.setAnimationLoop(() => {
  const elapsed = clock.getElapsedTime()
  // 相机绕到地面以下时隐藏承影圆盘，确保模型底部也能完整检查。
  ground.visible = camera.position.y >= -0.02
  for (const [index, dog] of [...dogs.values()].entries()) {
    if (!dog.visible) continue
    const rig = dog.userData.rig
    rig.root.position.y = Math.sin(elapsed * 1.45 + index) * 0.008
    rig.head.rotation.y = Math.sin(elapsed * 0.7 + index * 1.2) * 0.08
    rig.tail.rotation.y = Math.sin(elapsed * 2.3 + index) * 0.14
  }
  controls.update()
  renderer.render(scene, camera)
})
