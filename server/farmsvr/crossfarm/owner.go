package crossfarm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"
)

// Runtime is the owner-side Actor boundary.
type Runtime interface {
	Do(uid uint64, fn func(*room.FarmActor) error) error
}

// FriendChecker provides the shared friendship authority needed to reject
// forged CrossAction messages at the owner boundary.
type FriendChecker interface {
	AreFriends(ctx context.Context, a, b uint64) (bool, error)
}

// DeltaPublisher broadcasts an already committed FarmDelta.
type DeltaPublisher interface {
	Publish(ctx context.Context, delta farm.FarmDelta, originator presence.ConnRef) error
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
type DeltaPublisherFunc func(context.Context, farm.FarmDelta, presence.ConnRef) error

func (fn DeltaPublisherFunc) Publish(ctx context.Context, delta farm.FarmDelta, originator presence.ConnRef) error {
	return fn(ctx, delta, originator)
}

// Owner consumes CrossAction requests for farms owned by this process.
type Owner struct {
	runtime         Runtime
	friends         FriendChecker
	now             func() int64
	deltas          DeltaPublisher
	players         PlayerDeltaPublisher
	owns            func(uint64) bool
	hints           StealHintWriter
	scheduleAdvance func(uid uint64, due int64)

	stealRoll     func(CrossAction) uint16
	interceptRoll func(CrossAction) uint8
}

type ownerOutcome struct {
	result      CrossResult
	delta       *farm.FarmDelta
	playerDelta *farm.PlayerDelta
	stealable   bool
	refreshHint bool
	replayed    bool
}

// NewOwner assembles the authoritative owner-side cross-farm handler.
func NewOwner(runtime Runtime, friends FriendChecker, now func() int64, deltas DeltaPublisher, owns func(uint64) bool) *Owner {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if owns == nil {
		owns = func(uint64) bool { return true }
	}
	return &Owner{
		runtime:       runtime,
		friends:       friends,
		now:           now,
		deltas:        deltas,
		owns:          owns,
		stealRoll:     defaultStealRoll,
		interceptRoll: defaultInterceptRoll,
	}
}

func (o *Owner) SetStealHintWriter(hints StealHintWriter) {
	if o != nil {
		o.hints = hints
	}
}

func (o *Owner) SetPlayerDeltaPublisher(publisher PlayerDeltaPublisher) {
	if o != nil {
		o.players = publisher
	}
}

// SetAdvanceScheduler refreshes the owner's next gameplay boundary after a
// cross-farm write. The callback is intentionally transport-neutral.
func (o *Owner) SetAdvanceScheduler(schedule func(uint64, int64)) {
	if o != nil {
		o.scheduleAdvance = schedule
	}
}

// Apply validates and adjudicates one cross action, recording a durable result
// outbox event before returning.
func (o *Owner) Apply(ctx context.Context, action CrossAction) (CrossResult, error) {
	if o == nil {
		return CrossResult{}, errors.New("cross: owner is nil")
	}
	if !o.owns(action.OwnerUID) {
		return CrossResult{}, nil
	}
	telemetry.L().Debug("cross owner apply",
		"component", "cross",
		"op", "apply",
		"kind", action.Kind,
	)
	rejected, ok, err := o.validate(ctx, action)
	if err != nil {
		return CrossResult{}, err
	}
	if !ok {
		return rejected, nil
	}
	outcome, err := o.commit(action)
	if err != nil {
		return CrossResult{}, fmt.Errorf("cross: owner commit: %w", err)
	}
	if !outcome.replayed {
		if outcome.delta != nil && o.deltas != nil {
			_ = o.deltas.Publish(context.Background(), *outcome.delta, presence.ConnRef{})
		}
		if outcome.playerDelta != nil && o.players != nil {
			_ = o.players.PublishPlayerDelta(context.Background(), action.OwnerUID, *outcome.playerDelta)
		}
		if outcome.refreshHint {
			o.writeStealHint(action.OwnerUID, outcome.stealable)
		}
	}
	return outcome.result, nil
}

func (o *Owner) writeStealHint(uid uint64, hasStealable bool) {
	if o == nil || o.hints == nil || uid == 0 {
		return
	}
	_ = o.hints.SetStealHint(context.Background(), uid, hasStealable)
}

func (o *Owner) validate(ctx context.Context, action CrossAction) (CrossResult, bool, error) {
	result := CrossResult{
		ReqID:      action.ReqID,
		VisitorUID: action.VisitorUID,
		OwnerUID:   action.OwnerUID,
		Code:       errcode.BadRequest,
	}
	if action.ReqID == 0 || action.VisitorUID == 0 || action.OwnerUID == 0 || action.VisitorUID == action.OwnerUID {
		return result, false, nil
	}
	if action.Kind != Steal {
		if _, ok := ownerPlotActionKind(action.Kind); !ok {
			return result, false, nil
		}
	}
	if o.friends == nil {
		result.Code = errcode.NotFriend
		return result, false, nil
	}
	friends, err := o.friends.AreFriends(ctx, action.VisitorUID, action.OwnerUID)
	if err != nil {
		return CrossResult{}, false, fmt.Errorf("cross: check friendship: %w", err)
	}
	if !friends {
		result.Code = errcode.NotFriend
		return result, false, nil
	}
	if o.runtime == nil {
		return CrossResult{}, false, errors.New("cross: owner runtime is nil")
	}
	return result, true, nil
}

func (o *Owner) commit(action CrossAction) (ownerOutcome, error) {
	var outcome ownerOutcome
	var nextAdvance int64
	err := o.runtime.Do(action.OwnerUID, func(owner *room.FarmActor) error {
		if owner == nil || owner.Aggregate == nil {
			return errors.New("cross: owner actor aggregate is nil")
		}
		defer func() {
			nextAdvance = owner.Aggregate.NextAdvanceAt(o.now())
		}()
		if cached, ok := owner.CachedResult(action.ReqID); ok {
			if previous, typed := cached.(CrossResult); typed {
				outcome.result = previous
				outcome.replayed = true
				// 上一次 durable commit 可能返回过不确定错误；回放必须重新等待
				// 当前快照（含 receipt/outbox）落盘，不能只相信进程内热缓存。
				owner.RequireFlush()
				return nil
			}
		}
		if receipt, ok := owner.Aggregate.FindCrossReceipt(action.ReqID, action.VisitorUID, action.OwnerUID, o.now()); ok {
			outcome.result = CrossResult{
				ReqID:        receipt.ReqID,
				VisitorUID:   receipt.VisitorUID,
				OwnerUID:     receipt.OwnerUID,
				Code:         errcode.Code(receipt.Code),
				CropID:       receipt.CropID,
				Amount:       receipt.Amount,
				Compensation: receipt.Compensation,
				DogType:      receipt.DogType,
			}
			owner.CacheResult(action.ReqID, outcome.result)
			outcome.replayed = true
			owner.RequireFlush()
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
		event, eventErr := outbox.NewCrossResultEvent(action.OwnerUID, resultToProto(outcome.result))
		if eventErr != nil {
			return eventErr
		}
		if int(action.PlotIndex) < len(owner.Aggregate.Plots) {
			owner.RequireCrossOwnerFlush(action.PlotIndex, event)
		} else {
			// An invalid client plot still needs a durable receipt/outbox, but
			// there is no safe single plot row for the reduced commit.
			owner.RecordOutbox(event)
			owner.RequireFlush()
		}
		return nil
	})
	if err != nil {
		return ownerOutcome{}, err
	}
	if o.scheduleAdvance != nil {
		o.scheduleAdvance(action.OwnerUID, nextAdvance)
	}
	return outcome, nil
}

func (o *Owner) applyMaintenance(owner *room.FarmActor, action CrossAction, outcome *ownerOutcome) {
	kind, ok := ownerPlotActionKind(action.Kind)
	if !ok {
		outcome.result.Code = errcode.BadRequest
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

func (o *Owner) applySteal(owner *room.FarmActor, action CrossAction, outcome *ownerOutcome) {
	if int(action.PlotIndex) >= int(owner.Aggregate.UnlockedPlots) || int(action.PlotIndex) >= len(owner.Aggregate.Plots) {
		outcome.result.Code = errcode.PlotNotFound
		return
	}
	if owner.Aggregate.Plots[action.PlotIndex].CropID != action.CropID {
		outcome.result.Code = errcode.BadRequest
		return
	}
	crop, ok := gameconfig.CropByID(action.CropID)
	if !ok || action.Compensation != gameconfig.StealCompensation(crop) {
		outcome.result.Code = errcode.BadRequest
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

	if steal.Err == errcode.StealIntercepted {
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

func (o *Owner) emitDelta(owner *room.FarmActor, ownerUID uint64, plotIndex uint8) *farm.FarmDelta {
	emitted := farm.FarmDelta{
		OwnerUID: ownerUID,
		FarmSeq:  owner.Aggregate.FarmSeq,
		Plots:    []farm.PlotChange{ownerPlotChange(plotIndex, owner.Aggregate.Plots[plotIndex])},
	}
	owner.Deltas.Append(emitted)
	return &emitted
}

const (
	stealRollSalt     uint64 = 0x53544c31
	interceptRollSalt uint64 = 0x444f4731
)

func defaultStealRoll(action CrossAction) uint16 {
	return uint16(deterministicRoll(action, stealRollSalt)%maxStealRoll + 1)
}

func defaultInterceptRoll(action CrossAction) uint8 {
	return uint8(deterministicRoll(action, interceptRollSalt) % 100)
}

const maxStealRoll = 10

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
