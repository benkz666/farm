package cross

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/bus"
	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

const ownerDedupCapacity = 64

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
	Publish(ctx context.Context, delta farm.FarmDelta, originatorConnID uint64) error
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
type DeltaPublisherFunc func(context.Context, farm.FarmDelta, uint64) error

func (fn DeltaPublisherFunc) Publish(ctx context.Context, delta farm.FarmDelta, originatorConnID uint64) error {
	return fn(ctx, delta, originatorConnID)
}

// Owner consumes CrossAction messages for farms owned by this process and
// returns one CrossResult per delivery. It caches the last 64 results for each
// owner uid so at-least-once EventBus delivery cannot mutate a plot twice.
type Owner struct {
	runtime Runtime
	friends FriendChecker
	bus     bus.EventBus
	now     func() int64
	deltas  DeltaPublisher
	players PlayerDeltaPublisher
	owns    func(uint64) bool
	hints   StealHintWriter

	stealRoll     func() uint16
	interceptRoll func() uint8

	mu      sync.Mutex
	results map[uint64]*ownerResultCache
}

type ownerResultCache struct {
	order   []uint64
	byReqID map[uint64]CrossResult
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
		runtime: runtime,
		friends: friends,
		bus:     eventBus,
		now:     now,
		deltas:  deltas,
		owns:    owns,
		stealRoll: func() uint16 {
			return uint16(rand.IntN(10) + 1)
		},
		interceptRoll: func() uint8 {
			return uint8(rand.IntN(100))
		},
		results: make(map[uint64]*ownerResultCache),
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
	if !o.owns(action.OwnerUID) {
		return nil
	}

	o.mu.Lock()
	if cached, ok := o.cachedResult(action.OwnerUID, action.ReqID); ok {
		o.mu.Unlock()
		return o.publishResult(cached)
	}

	result, delta, playerDelta, stealable, refreshHint := o.decide(action)
	o.cacheResult(action.OwnerUID, result)
	o.mu.Unlock()

	if delta != nil && o.deltas != nil {
		_ = o.deltas.Publish(context.Background(), *delta, 0)
	}
	if playerDelta != nil && o.players != nil {
		_ = o.players.PublishPlayerDelta(context.Background(), action.OwnerUID, *playerDelta)
	}
	if refreshHint {
		o.writeStealHint(action.OwnerUID, stealable)
	}
	return o.publishResult(result)
}

func (o *Owner) writeStealHint(uid uint64, hasStealable bool) {
	if o == nil || o.hints == nil || uid == 0 {
		return
	}
	_ = o.hints.SetStealHint(context.Background(), uid, hasStealable)
}

func (o *Owner) decide(action CrossAction) (CrossResult, *farm.FarmDelta, *farm.PlayerDelta, bool, bool) {
	result := CrossResult{
		ReqID:      action.ReqID,
		VisitorUID: action.VisitorUID,
		OwnerUID:   action.OwnerUID,
		Code:       pkgerr.BadRequest,
	}
	if action.ReqID == 0 || action.VisitorUID == 0 || action.OwnerUID == 0 || action.VisitorUID == action.OwnerUID {
		return result, nil, nil, false, false
	}
	var kind farm.PlotActionKind
	if action.Kind != Steal {
		var ok bool
		kind, ok = ownerPlotActionKind(action.Kind)
		if !ok {
			return result, nil, nil, false, false
		}
	}
	if o.friends == nil {
		result.Code = pkgerr.NotFriend
		return result, nil, nil, false, false
	}
	friends, err := o.friends.AreFriends(context.Background(), action.VisitorUID, action.OwnerUID)
	if err != nil {
		result.Code = pkgerr.Internal
		return result, nil, nil, false, false
	}
	if !friends {
		result.Code = pkgerr.NotFriend
		return result, nil, nil, false, false
	}
	if o.runtime == nil {
		result.Code = pkgerr.Internal
		return result, nil, nil, false, false
	}
	if action.Kind == Steal {
		return o.decideSteal(action, result)
	}

	var delta *farm.FarmDelta
	err = o.runtime.Do(action.OwnerUID, func(owner *actor.FarmActor) error {
		if owner == nil || owner.Aggregate == nil {
			return errors.New("cross: owner actor aggregate is nil")
		}
		beforeSeq := owner.Aggregate.FarmSeq
		actionResult := owner.Aggregate.ApplyPlotAction(farm.PlotAction{
			Kind:      kind,
			PlotIndex: action.PlotIndex,
		}, o.now())
		result.Code = actionResult.Err
		if owner.Aggregate.FarmSeq != beforeSeq {
			emitted := farm.FarmDelta{
				OwnerUID: action.OwnerUID,
				FarmSeq:  owner.Aggregate.FarmSeq,
				Plots:    []farm.PlotChange{ownerPlotChange(action.PlotIndex, owner.Aggregate.Plots[action.PlotIndex])},
			}
			owner.Deltas.Append(emitted)
			delta = &emitted
		}
		return nil
	})
	if err != nil {
		result.Code = pkgerr.Internal
		return result, nil, nil, false, false
	}
	return result, delta, nil, false, false
}

