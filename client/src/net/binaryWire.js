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
  CommandRequestSchema,
  CommandResponseSchema,
  FarmPatchSchema,
  ActionResponseSchema,
  VisitorRewardSchema,
  FriendSchema,
  UserSchema,
  FriendRequestSchema,
  PetDogStatusSchema,
  PetStatusSchema,
  TaskSchema,
  TaskRewardSchema,
  MailSchema,
  CodexProgressSchema,
  CodexRewardNoticeSchema,
  PlayerDeltaSchema,
  MailNotifySchema,
  SessionKickSchema,
} from '../gen/farm/public/v3/client_pb.js'

export const WS_BINARY_SUBPROTOCOL = 'farm.v3.pb'
export const MAX_BINARY_BATCH_ENVELOPES = 64

const CMD_ENTER_FARM = 200
const CMD_SYNC_FARM = 204
const CMD_FARM_DELTA = 9000
const CMD_PLAYER_DELTA = 9002
const CMD_MAIL_NOTIFY = 9004
const CMD_SESSION_KICK = 9006
const CMD_TASK_NOTIFY = 9008

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
  }
  if (value.guardDog) result.guard_dog = guardDogFromProto(value.guardDog)
  if (value.actorUid !== 0n) result.actor_uid = identifier(value.actorUid)
  if (value.action !== 0) result.action = value.action
  return result
}

function codexProgressToProto(value = {}) {
  return create(CodexProgressSchema, {
    cropId: Number(value.crop_id ?? 0),
    harvestCount: Number(value.harvest_count ?? 0),
    tier: String(value.tier ?? ''),
    nextTarget: Number(value.next_target ?? 0),
  })
}

function codexProgressFromProto(value) {
  return { crop_id: value.cropId, harvest_count: value.harvestCount, tier: value.tier, next_target: value.nextTarget }
}

function codexRewardToProto(value = {}) {
  return create(CodexRewardNoticeSchema, {
    cropId: Number(value.crop_id ?? 0), tier: String(value.tier ?? ''),
    target: Number(value.target ?? 0), rewardCoin: int64(value.reward_coin ?? 0, 'reward_coin'),
  })
}

function codexRewardFromProto(value) {
  return { crop_id: value.cropId, tier: value.tier, target: value.target, reward_coin: safeInteger(value.rewardCoin) }
}

function patchToProto(value = {}) {
  const result = {
    plot: value.plot ? plotToProto(value.plot) : undefined,
    coin: int64(value.coin ?? 0, 'coin'), exp: Number(value.exp ?? 0),
    bagChanges: value.bag_changes ?? {}, warehouseChanges: value.warehouse_changes ?? {},
    farmSeq: uint64(value.farm_seq ?? 0, 'farm_seq'),
    codexProgress: value.codex_progress ? codexProgressToProto(value.codex_progress) : undefined,
  }
  if (value.plot_index != null || value.plot) result.plotIndex = Number(value.plot_index ?? value.plot?.index ?? 0)
  return create(FarmPatchSchema, result)
}

function patchFromProto(value) {
  if (!value) return {}
  const result = {
    coin: safeInteger(value.coin), exp: value.exp,
    farm_seq: identifier(value.farmSeq),
  }
  if (value.plotIndex != null) result.plot_index = value.plotIndex
  if (value.plot) result.plot = plotFromProto(value.plot)
  if (Object.keys(value.bagChanges).length) result.bag_changes = value.bagChanges
  if (Object.keys(value.warehouseChanges).length) result.warehouse_changes = value.warehouseChanges
  if (value.codexProgress) result.codex_progress = codexProgressFromProto(value.codexProgress)
  return result
}

function actionToProto(value = {}) {
  return create(ActionResponseSchema, {
    farmSeq: uint64(value.farm_seq ?? 0, 'farm_seq'),
    patch: patchToProto(value.patch ?? {}),
    codexRewards: (value.codex_rewards ?? []).map(codexRewardToProto),
  })
}

function actionFromProto(value) {
  const result = { farm_seq: identifier(value.farmSeq), patch: patchFromProto(value.patch) }
  if (value.codexRewards.length) result.codex_rewards = value.codexRewards.map(codexRewardFromProto)
  return result
}

function visitorRewardToProto(value = {}) {
  return create(VisitorRewardSchema, {
    reqId: uint64(value.req_id ?? 0, 'req_id'), expGained: Number(value.exp_gained ?? 0),
    coinGained: int64(value.coin_gained ?? 0, 'coin_gained'), cropId: Number(value.crop_id ?? 0),
    amount: Number(value.amount ?? 0), compensation: int64(value.compensation ?? 0, 'compensation'),
    dogType: Number(value.dog_type ?? 0),
  })
}

