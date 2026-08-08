import { create, fromBinary, toBinary } from '@bufbuild/protobuf'
import {
  WireBatchSchema,
  WireEnvelopeSchema,
  EnterFarmRequestSchema,
  SyncFarmRequestSchema,
  EnterFarmResponseSchema,
  SyncFarmResponseSchema,
  FarmDeltaSchema,
  FarmSnapshotSchema,
  PlotSnapshotSchema,
  GuardDogSchema,
} from '../gen/farm/public/v3/client_pb.js'
import { parseJSONSafe } from './jsonSafe.js'

export const WS_BINARY_SUBPROTOCOL = 'farm.v3.pb'
export const MAX_BINARY_BATCH_ENVELOPES = 64

const CMD_ENTER_FARM = 200
const CMD_SYNC_FARM = 204
const CMD_FARM_DELTA = 9000
const encoder = new TextEncoder()
const decoder = new TextDecoder('utf-8', { fatal: true })

function uint64(value, field) {
  if (typeof value === 'bigint') {
    if (value < 0n) throw new Error(`wire: ${field} must be uint64`)
    return value
  }
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value) || value < 0) throw new Error(`wire: unsafe ${field}`)
    return BigInt(value)
  }
  if (typeof value === 'string' && /^(0|[1-9]\d*)$/.test(value)) return BigInt(value)
  throw new Error(`wire: invalid ${field}`)
}

function int64(value, field) {
  if (typeof value === 'bigint') return value
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) throw new Error(`wire: unsafe ${field}`)
    return BigInt(value)
  }
  if (typeof value === 'string' && /^-?(0|[1-9]\d*)$/.test(value)) return BigInt(value)
  throw new Error(`wire: invalid ${field}`)
}

function safeInteger(value) {
  const number = Number(value)
  return Number.isSafeInteger(number) ? number : value.toString()
}

function identifier(value) {
  return value.toString()
}

function requestIdentifier(value) {
  return value <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(value) : value.toString()
}

function guardDogToProto(value) {
  if (!value) return undefined
  return create(GuardDogSchema, {
    activeDog: Number(value.active_dog ?? 0),
    bowlEmptyAt: int64(value.bowl_empty_at ?? 0, 'bowl_empty_at'),
  })
}

function plotToProto(value = {}) {
  return create(PlotSnapshotSchema, {
    index: Number(value.index ?? 0),
    state: Number(value.state ?? 0),
    cropId: Number(value.crop_id ?? 0),
    seasonIndex: Number(value.season_index ?? 0),
    seasonTotal: Number(value.season_total ?? 0),
    seasonStartAt: int64(value.season_start_at ?? 0, 'season_start_at'),
    matureAt: int64(value.mature_at ?? 0, 'mature_at'),
    seasonDuration: int64(value.season_duration ?? 0, 'season_duration'),
    finalYield: Number(value.final_yield ?? 0),
    lastSettleAt: int64(value.last_settle_at ?? 0, 'last_settle_at'),
    lastWaterAt: int64(value.last_water_at ?? 0, 'last_water_at'),
    weedSince: int64(value.weed_since ?? 0, 'weed_since'),
    pestSince: int64(value.pest_since ?? 0, 'pest_since'),
    health: Number(value.health ?? 0),
    stolenCount: Number(value.stolen_count ?? 0),
    fertMask: Number(value.fert_mask ?? 0),
  })
}

function snapshotToProto(value = {}) {
  return create(FarmSnapshotSchema, {
    ownerUid: uint64(value.owner_uid ?? 0, 'owner_uid'),
    nickname: String(value.nickname ?? ''),
    level: Number(value.level ?? 0),
    exp: Number(value.exp ?? 0),
    coin: int64(value.coin ?? 0, 'coin'),
    unlockedPlots: Number(value.unlocked_plots ?? 0),
    plots: (value.plots ?? []).map(plotToProto),
    bag: value.bag ?? {},
    warehouse: value.warehouse ?? {},
    guardDog: guardDogToProto(value.guard_dog),
  })
}