func (o *Owner) decideSteal(action CrossAction, result CrossResult) (CrossResult, *farm.FarmDelta, *farm.PlayerDelta, bool, bool) {
	var delta *farm.FarmDelta
	var playerDelta *farm.PlayerDelta
	var stealable bool
	var refreshHint bool
	err := o.runtime.Do(action.OwnerUID, func(owner *actor.FarmActor) error {
		if owner == nil || owner.Aggregate == nil {
			return errors.New("cross: owner actor aggregate is nil")
		}
		if int(action.PlotIndex) >= int(owner.Aggregate.UnlockedPlots) || int(action.PlotIndex) >= len(owner.Aggregate.Plots) {
			result.Code = pkgerr.PlotNotFound
			return nil
		}
		plot := owner.Aggregate.Plots[action.PlotIndex]
		if plot.CropID != action.CropID {
			result.Code = pkgerr.BadRequest
			return nil
		}
		crop, ok := gameconf.CropByID(action.CropID)
		if !ok || action.Compensation != int64(crop.FruitPrice)*10 {
			result.Code = pkgerr.BadRequest
			return nil
		}

		beforeSeq := owner.Aggregate.FarmSeq
		intercept := owner.Aggregate.Pet.ShouldIntercept(o.now(), o.interceptRoll())
		steal := owner.Aggregate.ApplySteal(farm.StealAction{
			VisitorUID: action.VisitorUID,
			PlotIndex:  action.PlotIndex,
			Roll:       o.stealRoll(),
			Intercept:  intercept,
		}, o.now())
		result.Code = steal.Err
		result.CropID = steal.CropID
		result.Amount = steal.Amount
		if steal.Err == pkgerr.StealIntercepted {
			result.Compensation = action.Compensation
			result.DogType = owner.Aggregate.Pet.ActiveDog
			owner.Aggregate.ReceiveStealCompensation(action.Compensation)
			owner.Aggregate.RecordPetIntercept()
			emitted := owner.Aggregate.PlayerDelta()
			playerDelta = &emitted
		}
		if owner.Aggregate.FarmSeq != beforeSeq {
			emitted := farm.FarmDelta{
				OwnerUID: action.OwnerUID,
				FarmSeq:  owner.Aggregate.FarmSeq,
				Plots:    []farm.PlotChange{ownerPlotChange(action.PlotIndex, owner.Aggregate.Plots[action.PlotIndex])},
			}
			owner.Deltas.Append(emitted)
			delta = &emitted
			stealable = owner.Aggregate.HasStealable()
			refreshHint = true
		}
		return nil
	})
	if err != nil {
		result.Code = pkgerr.Internal
		return result, nil, nil, false, false
	}
	return result, delta, playerDelta, stealable, refreshHint
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
	snapshot := farm.PlotSnapshotOf(index, plot)
	return farm.PlotChange{
		Index:          snapshot.Index,
		State:          snapshot.State,
		CropID:         snapshot.CropID,
		SeasonIndex:    snapshot.SeasonIndex,
		SeasonTotal:    snapshot.SeasonTotal,
		MatureAt:       snapshot.MatureAt,
		SeasonDuration: snapshot.SeasonDuration,
		FinalYield:     snapshot.FinalYield,
		LastWaterAt:    snapshot.LastWaterAt,
		WeedSince:      snapshot.WeedSince,
		PestSince:      snapshot.PestSince,
	}
}

func (o *Owner) publishResult(result CrossResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return o.bus.Publish(context.Background(), bus.TopicCrossResult, resultKey(result.VisitorUID), payload)
}

func resultKey(uid uint64) string {
	return "uid:" + strconv.FormatUint(uid, 10)
}

func (o *Owner) cachedResult(ownerUID, reqID uint64) (CrossResult, bool) {
	cache := o.results[ownerUID]
	if cache == nil {
		return CrossResult{}, false
	}
	result, ok := cache.byReqID[reqID]
	return result, ok
}

func (o *Owner) cacheResult(ownerUID uint64, result CrossResult) {
	cache := o.results[ownerUID]
	if cache == nil {
		cache = &ownerResultCache{byReqID: make(map[uint64]CrossResult)}
		o.results[ownerUID] = cache
	}
	if _, exists := cache.byReqID[result.ReqID]; exists {
		return
	}
	cache.byReqID[result.ReqID] = result
	cache.order = append(cache.order, result.ReqID)
	if len(cache.order) <= ownerDedupCapacity {
		return
	}
	oldest := cache.order[0]
	cache.order = cache.order[1:]
	delete(cache.byReqID, oldest)
}
