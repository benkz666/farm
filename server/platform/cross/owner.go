package cross

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"strconv"
	"time"

	"farm/server/platform/actor"
	"farm/server/platform/bus"
	"farm/server/platform/connreg"
	"farm/server/platform/farm"
	"farm/server/platform/gameconf"
	"farm/server/platform/obs"
	"farm/server/platform/pkgerr"
)

// Runtime is the owner-side Actor boundary. The owner never calls another
// actor directly; EventBus delivery enters the target owner Actor through this
// boundary.
type Runtime interface {
	Do(uid uint64, fn func(*actor.FarmActor) error) error
}

// FriendChecker provides the shared friendship authority needed to reject
// forged CrossAction messages at the owner boundary.
type FriendChecker interface {
	AreFriends(ctx context.Context, a, b uint64) (bool, error)
}

// DeltaPublisher broadcasts an already committed FarmDelta. Delivery failure is
// best effort because clients recover missing deltas through SyncFarm.
type DeltaPublisher interface {
	Publish(ctx context.Context, delta farm.FarmDelta, originator connreg.ConnRef) error
}

// PlayerDeltaPublisher delivers personal-state changes that cannot be inferred
// from a farm-room delta, such as dog-interception compensation.
type PlayerDeltaPublisher interface {
	PublishPlayerDelta(ctx context.Context, uid uint64, delta farm.PlayerDelta) error
}

// StealHintWriter updates the weak-consistent FriendList stealable hint.
type StealHintWriter interface {
	SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error
}

// DeltaPublisherFunc adapts a function to DeltaPublisher.
type DeltaPublisherFunc func(context.Context, farm.FarmDelta, connreg.ConnRef) error

func (fn DeltaPublisherFunc) Publish(ctx context.Context, delta farm.FarmDelta, originator connreg.ConnRef) error {
	return fn(ctx, delta, originator)
}

// Owner consumes CrossAction messages for farms owned by this process and
// returns one CrossResult per delivery.
//
// 幂等由两层结果缓存保证：Actor 内存缓存覆盖热重投，聚合里的 CrossReceipts 与
// 地块变更同步持久化，覆盖 Actor 卸载、进程重启和消息总线的延迟重放。
type Owner struct {
	runtime Runtime
	friends FriendChecker
	bus     bus.EventBus
	now     func() int64
	deltas  DeltaPublisher
	players PlayerDeltaPublisher
	owns    func(uint64) bool
	hints   StealHintWriter

	stealRoll     func(CrossAction) uint16
	interceptRoll func(CrossAction) uint8
}

// ownerOutcome 收集一次跨农场动作在主人侧产生的全部结果与待发布副作用。
type ownerOutcome struct {
	result      CrossResult
	delta       *farm.FarmDelta
	playerDelta *farm.PlayerDelta
	stealable   bool
	refreshHint bool
	replayed    bool
}

// NewOwner assembles the authoritative owner-side cross-farm handler. Start
// must be called once after construction to subscribe it to EventBus.
func NewOwner(runtime Runtime, friends FriendChecker, eventBus bus.EventBus, now func() int64, deltas DeltaPublisher, owns func(uint64) bool) *Owner {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if owns == nil {
		owns = func(uint64) bool { return true }
	}
	return &Owner{
		runtime:       runtime,
		friends:       friends,
		bus:           eventBus,
		now:           now,
		deltas:        deltas,
		owns:          owns,
		stealRoll:     defaultStealRoll,
		interceptRoll: defaultInterceptRoll,
	}
}

// SetStealHintWriter configures optional Redis stealable-farm hint updates.
func (o *Owner) SetStealHintWriter(hints StealHintWriter) {
	if o != nil {
		o.hints = hints
	}
}

// SetPlayerDeltaPublisher configures personal-state pushes for cross-farm
// settlements. It is optional for tests that only exercise owner authority.
func (o *Owner) SetPlayerDeltaPublisher(publisher PlayerDeltaPublisher) {
	if o != nil {
		o.players = publisher
	}
}