function deltaToProto(value = {}) {
  return create(FarmDeltaSchema, {
    ownerUid: uint64(value.owner_uid ?? 0, 'owner_uid'),
    farmSeq: uint64(value.farm_seq ?? 0, 'farm_seq'),
    plots: (value.plots ?? []).map(plotToProto),
    guardDog: guardDogToProto(value.guard_dog),
    actorUid: uint64(value.actor_uid ?? 0, 'actor_uid'),
    action: Number(value.action ?? 0),
  })
}

function plotFromProto(value) {
  return {
    index: value.index,
    state: value.state,
    crop_id: value.cropId,
    season_index: value.seasonIndex,
    season_total: value.seasonTotal,
    season_start_at: safeInteger(value.seasonStartAt),
    mature_at: safeInteger(value.matureAt),
    season_duration: safeInteger(value.seasonDuration),
    final_yield: value.finalYield,
    last_settle_at: safeInteger(value.lastSettleAt),
    last_water_at: safeInteger(value.lastWaterAt),
    weed_since: safeInteger(value.weedSince),
    pest_since: safeInteger(value.pestSince),
    health: value.health,
    stolen_count: value.stolenCount,
    fert_mask: value.fertMask,
  }
}

function guardDogFromProto(value) {
  if (!value) return undefined
  return { active_dog: value.activeDog, bowl_empty_at: safeInteger(value.bowlEmptyAt) }
}

function snapshotFromProto(value) {
  if (!value) return undefined
  return {
    owner_uid: identifier(value.ownerUid),
    nickname: value.nickname,
    level: value.level,
    exp: value.exp,
    coin: safeInteger(value.coin),
    unlocked_plots: value.unlockedPlots,
    plots: value.plots.map(plotFromProto),
    bag: value.bag,
    warehouse: value.warehouse,
    guard_dog: guardDogFromProto(value.guardDog) ?? { active_dog: 0, bowl_empty_at: 0 },
  }
}

function deltaFromProto(value) {
  const result = {
    owner_uid: identifier(value.ownerUid),
    farm_seq: identifier(value.farmSeq),
    plots: value.plots.map(plotFromProto),
    actor_uid: identifier(value.actorUid),
    action: value.action,
  }
  if (value.guardDog) result.guard_dog = guardDogFromProto(value.guardDog)
  return result
}

function payloadToProto(envelope) {
  const payload = envelope.payload ?? {}
  if ((envelope.err ?? 0) === 0 && envelope.cmd === CMD_ENTER_FARM) {
    if ('snapshot' in payload) {
      return { case: 'enterFarmResponse', value: create(EnterFarmResponseSchema, {
        snapshot: snapshotToProto(payload.snapshot),
        farmSeq: uint64(payload.farm_seq ?? 0, 'farm_seq'),
        serverTime: int64(payload.server_time ?? 0, 'server_time'),
        timeProfile: String(payload.time_profile ?? ''),
        timeProfileMutable: Boolean(payload.time_profile_mutable),
        relation: String(payload.relation ?? ''),
      }) }
    }
    return { case: 'enterFarmRequest', value: create(EnterFarmRequestSchema, {
      ownerUid: uint64(payload.owner_uid ?? 0, 'owner_uid'),
    }) }
  }
  if ((envelope.err ?? 0) === 0 && envelope.cmd === CMD_SYNC_FARM) {
    if ('farm_seq' in payload) {
      return { case: 'syncFarmResponse', value: create(SyncFarmResponseSchema, {
        deltas: (payload.deltas ?? []).map(deltaToProto),
        snapshot: payload.snapshot ? snapshotToProto(payload.snapshot) : undefined,
        farmSeq: uint64(payload.farm_seq, 'farm_seq'),
        serverTime: int64(payload.server_time ?? 0, 'server_time'),
        timeProfile: String(payload.time_profile ?? ''),
        timeProfileMutable: Boolean(payload.time_profile_mutable),
      }) }
    }
    return { case: 'syncFarmRequest', value: create(SyncFarmRequestSchema, {
      ownerUid: uint64(payload.owner_uid ?? 0, 'owner_uid'),
      fromSeq: uint64(payload.from_seq ?? 0, 'from_seq'),
    }) }
  }
  if ((envelope.err ?? 0) === 0 && envelope.cmd === CMD_FARM_DELTA && 'owner_uid' in payload && 'actor_uid' in payload) {
    return { case: 'farmDelta', value: deltaToProto(payload) }
  }
  return { case: 'jsonPayload', value: encoder.encode(JSON.stringify(payload)) }
}

