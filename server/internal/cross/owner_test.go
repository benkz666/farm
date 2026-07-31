package cross

import (
	"context"
	"encoding/json"
	"testing"

	"farm/server/internal/actor"
	"farm/server/internal/bus"
	"farm/server/internal/connreg"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

func TestOwnerRejectsNonFriendAction(t *testing.T) {
	eventBus := bus.NewMemoryBus()
	t.Cleanup(func() { _ = eventBus.Close() })

	runtime := ownerRuntime{actors: map[uint64]*actor.FarmActor{
		9: {Aggregate: growingAggregate(9)},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: false}, eventBus, func() int64 { return 40_000 }, nil, nil)
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	results := collectResults(t, eventBus)
	action := CrossAction{ReqID: 1, Kind: Water, VisitorUID: 7, OwnerUID: 9, PlotIndex: 0}
	publishAction(t, eventBus, action)

	if got := <-results; got.Code != pkgerr.NotFriend {
		t.Fatalf("result code = %d, want %d", got.Code, pkgerr.NotFriend)
	}
	if got := runtime.actors[9].Aggregate.Plots[0].LastWaterAt; got != 1 {
		t.Fatalf("non-friend action mutated owner plot: LastWaterAt = %d", got)
	}
}

func TestOwnerCommitsActionPublishesDeltaAndDeduplicatesReqID(t *testing.T) {
	eventBus := bus.NewMemoryBus()
	t.Cleanup(func() { _ = eventBus.Close() })

	runtime := ownerRuntime{actors: map[uint64]*actor.FarmActor{
		9: {Aggregate: growingAggregate(9)},
	}}
	deltas := make(chan farm.FarmDelta, 2)
	owner := NewOwner(
		runtime,
		ownerFriends{allowed: true},
		eventBus,
		func() int64 { return 40_000 },
		DeltaPublisherFunc(func(_ context.Context, delta farm.FarmDelta, _ connreg.ConnRef) error {
			deltas <- delta
			return nil
		}),
		nil,
	)
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	results := collectResults(t, eventBus)
	action := CrossAction{ReqID: 2, Kind: Water, VisitorUID: 7, OwnerUID: 9, PlotIndex: 0}
	publishAction(t, eventBus, action)
	first := <-results
	if first.Code != pkgerr.OK {
		t.Fatalf("first result = %#v, want OK", first)
	}
	delta := <-deltas
	if delta.OwnerUID != 9 || delta.FarmSeq != 1 || len(delta.Plots) != 1 || delta.Plots[0].Index != 0 {
		t.Fatalf("delta = %#v", delta)
	}

	publishAction(t, eventBus, action)
	duplicate := <-results
	if duplicate != first {
		t.Fatalf("duplicate result = %#v, want cached %#v", duplicate, first)
	}
	select {
	case extra := <-deltas:
		t.Fatalf("duplicate request emitted another delta: %#v", extra)
	default:
	}
	if got := runtime.actors[9].Aggregate.FarmSeq; got != 1 {
		t.Fatalf("owner farm seq = %d, want 1", got)
	}
}

func TestOwnerReturnsAlreadyWateredWithoutCommitting(t *testing.T) {
	eventBus := bus.NewMemoryBus()
	t.Cleanup(func() { _ = eventBus.Close() })

	aggregate := growingAggregate(9)
	aggregate.Plots[0].LastWaterAt = 30_000
	runtime := ownerRuntime{actors: map[uint64]*actor.FarmActor{
		9: {Aggregate: aggregate},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: true}, eventBus, func() int64 { return 40_000 }, nil, nil)
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	results := collectResults(t, eventBus)
	publishAction(t, eventBus, CrossAction{
		ReqID:      3,
		Kind:       Water,
		VisitorUID: 7,
		OwnerUID:   9,
		PlotIndex:  0,
	})
	if got := <-results; got.Code != pkgerr.AlreadyWatered {
		t.Fatalf("result code = %d, want %d", got.Code, pkgerr.AlreadyWatered)
	}
	if aggregate.FarmSeq != 0 {
		t.Fatalf("owner farm seq = %d, want 0", aggregate.FarmSeq)
	}
}

func TestOwnerInterceptedStealCreditsFrozenCompensation(t *testing.T) {
	ownerAggregate := farm.NewAggregate(9, "owner")
	ownerAggregate.Plots[0] = farm.Plot{
		State:          farm.StateMature,
		CropID:         1,
		FinalYield:     16,
		HarvestRound:   1,
		SeasonDuration: 60_000,
		MatureAt:       40_000,
	}
	ownerAggregate.Pet = farm.PetState{
		ActiveDog:   farm.DogShepherd,
		Owned:       0b010,
		Intercepts:  [3]uint16{0, 19, 0},
		BowlEmptyAt: 100_000,
		MsPerGram:   farm.ShepherdMsPerGram,
	}
	runtime := ownerRuntime{actors: map[uint64]*actor.FarmActor{
		9: {Aggregate: ownerAggregate},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: true}, nil, func() int64 { return 40_000 }, nil, nil)
	owner.stealRoll = func(CrossAction) uint16 { return 1 }
	owner.interceptRoll = func(CrossAction) uint8 { return 0 }

	action := CrossAction{
		ReqID:        4,
		Kind:         Steal,
		VisitorUID:   7,
		OwnerUID:     9,
		PlotIndex:    0,
		CropID:       1,
		Compensation: 170,
	}
	if _, ok := owner.validate(action); !ok {
		t.Fatal("action must pass owner validation")
	}
	outcome, err := owner.commit(action)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	result := outcome.result

	if result.Code != pkgerr.StealIntercepted || result.Compensation != 170 || result.DogType != farm.DogShepherd {
		t.Fatalf("result = %#v", result)
	}
	if ownerAggregate.Coin != 1_170 || ownerAggregate.Plots[0].StolenCount != 0 || len(ownerAggregate.Plots[0].Stealers) != 1 {
		t.Fatalf("owner aggregate after interception = %#v", ownerAggregate)
	}
	if outcome.playerDelta == nil || outcome.playerDelta.Pet == nil ||
		outcome.playerDelta.Pet.ActiveDog != farm.DogShepherd ||
		outcome.playerDelta.Pet.DogLevel != 1 ||
		outcome.playerDelta.Pet.InterceptionPct != 36 {
		t.Fatalf("pet growth player delta = %#v", outcome.playerDelta)
	}
}

func collectResults(t *testing.T, eventBus bus.EventBus) <-chan CrossResult {
	t.Helper()
	results := make(chan CrossResult, 4)
	if err := eventBus.Subscribe(context.Background(), bus.TopicCrossResult, func(_ string, payload []byte) error {
		var result CrossResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return err
		}
		results <- result
		return nil
	}); err != nil {
		t.Fatalf("subscribe result: %v", err)
	}
	return results
}

func publishAction(t *testing.T, eventBus bus.EventBus, action CrossAction) {
	t.Helper()
	payload, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("marshal action: %v", err)
	}
	if err := eventBus.Publish(context.Background(), bus.TopicCrossAction, "9", payload); err != nil {
		t.Fatalf("publish action: %v", err)
	}
}

func growingAggregate(uid uint64) *farm.Aggregate {
	aggregate := farm.NewAggregate(uid, "owner")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonStartAt:  1,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
		LastSettleAt:   40_000,
		LastWaterAt:    1,
		WeedNextWin:    10,
		PestNextWin:    10,
	}
	return aggregate
}

type ownerRuntime struct {
	actors map[uint64]*actor.FarmActor
}

func (r ownerRuntime) Do(uid uint64, fn func(*actor.FarmActor) error) error {
	return fn(r.actors[uid])
}

type ownerFriends struct {
	allowed bool
}

func (f ownerFriends) AreFriends(context.Context, uint64, uint64) (bool, error) {
	return f.allowed, nil
}