function visitorRewardFromProto(value) {
  const result = { req_id: identifier(value.reqId), exp_gained: value.expGained, coin_gained: safeInteger(value.coinGained) }
  if (value.cropId) result.crop_id = value.cropId
  if (value.amount) result.amount = value.amount
  if (value.compensation) result.compensation = safeInteger(value.compensation)
  if (value.dogType) result.dog_type = value.dogType
  return result
}

function petStatusToProto(value = {}) {
  return create(PetStatusSchema, {
    activeDog: Number(value.active_dog ?? 0), owned: Number(value.owned ?? 0), bowlGrams: Number(value.bowl_grams ?? 0),
    bowlEmptyAt: int64(value.bowl_empty_at ?? 0, 'bowl_empty_at'), msPerGram: int64(value.ms_per_gram ?? 0, 'ms_per_gram'),
    dogLevel: Number(value.dog_level ?? 0), intercepts: Number(value.intercepts ?? 0), interceptionPct: Number(value.interception_pct ?? 0),
    dogs: (value.dogs ?? []).map((dog) => create(PetDogStatusSchema, {
      dogType: Number(dog.dog_type ?? 0), level: Number(dog.level ?? 0),
      intercepts: Number(dog.intercepts ?? 0), interceptionPct: Number(dog.interception_pct ?? 0),
    })),
  })
}

function petStatusFromProto(value) {
  return {
    active_dog: value.activeDog, owned: value.owned, bowl_grams: value.bowlGrams,
    bowl_empty_at: safeInteger(value.bowlEmptyAt), ms_per_gram: safeInteger(value.msPerGram),
    dog_level: value.dogLevel, intercepts: value.intercepts, interception_pct: value.interceptionPct,
    dogs: value.dogs.map((dog) => ({ dog_type: dog.dogType, level: dog.level, intercepts: dog.intercepts, interception_pct: dog.interceptionPct })),
  }
}

function taskToProto(value = {}) {
  return create(TaskSchema, {
    id: Number(value.id ?? 0), dayKey: int64(value.day_key ?? 0, 'day_key'), kind: String(value.kind ?? ''),
    title: String(value.title ?? ''), progress: Number(value.progress ?? 0), target: Number(value.target ?? 0),
    rewardCoin: int64(value.reward_coin ?? 0, 'reward_coin'), claimed: Boolean(value.claimed),
  })
}

function taskFromProto(value) {
  const result = { id: value.id, title: value.title, progress: value.progress, target: value.target, reward_coin: safeInteger(value.rewardCoin), claimed: value.claimed }
  if (value.dayKey !== 0n) result.day_key = safeInteger(value.dayKey)
  if (value.kind) result.kind = value.kind
  return result
}

function mailToProto(value = {}) {
  return create(MailSchema, {
    id: uint64(value.id ?? 0, 'mail_id'), title: String(value.title ?? ''), attachmentCoin: int64(value.attachment_coin ?? 0, 'attachment_coin'),
    claimed: Boolean(value.claimed), read: Boolean(value.read), createdAt: int64(value.created_at ?? 0, 'created_at'),
  })
}

function mailFromProto(value) {
  return { id: identifier(value.id), title: value.title, attachment_coin: safeInteger(value.attachmentCoin), claimed: value.claimed, read: value.read, created_at: safeInteger(value.createdAt) }
}

function playerDeltaToProto(value = {}) {
  return create(PlayerDeltaSchema, {
    coin: int64(value.coin ?? 0, 'coin'), exp: Number(value.exp ?? 0), level: Number(value.level ?? 0),
    bag: value.bag ?? {}, warehouse: value.warehouse ?? {}, pet: value.pet ? petStatusToProto(value.pet) : undefined,
  })
}

function playerDeltaFromProto(value) {
  const result = { coin: safeInteger(value.coin), exp: value.exp, level: value.level, bag: value.bag, warehouse: value.warehouse }
  if (value.pet) result.pet = petStatusFromProto(value.pet)
  return result
}