// Start registers the owner action handler. EventBus owns consumer lifecycle;
// its context should be cancelled during process shutdown.
func (o *Owner) Start(ctx context.Context) error {
	if o == nil || o.bus == nil {
		return errors.New("cross: owner event bus is nil")
	}
	return o.bus.Subscribe(ctx, bus.TopicCrossAction, o.handleAction)
}

func (o *Owner) handleAction(_ string, payload []byte) error {
	var action CrossAction
	if err := json.Unmarshal(payload, &action); err != nil {
		// A malformed internal message cannot become valid on retry.
		return nil
	}
	obs.L().Debug("cross owner handle action",
		"component", "cross",
		"op", "handle_action",
		"kind", action.Kind,
		"owns", o.owns(action.OwnerUID),
	)
	if !o.owns(action.OwnerUID) {
		return nil
	}

	// 入参与好友关系校验留在 Actor 之外：它们只读共享状态，没有理由占用主人 Actor
	// 的串行区，更不该让一次好友查询把该农场的其他请求全部排在后面。
	if rejected, ok := o.validate(action); !ok {
		return o.publishResult(rejected)
	}

	outcome, err := o.commit(action)
	if err != nil {
		return o.publishResult(CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Internal,
		})
	}

	// 副作用发布同样在串行区之外：房间广播与 Redis 写入都是网络 IO，放在回调里会
	// 让整个农场的动作吞吐被最慢的一次推送拖住。重投的请求不再重发这些副作用。
	if !outcome.replayed {
		if outcome.delta != nil && o.deltas != nil {
			_ = o.deltas.Publish(context.Background(), *outcome.delta, connreg.ConnRef{})
		}
		if outcome.playerDelta != nil && o.players != nil {
			_ = o.players.PublishPlayerDelta(context.Background(), action.OwnerUID, *outcome.playerDelta)
		}
		if outcome.refreshHint {
			o.writeStealHint(action.OwnerUID, outcome.stealable)
		}
	}
	return o.publishResult(outcome.result)
}

func (o *Owner) writeStealHint(uid uint64, hasStealable bool) {
	if o == nil || o.hints == nil || uid == 0 {
		return
	}
	_ = o.hints.SetStealHint(context.Background(), uid, hasStealable)
}

// validate 做全部与农场状态无关的前置校验。ok 为 false 时返回的结果可直接回执。
func (o *Owner) validate(action CrossAction) (CrossResult, bool) {
	result := CrossResult{
		ReqID:      action.ReqID,
		VisitorUID: action.VisitorUID,
		OwnerUID:   action.OwnerUID,
		Code:       pkgerr.BadRequest,
	}
	if action.ReqID == 0 || action.VisitorUID == 0 || action.OwnerUID == 0 || action.VisitorUID == action.OwnerUID {
		return result, false
	}
	if action.Kind != Steal {
		if _, ok := ownerPlotActionKind(action.Kind); !ok {
			return result, false
		}
	}
	if o.friends == nil {
		result.Code = pkgerr.NotFriend
		return result, false
	}
	friends, err := o.friends.AreFriends(context.Background(), action.VisitorUID, action.OwnerUID)
	if err != nil {
		result.Code = pkgerr.Internal
		return result, false
	}
	if !friends {
		result.Code = pkgerr.NotFriend
		return result, false
	}
	if o.runtime == nil {
		result.Code = pkgerr.Internal
		return result, false
	}
	return result, true
}

