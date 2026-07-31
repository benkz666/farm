import test from 'node:test'
import assert from 'node:assert/strict'

import { defaultState } from './state.js'
import { applyPetStatus, DOG_TYPE_TO_ID } from './petStatus.js'

test('三种服务端宠物类型映射到稳定客户端 id', () => {
  assert.deepEqual(DOG_TYPE_TO_ID, {
    1: 'tugou',
    2: 'muyang',
    3: 'zangao',
  })
})

test('PetStatus 保留所有已拥有宠物并突出当前启用品种', () => {
  const state = defaultState()

  assert.equal(applyPetStatus(state, {
    active_dog: 2,
    owned: 0b111,
    bowl_grams: 68,
    bowl_empty_at: 123456,
    ms_per_gram: 1200,
    dogs: [
      { dog_type: 1, level: 0, intercepts: 3, interception_pct: 25 },
      { dog_type: 2, level: 2, intercepts: 40, interception_pct: 37 },
      { dog_type: 3, level: 1, intercepts: 20, interception_pct: 46 },
    ],
  }), true)

  assert.deepEqual(Object.keys(state.petDogs), ['tugou', 'muyang', 'zangao'])
  assert.deepEqual(state.dog, {
    id: 'muyang',
    owned: true,
    level: 2,
    intercepts: 40,
    interceptionPct: 37,
  })
  assert.equal(state.petDogs.zangao.interceptionPct, 46)
  assert.equal(state.dogBowl, 68)
  assert.equal(state.dogBowlEmptyAt, 123456)
  assert.equal(state.dogMsPerGram, 1200)
})

test('未启用时仍保留拥有列表，旧版单狗状态继续兼容', () => {
  const state = defaultState()
  applyPetStatus(state, { active_dog: 0, owned: 0b101, bowl_grams: 0, dogs: [
    { dog_type: 1, level: 1, intercepts: 20, interception_pct: 26 },
    { dog_type: 3, level: 0, intercepts: 0, interception_pct: 45 },
  ] })
  assert.equal(state.dog, null)
  assert.deepEqual(Object.keys(state.petDogs), ['tugou', 'zangao'])

  applyPetStatus(state, {
    active_dog: 1,
    owned: 1,
    dog_level: 2,
    intercepts: 40,
    interception_pct: 27,
    bowl_grams: 12,
  })
  assert.equal(state.dog.id, 'tugou')
  assert.equal(state.dog.level, 2)
  assert.equal(state.petDogs.tugou.intercepts, 40)
})