function commandRequestToProto(cmd, payload = {}) {
  const result = {}
  switch (cmd) {
    case 100:
      result.authToken = String(payload.token ?? '')
      result.resumeFarmUid = uint64(payload.resume_farm_uid ?? 0, 'resume_farm_uid')
      result.resumeFarmSeq = uint64(payload.resume_farm_seq ?? 0, 'resume_farm_seq')
      result.clientConfigVer = Number(payload.client_config_ver ?? 0)
      break
    case 102: result.clientTime = int64(payload.client_time ?? 0, 'client_time'); break
    case 202: case 400: case 402: case 414: case 500: case 600: case 604: case 612: case 614: break
    case 206: case 208: case 210: case 212: case 214: case 216: case 218: case 220:
      result.ownerUid = uint64(payload.owner_uid ?? 0, 'owner_uid')
      result.plotIndex = Number(payload.plot_index ?? 0)
      result.arg = Number(payload.arg ?? 0)
      break
    case 222:
      result.ownerUid = uint64(payload.owner_uid ?? 0, 'owner_uid')
      result.plotIndex = Number(payload.plot_index ?? 0)
      result.cropId = Number(payload.crop_id ?? 0)
      break
    case 302: case 304:
      result.itemId = Number(payload.item_id ?? 0)
      result.quantity = Number(payload.quantity ?? 0)
      break
    case 404: result.inviteToken = String(payload.token ?? ''); break
    case 406: case 408: case 412: result.peerUid = uint64(payload.peer_uid ?? 0, 'peer_uid'); break
    case 410: result.username = String(payload.username ?? ''); break
    case 416: case 418: result.fromUid = uint64(payload.from_uid ?? 0, 'from_uid'); break
    case 502: result.dogType = Number(payload.dog_type ?? 0); break
    case 504: result.grams = Number(payload.grams ?? 0); break
    case 602: result.taskId = Number(payload.task_id ?? 0); break
    case 606: case 610:
      result.mailId = uint64(payload.mail_id ?? 0, 'mail_id')
      result.all = Boolean(payload.all)
      break
    case 608: result.mailId = uint64(payload.mail_id ?? 0, 'mail_id'); break
    case 616: result.timeProfile = String(payload.time_profile ?? ''); break
    default: throw new Error(`wire: unsupported command request ${cmd}`)
  }
  return create(CommandRequestSchema, result)
}

function commandRequestFromProto(cmd, value) {
  switch (cmd) {
    case 100: return { token: value.authToken, client_config_ver: value.clientConfigVer, resume_farm_uid: requestIdentifier(value.resumeFarmUid), resume_farm_seq: requestIdentifier(value.resumeFarmSeq) }
    case 102: return { client_time: safeInteger(value.clientTime) }
    case 202: case 400: case 402: case 414: case 500: case 600: case 604: case 612: case 614: return {}
    case 206: case 208: case 210: case 212: case 214: case 216: case 218: case 220: return { owner_uid: requestIdentifier(value.ownerUid), plot_index: value.plotIndex, arg: value.arg }
    case 222: return { owner_uid: requestIdentifier(value.ownerUid), plot_index: value.plotIndex, crop_id: value.cropId }
    case 302: case 304: return { item_id: value.itemId, quantity: value.quantity }
    case 404: return { token: value.inviteToken }
    case 406: case 408: case 412: return { peer_uid: requestIdentifier(value.peerUid) }
    case 410: return { username: value.username }
    case 416: case 418: return { from_uid: requestIdentifier(value.fromUid) }
    case 502: return { dog_type: value.dogType }
    case 504: return { grams: value.grams }
    case 602: return { task_id: value.taskId }
    case 606: case 610: return value.all ? { all: true } : { mail_id: requestIdentifier(value.mailId), all: false }
    case 608: return { mail_id: requestIdentifier(value.mailId) }
    case 616: return { time_profile: value.timeProfile }
    default: throw new Error(`wire: unsupported command request ${cmd}`)
  }
}

function isCommandResponse(envelope) {
  if ((envelope.err ?? 0) !== 0) return true
  const payload = envelope.payload ?? {}
  switch (envelope.cmd) {
    case 100: return !('token' in payload)
    case 102: return !('client_time' in payload) || 'server_time' in payload
    case 206: case 208: case 210: case 212: case 214: case 216: case 218: case 220: return 'farm_seq' in payload || 'req_id' in payload
    case 222: return 'req_id' in payload
    case 302: case 304: return 'farm_seq' in payload
    case 400: return 'friends' in payload
    case 402: return 'path' in payload
    case 410: return 'users' in payload
    case 414: return 'requests' in payload
    case 500: case 502: case 504: return 'active_dog' in payload || 'dogs' in payload
    case 600: return 'tasks' in payload
    case 602: case 614: return 'coin' in payload || 'exp' in payload
    case 604: return 'mails' in payload
    case 606: case 610: return 'affected' in payload
    case 608: return 'id' in payload || 'attachment_coin' in payload
    case 612: return 'entries' in payload
    case 616: return 'time_profile_mutable' in payload
    default: return false
  }
}