// commit 在主人 Actor 的串行区内完成幂等判定与状态变更。
func (o *Owner) commit(action CrossAction) (ownerOutcome, error) {
	var outcome ownerOutcome
	err := o.runtime.Do(action.OwnerUID, func(owner *actor.FarmActor) error {
		if owner == nil || owner.Aggregate == nil {
			return errors.New("cross: owner actor aggregate is nil")
		}
		// 查重与提交必须同处一个串行区：拆开的话两次并发投递会同时查不到缓存，
		// 然后各自把同一块地改一遍。
		if cached, ok := owner.CachedResult(action.ReqID); ok {
			if previous, typed := cached.(CrossResult); typed {
				outcome.result = previous
				outcome.replayed = true
				return nil
			}
		}
		if receipt, ok := owner.Aggregate.FindCrossReceipt(action.ReqID, action.VisitorUID, action.OwnerUID, o.now()); ok {
			outcome.result = CrossResult{
				ReqID:        receipt.ReqID,
				VisitorUID:   receipt.VisitorUID,
				OwnerUID:     receipt.OwnerUID,
				Code:         pkgerr.Code(receipt.Code),
				CropID:       receipt.CropID,
				Amount:       receipt.Amount,
				Compensation: receipt.Compensation,
				DogType:      receipt.DogType,
			}
			owner.CacheResult(action.ReqID, outcome.result)
			outcome.replayed = true
			return nil
		}

		outcome.result = CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
		}
		if action.Kind == Steal {
			o.applySteal(owner, action, &outcome)
		} else {
			o.applyMaintenance(owner, action, &outcome)
		}
		owner.Aggregate.RecordCrossReceipt(farm.CrossReceipt{
			ReqID:        outcome.result.ReqID,
			VisitorUID:   outcome.result.VisitorUID,
			OwnerUID:     outcome.result.OwnerUID,
			Code:         int(outcome.result.Code),
			CropID:       outcome.result.CropID,
			Amount:       outcome.result.Amount,
			Compensation: outcome.result.Compensation,
			DogType:      outcome.result.DogType,
		}, o.now())
		owner.CacheResult(action.ReqID, outcome.result)
		// 结果回执与主人侧地块变更必须一起落盘；否则 Actor 卸载后同一
		// req_id 会丢失原始裁决，访客无法安全结算预占。
		owner.RequireFlush()
		return nil
	})
	if err != nil {
		return ownerOutcome{}, err
	}
	return outcome, nil
}

func (o *Owner) applyMaintenance(owner *actor.FarmActor, action CrossAction, outcome *ownerOutcome) {
	kind, ok := ownerPlotActionKind(action.Kind)
	if !ok {
		outcome.result.Code = pkgerr.BadRequest
		return
	}

	beforeSeq := owner.Aggregate.FarmSeq
	actionResult := owner.Aggregate.ApplyPlotAction(farm.PlotAction{
		Kind:      kind,
		PlotIndex: action.PlotIndex,
	}, o.now())
	outcome.result.Code = actionResult.Err
	if owner.Aggregate.FarmSeq != beforeSeq {
		outcome.delta = o.emitDelta(owner, action.OwnerUID, action.PlotIndex)
	}
}

func (o *Owner) applySteal(owner *actor.FarmActor, action CrossAction, outcome *ownerOutcome) {
	if int(action.PlotIndex) >= int(owner.Aggregate.UnlockedPlots) || int(action.PlotIndex) >= len(owner.Aggregate.Plots) {
		outcome.result.Code = pkgerr.PlotNotFound
		return
	}
	if owner.Aggregate.Plots[action.PlotIndex].CropID != action.CropID {
		outcome.result.Code = pkgerr.BadRequest
		return
	}
	crop, ok := gameconf.CropByID(action.CropID)
	if !ok || action.Compensation != gameconf.StealCompensation(crop) {
		outcome.result.Code = pkgerr.BadRequest
		return
	}

	beforeSeq := owner.Aggregate.FarmSeq
	steal := owner.Aggregate.ApplySteal(farm.StealAction{
		VisitorUID: action.VisitorUID,
		PlotIndex:  action.PlotIndex,
		Roll:       o.stealRoll(action),
		Intercept:  owner.Aggregate.Pet.ShouldIntercept(o.now(), o.interceptRoll(action)),
	}, o.now())
	outcome.result.Code = steal.Err
	outcome.result.CropID = steal.CropID
	outcome.result.Amount = steal.Amount

	if steal.Err == pkgerr.StealIntercepted {
		outcome.result.Compensation = action.Compensation
		outcome.result.DogType = owner.Aggregate.Pet.ActiveDog
		owner.Aggregate.ReceiveStealCompensation(action.Compensation)
		owner.Aggregate.RecordPetIntercept()
		emitted := owner.Aggregate.PlayerDelta()
		petStatus := owner.Aggregate.PetStatus(o.now())
		emitted.Pet = &petStatus
		outcome.playerDelta = &emitted
	}
	if owner.Aggregate.FarmSeq != beforeSeq {
		outcome.delta = o.emitDelta(owner, action.OwnerUID, action.PlotIndex)
		outcome.stealable = owner.Aggregate.HasStealable()
		outcome.refreshHint = true
	}
}

