package crossfarm

import (
	"context"
	"testing"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	"farm/server/shared/errcode"
)

func TestOwnerRejectsNonFriendAction(t *testing.T) {
	runtime := ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: growingAggregate(9)},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: false}, func() int64 { return 40_000 }, nil, nil)

	action := CrossAction{ReqID: 1, Kind: Water, VisitorUID: 7, OwnerUID: 9, PlotIndex: 0}
	got, err := owner.Apply(context.Background(), action)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Code != errcode.NotFriend {
		t.Fatalf("result code = %d, want %d", got.Code, errcode.NotFriend)
	}
	if runtime.actors[9].Aggregate.Plots[0].LastWaterAt != 1 {
		t.Fatalf("non-friend action mutated owner plot: LastWaterAt = %d", runtime.actors[9].Aggregate.Plots[0].LastWaterAt)
	}
}

func TestOwnerCommitsActionPublishesDeltaAndDeduplicatesReqID(t *testing.T) {
	runtime := ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: growingAggregate(9)},
	}}
	deltas := make(chan farm.FarmDelta, 2)
	owner := NewOwner(
		runtime,
		ownerFriends{allowed: true},
		func() int64 { return 40_000 },
		DeltaPublisherFunc(func(_ context.Context, delta farm.FarmDelta, _ presence.ConnRef) error {
			deltas <- delta
			return nil
		}),
		nil,
	)
	scheduled := 0
	owner.SetAdvanceScheduler(func(uid uint64, due int64) {
		if uid != 9 || due <= 0 {
			t.Fatalf("scheduled uid=%d due=%d", uid, due)
		}
		scheduled++
	})

	action := CrossAction{ReqID: 2, Kind: Water, VisitorUID: 7, OwnerUID: 9, PlotIndex: 0}
	first, err := owner.Apply(context.Background(), action)
	if err != nil || first.Code != errcode.OK {
		t.Fatalf("first result = %#v, err=%v", first, err)
	}
	delta := <-deltas
	if delta.OwnerUID != 9 || delta.FarmSeq != 1 || len(delta.Plots) != 1 || delta.Plots[0].Index != 0 {
		t.Fatalf("delta = %#v", delta)
	}
	if scheduled != 1 {
		t.Fatalf("schedule calls=%d, want 1", scheduled)
	}

	duplicate, err := owner.Apply(context.Background(), action)
	if err != nil || duplicate != first {
		t.Fatalf("duplicate result = %#v, want %#v", duplicate, first)
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

func TestOwnerReplaysPersistedReceiptAfterActorReplacement(t *testing.T) {
	agg := growingAggregate(9)
	runtime := ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: agg},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: true}, func() int64 { return 40_000 }, nil, nil)
	action := CrossAction{ReqID: 77, Kind: Water, VisitorUID: 7, OwnerUID: 9, PlotIndex: 0}

	first, err := owner.commit(action)
	if err != nil || first.result.Code != errcode.OK {
		t.Fatalf("first commit = %#v, err=%v", first, err)
	}
	if len(agg.CrossReceipts) != 1 {
		t.Fatalf("persisted receipts = %#v, want one", agg.CrossReceipts)
	}
	runtime.actors[9] = &room.FarmActor{Aggregate: agg}
	second, err := owner.commit(action)
	if err != nil {
		t.Fatalf("replay commit: %v", err)
	}
	if !second.replayed || second.result != first.result {
		t.Fatalf("replayed result = %#v, want %#v", second.result, first.result)
	}
	if second.delta != nil || agg.FarmSeq != 1 {
		t.Fatalf("replay changed owner state: delta=%#v farm_seq=%d", second.delta, agg.FarmSeq)
	}
}

func TestOwnerReturnsAlreadyWateredWithoutCommitting(t *testing.T) {
	aggregate := growingAggregate(9)
	aggregate.Plots[0].LastWaterAt = 30_000
	runtime := ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: aggregate},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: true}, func() int64 { return 40_000 }, nil, nil)

	got, err := owner.Apply(context.Background(), CrossAction{
		ReqID:      3,
		Kind:       Water,
		VisitorUID: 7,
		OwnerUID:   9,
		PlotIndex:  0,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Code != errcode.AlreadyWatered {
		t.Fatalf("result code = %d, want %d", got.Code, errcode.AlreadyWatered)
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
	runtime := ownerRuntime{actors: map[uint64]*room.FarmActor{
		9: {Aggregate: ownerAggregate},
	}}
	owner := NewOwner(runtime, ownerFriends{allowed: true}, func() int64 { return 40_000 }, nil, nil)
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
	if _, ok, err := owner.validate(context.Background(), action); err != nil || !ok {
		t.Fatal("action must pass owner validation")
	}
	outcome, err := owner.commit(action)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	result := outcome.result

	if result.Code != errcode.StealIntercepted || result.Compensation != 170 || result.DogType != farm.DogShepherd {
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
	actors map[uint64]*room.FarmActor
}

func (r ownerRuntime) Do(uid uint64, fn func(*room.FarmActor) error) error {
	return fn(r.actors[uid])
}

type ownerFriends struct {
	allowed bool
}

func (f ownerFriends) AreFriends(context.Context, uint64, uint64) (bool, error) {
	return f.allowed, nil
}