function commandResponseToProto(cmd, payload = {}) {
  const result = {}
  switch (cmd) {
    case 100: result.uid = uint64(payload.uid ?? 0, 'uid'); break
    case 102: result.clientTime = int64(payload.client_time ?? 0, 'client_time'); result.serverTime = int64(payload.server_time ?? 0, 'server_time'); break
    case 206: case 208: case 210: case 212: case 214: case 216: case 218: case 220: case 302: case 304:
      if ('req_id' in payload) result.visitorReward = visitorRewardToProto(payload); else if ('farm_seq' in payload) result.action = actionToProto(payload)
      break
    case 222: if ('req_id' in payload) result.visitorReward = visitorRewardToProto(payload); break
    case 400: result.friends = (payload.friends ?? []).map((friend) => create(FriendSchema, { uid: uint64(friend.uid ?? 0, 'uid'), nickname: String(friend.nickname ?? ''), hasStealable: Boolean(friend.has_stealable) })); break
    case 402: result.path = String(payload.path ?? ''); break
    case 410: result.users = (payload.users ?? []).map((user) => create(UserSchema, { uid: uint64(user.uid ?? 0, 'uid'), nickname: String(user.nickname ?? '') })); break
    case 414: result.friendRequests = (payload.requests ?? []).map((request) => create(FriendRequestSchema, { fromUid: uint64(request.from_uid ?? 0, 'from_uid'), nickname: String(request.nickname ?? ''), createdAt: int64(request.created_at ?? 0, 'created_at') })); break
    case 500: case 502: case 504: if (Object.keys(payload).length) result.petStatus = petStatusToProto(payload); break
    case 600: result.tasks = (payload.tasks ?? []).map(taskToProto); result.resetAt = int64(payload.reset_at ?? 0, 'reset_at'); break
    case 602: case 614: if (Object.keys(payload).length) result.taskReward = create(TaskRewardSchema, { coin: int64(payload.coin ?? 0, 'coin'), exp: Number(payload.exp ?? 0) }); break
    case 604: result.mails = (payload.mails ?? []).map(mailToProto); break
    case 606: case 610: result.affected = int64(payload.affected ?? 0, 'affected'); break
    case 608: if (Object.keys(payload).length) result.mail = mailToProto(payload); break
    case 612: result.codexEntries = (payload.entries ?? []).map(codexProgressToProto); result.codexTotal = Number(payload.total ?? 0); break
    case 616: result.timeProfile = String(payload.time_profile ?? ''); result.timeProfileMutable = Boolean(payload.time_profile_mutable); break
  }
  return create(CommandResponseSchema, result)
}

function commandResponseFromProto(cmd, value) {
  switch (cmd) {
    case 100: return value.uid ? { uid: identifier(value.uid) } : {}
    case 102: return value.serverTime || value.clientTime ? { client_time: safeInteger(value.clientTime), server_time: safeInteger(value.serverTime) } : {}
    case 206: case 208: case 210: case 212: case 214: case 216: case 218: case 220: case 302: case 304:
      if (value.visitorReward) return visitorRewardFromProto(value.visitorReward)
      return value.action ? actionFromProto(value.action) : {}
    case 222: return value.visitorReward ? visitorRewardFromProto(value.visitorReward) : {}
    case 400: return { friends: value.friends.map((friend) => ({ uid: identifier(friend.uid), nickname: friend.nickname, has_stealable: friend.hasStealable })) }
    case 402: return value.path ? { path: value.path } : {}
    case 410: return { users: value.users.map((user) => ({ uid: identifier(user.uid), nickname: user.nickname })) }
    case 414: return { requests: value.friendRequests.map((request) => ({ from_uid: identifier(request.fromUid), nickname: request.nickname, created_at: safeInteger(request.createdAt) })) }
    case 500: case 502: case 504: return value.petStatus ? petStatusFromProto(value.petStatus) : {}
    case 600: return { tasks: value.tasks.map(taskFromProto), reset_at: safeInteger(value.resetAt) }
    case 602: case 614: return value.taskReward ? { coin: safeInteger(value.taskReward.coin), exp: value.taskReward.exp } : {}
    case 604: return { mails: value.mails.map(mailFromProto) }
    case 606: case 610: return { affected: safeInteger(value.affected) }
    case 608: return value.mail ? mailFromProto(value.mail) : {}
    case 612: return { entries: value.codexEntries.map(codexProgressFromProto), total: value.codexTotal }
    case 616: return { time_profile: value.timeProfile, time_profile_mutable: value.timeProfileMutable }
    default: return {}
  }
}

