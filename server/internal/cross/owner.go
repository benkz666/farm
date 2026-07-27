package cross

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/bus"
	"farm/server/internal/farm"
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
	owns    func(uint64) bool

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
		results: make(map[uint64]*ownerResultCache),
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

	result, delta := o.decide(action)
	o.cacheResult(action.OwnerUID, result)
	o.mu.Unlock()

	if delta != nil && o.deltas != nil {
		_ = o.deltas.Publish(context.Background(), *delta, 0)
	}
	return o.publishResult(result)
}

func (o *Owner) decide(action CrossAction) (CrossResult, *farm.FarmDelta) {
	result := CrossResult{
		ReqID:      action.ReqID,
		VisitorUID: action.VisitorUID,
		OwnerUID:   action.OwnerUID,
		Code:       pkgerr.BadRequest,
	}
	if action.ReqID == 0 || action.VisitorUID == 0 || action.OwnerUID == 0 || action.VisitorUID == action.OwnerUID {
		return result, nil
	}
	kind, ok := ownerPlotActionKind(action.Kind)
	if !ok {
		return result, nil
	}
	if o.friends == nil {
		result.Code = pkgerr.NotFriend
		return result, nil
	}
	friends, err := o.friends.AreFriends(context.Background(), action.VisitorUID, action.OwnerUID)
	if err != nil {
		result.Code = pkgerr.Internal
		return result, nil
	}
	if !friends {
		result.Code = pkgerr.NotFriend
		return result, nil
	}
	if o.runtime == nil {
		result.Code = pkgerr.Internal
		return result, nil
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
		return result, nil
	}
	return result, delta
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
