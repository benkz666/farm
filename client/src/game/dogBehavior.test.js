import test from 'node:test'
import assert from 'node:assert/strict'

import { createDogModel } from './crops.js'
import {
  DOG_STATES,
  DogBehaviorController,
  dogPathPoint,
  isDogWalkablePoint,
} from './dogBehavior.js'

function sequenceRandom(values) {
  let index = 0
  return () => values[index++ % values.length]
}

test('看家狗模型放大且具有可见的双眼骨架节点', () => {
  const dog = createDogModel(0xb08968)
  const { rig } = dog.userData

  assert.equal(dog.scale.x, 1.45)
  assert.equal(dog.scale.y, 1.45)
  assert.equal(rig.eyes.length, 2)
  assert.equal(rig.eyeHighlights.length, 2)
  assert.ok(rig.eyes.every((eye) => eye.isMesh && eye.castShadow))
  assert.ok(rig.eyes.every((eye) => eye.position.x > 0.3 && Math.abs(eye.position.z) > 0.2))
})

test('牧羊犬与藏獒具有独立体型和品种标志', () => {
  const shepherd = createDogModel({ id: 'muyang', color: 0x8d99ae })
  const mastiff = createDogModel({ id: 'zangao', color: 0x4a3728 })

  assert.equal(shepherd.userData.breedId, 'muyang')
  assert.ok(shepherd.userData.rig.markings.saddle?.isMesh)
  assert.ok(shepherd.userData.rig.markings.blaze?.isMesh)
  assert.ok(shepherd.userData.rig.body.scale.x > 1.6, '牧羊犬躯干应更修长')

  assert.equal(mastiff.userData.breedId, 'zangao')
  assert.ok(mastiff.userData.rig.markings.mane?.isMesh)
  assert.ok(mastiff.scale.x > shepherd.scale.x, '藏獒整体应比牧羊犬更高大')
  assert.ok(mastiff.userData.rig.body.scale.z > shepherd.userData.rig.body.scale.z, '藏獒躯干应更宽厚')
  assert.ok(mastiff.userData.rig.ears[0].rotation.x < 0)
  assert.ok(mastiff.userData.rig.ears[1].rotation.x > 0)
})

test('外围通道坐标始终位于围栏内且避开田地', () => {
  for (let distance = 0; distance < 96; distance += 0.25) {
    assert.equal(isDogWalkablePoint(dogPathPoint(distance)), true)
  }
})

test('跨越外围通道拐角时先经过精确拐角，不斜切土地', () => {
  const starts = [30.5, 47.5, 78.5, 95.5]
  const expectedCorners = [31, 48, 79, 0]

  starts.forEach((pathDistance, index) => {
    const behavior = new DogBehaviorController()
    behavior.random = sequenceRandom([0.9, 0.2, 0.5, 0.5])
    behavior.pathDistance = pathDistance
    behavior.position = dogPathPoint(pathDistance)
    behavior._beginRandomTrip()

    const firstWaypoint = behavior.route[0]
    assert.deepEqual(
      { x: firstWaypoint.x, z: firstWaypoint.z },
      dogPathPoint(expectedCorners[index], behavior.laneOffset),
    )

    let previous = behavior.position
    for (const waypoint of behavior.route) {
      for (let sample = 0; sample <= 20; sample++) {
        const ratio = sample / 20
        assert.equal(isDogWalkablePoint({
          x: previous.x + (waypoint.x - previous.x) * ratio,
          z: previous.z + (waypoint.z - previous.z) * ratio,
        }), true)
      }
      previous = waypoint
    }
  })
})

test('宠物按转向、移动、休息顺序循环，且不横向滑行', () => {
  const random = sequenceRandom([0.2, 0, 0.8, 0.5, 0.7, 0.4, 0.2, 0.6])
  const behavior = new DogBehaviorController({ random })
  const dog = createDogModel(0xb08968)
  behavior.attach(dog)
  const visited = new Set([behavior.state])
  let previous = dog.position.clone()

  for (let frame = 0; frame < 800; frame++) {
    behavior.update(dog, 0.05, frame * 0.05, false)
    visited.add(behavior.state)
    assert.equal(isDogWalkablePoint(dog.position), true)
    assert.ok(Number.isFinite(dog.position.x) && Number.isFinite(dog.position.z))

    const moved = dog.position.distanceTo(previous)
    if (moved > 1e-6) {
      assert.equal(behavior.state === DOG_STATES.WALK || visited.has(DOG_STATES.WALK), true)
      const dx = dog.position.x - previous.x
      const dz = dog.position.z - previous.z
      const forwardX = Math.cos(dog.rotation.y)
      const forwardZ = -Math.sin(dog.rotation.y)
      const alignment = (dx * forwardX + dz * forwardZ) / moved
      assert.ok(alignment > 0.96, `移动方向必须朝向身体正前方，实际 ${alignment}`)
    }
    previous = dog.position.clone()
  }

  assert.equal(visited.has(DOG_STATES.TURN), true)
  assert.equal(visited.has(DOG_STATES.WALK), true)
  assert.ok(
    [...visited].some((state) => [
      DOG_STATES.SNIFF,
      DOG_STATES.SIT,
      DOG_STATES.LIE,
    ].includes(state)),
  )
})