function payloadToProto(envelope) {
  const payload = envelope.payload ?? {}
  if ((envelope.err ?? 0) === 0 && envelope.cmd === CMD_ENTER_FARM) {
    if ('snapshot' in payload) return { case: 'enterFarmResponse', value: create(EnterFarmResponseSchema, { snapshot: snapshotToProto(payload.snapshot), farmSeq: uint64(payload.farm_seq ?? 0, 'farm_seq'), serverTime: int64(payload.server_time ?? 0, 'server_time'), timeProfile: String(payload.time_profile ?? ''), timeProfileMutable: Boolean(payload.time_profile_mutable), relation: String(payload.relation ?? '') }) }
    return { case: 'enterFarmRequest', value: create(EnterFarmRequestSchema, { ownerUid: uint64(payload.owner_uid ?? 0, 'owner_uid') }) }
  }
  if ((envelope.err ?? 0) === 0 && envelope.cmd === CMD_SYNC_FARM) {
    if ('farm_seq' in payload) return { case: 'syncFarmResponse', value: create(SyncFarmResponseSchema, { deltas: (payload.deltas ?? []).map(deltaToProto), snapshot: payload.snapshot ? snapshotToProto(payload.snapshot) : undefined, farmSeq: uint64(payload.farm_seq, 'farm_seq'), serverTime: int64(payload.server_time ?? 0, 'server_time'), timeProfile: String(payload.time_profile ?? ''), timeProfileMutable: Boolean(payload.time_profile_mutable) }) }
    return { case: 'syncFarmRequest', value: create(SyncFarmRequestSchema, { ownerUid: uint64(payload.owner_uid ?? 0, 'owner_uid'), fromSeq: uint64(payload.from_seq ?? 0, 'from_seq') }) }
  }
  if (envelope.cmd === CMD_FARM_DELTA) return { case: 'farmDelta', value: deltaToProto(payload) }
  if (envelope.cmd === CMD_PLAYER_DELTA) return { case: 'playerDelta', value: playerDeltaToProto(payload) }
  if (envelope.cmd === CMD_MAIL_NOTIFY) return { case: 'mailNotify', value: create(MailNotifySchema, { kind: String(payload.kind ?? '') }) }
  if (envelope.cmd === CMD_SESSION_KICK) return { case: 'sessionKick', value: create(SessionKickSchema, { reason: Number(payload.reason ?? 0) }) }
  if (envelope.cmd === CMD_TASK_NOTIFY) return { case: 'taskNotify', value: taskToProto(payload) }
  if (isCommandResponse(envelope)) return { case: 'commandResponse', value: commandResponseToProto(envelope.cmd, payload) }
  return { case: 'commandRequest', value: commandRequestToProto(envelope.cmd, payload) }
}

function payloadFromProto(envelope) {
  const payload = envelope.payload
  switch (payload.case) {
    case 'commandRequest': return commandRequestFromProto(envelope.cmd, payload.value)
    case 'commandResponse': return commandResponseFromProto(envelope.cmd, payload.value)
    case 'enterFarmRequest': return { owner_uid: requestIdentifier(payload.value.ownerUid) }
    case 'syncFarmRequest': return { owner_uid: requestIdentifier(payload.value.ownerUid), from_seq: requestIdentifier(payload.value.fromSeq) }
    case 'enterFarmResponse': return { snapshot: snapshotFromProto(payload.value.snapshot), farm_seq: identifier(payload.value.farmSeq), server_time: safeInteger(payload.value.serverTime), time_profile: payload.value.timeProfile, time_profile_mutable: payload.value.timeProfileMutable, relation: payload.value.relation }
    case 'syncFarmResponse': {
      const result = { farm_seq: identifier(payload.value.farmSeq), server_time: safeInteger(payload.value.serverTime), time_profile: payload.value.timeProfile, time_profile_mutable: payload.value.timeProfileMutable }
      if (payload.value.deltas.length) result.deltas = payload.value.deltas.map(deltaFromProto)
      if (payload.value.snapshot) result.snapshot = snapshotFromProto(payload.value.snapshot)
      return result
    }
    case 'farmDelta': return deltaFromProto(payload.value)
    case 'playerDelta': return playerDeltaFromProto(payload.value)
    case 'mailNotify': return { kind: payload.value.kind }
    case 'sessionKick': return { reason: payload.value.reason }
    case 'taskNotify': return taskFromProto(payload.value)
    default: throw new Error('wire: missing protobuf payload')
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
