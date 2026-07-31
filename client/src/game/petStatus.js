import { DOGS } from './config.js'

export const DOG_TYPE_TO_ID = Object.freeze(Object.fromEntries(
  DOGS.filter((dog) => dog.dogType > 0).map((dog) => [dog.dogType, dog.id]),
))

/**
 * 将服务端 PetStatus 投影为客户端的“当前启用宠物 + 已拥有宠物表”。
 * 兼容仅返回 active_dog/dog_level 的旧服务端。
 */
export function applyPetStatus(state, status) {
  if (!state || !status || typeof status !== 'object') return false

  const ownedMask = Math.max(0, Number(status.owned) || 0)
  const entries = Array.isArray(status.dogs) ? status.dogs : []
  const byType = new Map(entries.map((entry) => [Number(entry?.dog_type), entry]))
  const activeType = Number(status.active_dog) || 0
  const petDogs = {}

  for (const definition of DOGS) {
    const dogType = Number(definition.dogType)
    if (dogType <= 0) continue
    const entry = byType.get(dogType)
    const owned = Boolean(entry) || Boolean(ownedMask & (1 << (dogType - 1)))
    if (!owned) continue

    const legacyActive = activeType === dogType
    const level = Math.max(0, Number(entry?.level ?? (legacyActive ? status.dog_level : 0)) || 0)
    const intercepts = Math.max(0, Number(entry?.intercepts ?? (legacyActive ? status.intercepts : 0)) || 0)
    const interceptionPct = Math.max(
      0,
      Number(entry?.interception_pct ?? (legacyActive ? status.interception_pct : 0)) ||
        Math.round(definition.intercept * 100) + level,
    )
    petDogs[definition.id] = {
      id: definition.id,
      owned: true,
      level,
      intercepts,
      interceptionPct,
    }
  }

  state.petDogs = petDogs
  const activeId = DOG_TYPE_TO_ID[activeType]
  state.dog = activeId && petDogs[activeId] ? { ...petDogs[activeId] } : null
  if (typeof status.bowl_grams === 'number') {
    state.dogBowl = Math.max(0, status.bowl_grams)
  }
  state.dogBowlEmptyAt = Math.max(0, Number(status.bowl_empty_at) || 0)
  state.dogMsPerGram = Math.max(0, Number(status.ms_per_gram) || 0)
  return true
}
