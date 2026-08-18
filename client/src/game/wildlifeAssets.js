import * as THREE from 'three'
import { GLTFLoader } from 'three/examples/jsm/loaders/GLTFLoader.js'
import { clone as cloneSkeleton } from 'three/examples/jsm/utils/SkeletonUtils.js'

const loader = new GLTFLoader()

export function loadWildlifeModel(url) {
  return loader.loadAsync(url)
}

export function createAnimatedWildlifeModel(gltf, targetHeight, animationSeed) {
  const model = cloneSkeleton(gltf.scene)
  model.traverse((child) => {
    if (!child.isMesh) return
    child.castShadow = true
    child.receiveShadow = true
  })

  model.updateMatrixWorld(true)
  const initialBounds = new THREE.Box3().setFromObject(model)
  const initialHeight = Math.max(0.001, initialBounds.max.y - initialBounds.min.y)
  model.scale.setScalar(targetHeight / initialHeight)
  model.updateMatrixWorld(true)
  const bounds = new THREE.Box3().setFromObject(model)
  const center = bounds.getCenter(new THREE.Vector3())
  model.position.set(-center.x, -bounds.min.y, -center.z)

  const wrapper = new THREE.Group()
  wrapper.add(model)
  wrapper.userData.assetRoot = model
  if (gltf.animations.length) {
    const mixer = new THREE.AnimationMixer(model)
    const movementClip =
      gltf.animations.find((clip) => /walk|run|jump|crawl/i.test(clip.name)) ||
      gltf.animations.find((clip) => /idle/i.test(clip.name)) ||
      gltf.animations[0]
    const action = mixer.clipAction(movementClip)
    action.timeScale = 0.82 + (animationSeed % 3) * 0.09
    action.play()
    wrapper.userData.mixer = mixer
  }
  return wrapper
}