test('每次巡游在外围通道内随机换行，而不重复固定中心线', () => {
  const behavior = new DogBehaviorController()
  behavior.random = sequenceRandom([0.8, 0.5, 0.5, 0.2, 0.99])
  behavior.pathDistance = 8
  behavior.position = dogPathPoint(8)
  behavior._beginRandomTrip()

  assert.ok(Math.abs(behavior.laneOffset) > 0.3)
  assert.ok(behavior.route.every((point) => isDogWalkablePoint(point)))
  assert.notEqual(behavior.route[0].z, dogPathPoint(behavior.route[0].pathDistance).z)
})

test('休息动作权重分段与持续时间保持在设计范围', () => {
  const cases = [
    { values: [0.2, 0], state: DOG_STATES.IDLE, min: 1.5, max: 4 },
    { values: [0.5, 0.999], state: DOG_STATES.SNIFF, min: 1.5, max: 2.8 },
    { values: [0.7, 0], state: DOG_STATES.SIT, min: 3, max: 6 },
    { values: [0.95, 0.999], state: DOG_STATES.LIE, min: 5, max: 10 },
  ]

  for (const item of cases) {
    const behavior = new DogBehaviorController()
    behavior.random = sequenceRandom(item.values)
    behavior._chooseRest()
    assert.equal(behavior.state, item.state)
    assert.ok(behavior.stateDuration >= item.min && behavior.stateDuration <= item.max)
  }
})

test('行走驱动对角腿步态，休息姿势平滑折叠骨架', () => {
  const behavior = new DogBehaviorController({
    random: sequenceRandom([0.1, 0, 0.6, 0, 0.4, 0.8]),
  })
  const dog = createDogModel(0x8d99ae)
  behavior.attach(dog)
  behavior.state = DOG_STATES.WALK
  behavior.speed = 1.2
  behavior.route = [{ ...dogPathPoint(behavior.pathDistance + 4), pathDistance: behavior.pathDistance + 4 }]
  const walking = behavior.update(dog, 0.05, 0.2, false)

  const { rig } = dog.userData
  assert.equal(walking.state, DOG_STATES.WALK)
  assert.notEqual(walking.pose.stride, 0)
  assert.notEqual(rig.frontLegs[0].rotation.z, rig.frontLegs[1].rotation.z)
  assert.notEqual(rig.frontLegs[0].rotation.z, rig.hindLegs[0].rotation.z)

  behavior.state = DOG_STATES.SNIFF
  for (let i = 0; i < 10; i++) behavior.update(dog, 0.05, 1 + i * 0.05, false)
  assert.ok(rig.head.rotation.z < -0.3)
  assert.ok(rig.head.position.x > rig.head.userData.baseX)

  behavior.state = DOG_STATES.LIE
  for (let i = 0; i < 12; i++) behavior.update(dog, 0.05, 2 + i * 0.05, false)
  assert.ok(rig.root.position.y < -0.15)
  assert.ok(rig.frontLegs.every((leg) => leg.rotation.z > 0.45))
  assert.ok(rig.hindLegs.every((leg) => leg.rotation.z < -0.45))
})

test('饥饿时沿外围通道返回休息点，补粮后恢复行为循环', () => {
  const behavior = new DogBehaviorController({
    random: sequenceRandom([0.05, 0.2, 0.5, 0.7, 0.4]),
  })
  const dog = createDogModel(0x4a3728)
  behavior.attach(dog)

  for (let frame = 0; frame < 1600 && behavior.state !== DOG_STATES.HUNGRY_REST; frame++) {
    behavior.update(dog, 0.05, frame * 0.05, true)
    assert.equal(isDogWalkablePoint(dog.position), true)
  }
  assert.equal(behavior.state, DOG_STATES.HUNGRY_REST)
  assert.deepEqual(
    { x: Number(dog.position.x.toFixed(3)), z: Number(dog.position.z.toFixed(3)) },
    { x: 12.5, z: 8.5 },
  )

  behavior.update(dog, 0.05, 90, false)
  assert.equal(behavior.state, DOG_STATES.IDLE)
  for (let frame = 0; frame < 200; frame++) behavior.update(dog, 0.05, 91 + frame * 0.05, false)
  assert.ok([DOG_STATES.TURN, DOG_STATES.WALK, DOG_STATES.IDLE, DOG_STATES.SNIFF, DOG_STATES.SIT, DOG_STATES.LIE].includes(behavior.state))
})

test('异常大帧间隔会被限制，不会越界或产生非法坐标', () => {
  const behavior = new DogBehaviorController({
    random: sequenceRandom([0.4, 0.1, 0.9, 0.3, 0.6]),
  })
  const dog = createDogModel(0xb08968)
  behavior.attach(dog)
  behavior.stateDuration = 0
  for (let i = 0; i < 60; i++) {
    behavior.update(dog, 30, i, false)
    assert.equal(isDogWalkablePoint(dog.position), true)
  }
})