func (o *Owner) emitDelta(owner *actor.FarmActor, ownerUID uint64, plotIndex uint8) *farm.FarmDelta {
	emitted := farm.FarmDelta{
		OwnerUID: ownerUID,
		FarmSeq:  owner.Aggregate.FarmSeq,
		Plots:    []farm.PlotChange{ownerPlotChange(plotIndex, owner.Aggregate.Plots[plotIndex])},
	}
	owner.Deltas.Append(emitted)
	return &emitted
}

// 掷点用的盐，让同一请求派生出互不相关的两个随机数。
const (
	stealRollSalt     uint64 = 0x53544c31 // "STL1"
	interceptRollSalt uint64 = 0x444f4731 // "DOG1"
)

func defaultStealRoll(action CrossAction) uint16 {
	return uint16(deterministicRoll(action, stealRollSalt)%maxStealRoll + 1)
}

func defaultInterceptRoll(action CrossAction) uint8 {
	return uint8(deterministicRoll(action, interceptRollSalt) % 100)
}

// maxStealRoll 是单次偷菜可掷出的最大颗数，与 farm.StealAction.Roll 的约定一致。
const maxStealRoll = 10

// deterministicRoll 用请求身份派生伪随机数，取代进程本地的 math/rand。
//
// 架构 D2 要求概率事件可重放：同一次请求无论被投递几次、落到哪个进程，都必须掷出
// 同一个点数。用全局随机源的话，去重缓存一旦随 Actor 卸载而消失，重投的同一次偷菜
// 就会得出不同结果——玩家能靠断线重试刷点数。
func deterministicRoll(action CrossAction, salt uint64) uint64 {
	hash := fnv.New64a()
	var buf [8]byte
	for _, part := range [...]uint64{
		salt,
		action.OwnerUID,
		action.VisitorUID,
		action.ReqID,
		uint64(action.PlotIndex),
	} {
		binary.BigEndian.PutUint64(buf[:], part)
		_, _ = hash.Write(buf[:])
	}
	return hash.Sum64()
}

func ownerPlotActionKind(kind ActionKind) (farm.PlotActionKind, bool) {
	switch kind {
	case Water:
		return farm.Water, true
	case RemoveWeed:
		return farm.Weed, true
	case RemovePest:
		return farm.Pest, true
	default:
		return 0, false
	}
}

func ownerPlotChange(index uint8, plot farm.Plot) farm.PlotChange {
	return farm.PlotChangeOf(index, plot)
}

func (o *Owner) publishResult(result CrossResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := o.bus.Publish(context.Background(), bus.TopicCrossResult, resultKey(result.VisitorUID), payload); err != nil {
		obs.L().Error("cross result publish failed",
			"component", "cross",
			"op", "publish_result",
			"code", int(result.Code),
			"err", err.Error(),
		)
		return err
	}
	return nil
}

func resultKey(uid uint64) string {
	return "uid:" + strconv.FormatUint(uid, 10)
}