function payloadFromProto(envelope) {
  const payload = envelope.payload
  switch (payload.case) {
    case 'jsonPayload': {
      const parsed = parseJSONSafe(decoder.decode(payload.value))
      if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') throw new Error('wire: payload must be an object')
      return parsed
    }
    case 'enterFarmRequest':
      return { owner_uid: requestIdentifier(payload.value.ownerUid) }
    case 'syncFarmRequest':
      return { owner_uid: requestIdentifier(payload.value.ownerUid), from_seq: requestIdentifier(payload.value.fromSeq) }
    case 'enterFarmResponse':
      return {
        snapshot: snapshotFromProto(payload.value.snapshot),
        farm_seq: identifier(payload.value.farmSeq),
        server_time: safeInteger(payload.value.serverTime),
        time_profile: payload.value.timeProfile,
        time_profile_mutable: payload.value.timeProfileMutable,
        relation: payload.value.relation,
      }
    case 'syncFarmResponse': {
      const result = {
        farm_seq: identifier(payload.value.farmSeq),
        server_time: safeInteger(payload.value.serverTime),
        time_profile: payload.value.timeProfile,
        time_profile_mutable: payload.value.timeProfileMutable,
      }
      if (payload.value.deltas.length) result.deltas = payload.value.deltas.map(deltaFromProto)
      if (payload.value.snapshot) result.snapshot = snapshotFromProto(payload.value.snapshot)
      return result
    }
    case 'farmDelta':
      return deltaFromProto(payload.value)
    default:
      throw new Error('wire: missing protobuf payload')
  }
}

/** @param {Array<{cmd:number, client_seq:number, err:number, payload:object}>} envelopes */
export function encodeBinaryBatch(envelopes) {
  if (!Array.isArray(envelopes) || envelopes.length < 1 || envelopes.length > MAX_BINARY_BATCH_ENVELOPES) {
    throw new Error('wire: protobuf batch size must be 1..64')
  }
  const batch = create(WireBatchSchema, {
    envelopes: envelopes.map((envelope) => create(WireEnvelopeSchema, {
      cmd: envelope.cmd,
      clientSeq: envelope.client_seq,
      err: envelope.err ?? 0,
      payload: payloadToProto(envelope),
    })),
  })
  return toBinary(WireBatchSchema, batch)
}

/** @param {ArrayBuffer|ArrayBufferView} source */
export function decodeBinaryBatch(source) {
  const data = source instanceof ArrayBuffer
    ? new Uint8Array(source)
    : new Uint8Array(source.buffer, source.byteOffset, source.byteLength)
  let batch
  try {
    batch = fromBinary(WireBatchSchema, data, { readUnknownFields: false })
  } catch (error) {
    throw new Error(`wire: trailing or invalid protobuf data: ${error instanceof Error ? error.message : error}`)
  }
  if (batch.envelopes.length < 1 || batch.envelopes.length > MAX_BINARY_BATCH_ENVELOPES) {
    throw new Error('wire: invalid protobuf batch count')
  }
  return batch.envelopes.map((envelope) => ({
    cmd: envelope.cmd,
    client_seq: envelope.clientSeq,
    err: envelope.err,
    payload: payloadFromProto(envelope),
  }))
}
