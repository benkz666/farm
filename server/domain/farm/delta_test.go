package farm

import "testing"

func TestDeltaRingAllocatesLazily(t *testing.T) {
	var ring DeltaRing
	if ring.deltas != nil {
		t.Fatal("zero-value ring allocated storage")
	}
	ring.Append(FarmDelta{OwnerUID: 7, FarmSeq: 1})
	if len(ring.deltas) != 1 || cap(ring.deltas) >= DeltaRingCapacity {
		t.Fatalf("first append len/cap = %d/%d, want a small lazy allocation", len(ring.deltas), cap(ring.deltas))
	}
}

func TestDeltaRingSinceRejectsEvictedSequence(t *testing.T) {
	var ring DeltaRing
	for seq := uint64(1); seq <= DeltaRingCapacity+1; seq++ {
		ring.Append(FarmDelta{OwnerUID: 7, FarmSeq: seq})
	}

	if _, ok := ring.Since(1); ok {
		t.Fatal("Since accepted an evicted sequence")
	}

	got, ok := ring.Since(2)
	if !ok {
		t.Fatal("Since rejected the oldest retained sequence")
	}
	if len(got) != DeltaRingCapacity {
		t.Fatalf("delta count = %d, want %d", len(got), DeltaRingCapacity)
	}
	if got[0].FarmSeq != 2 || got[len(got)-1].FarmSeq != DeltaRingCapacity+1 {
		t.Fatalf("delta range = %d..%d, want 2..%d", got[0].FarmSeq, got[len(got)-1].FarmSeq, DeltaRingCapacity+1)
	}
}

func TestDeltaRingReturnsIndependentDeltaCopies(t *testing.T) {
	var ring DeltaRing
	guardDog := GuardDogSnapshot{ActiveDog: DogMutt, BowlEmptyAt: 10_000}
	delta := FarmDelta{
		OwnerUID: 7,
		FarmSeq:  1,
		Plots:    []PlotChange{{Index: 0, State: StateTilled}},
		GuardDog: &guardDog,
	}
	ring.Append(delta)
	delta.Plots[0].State = StateGrowing
	guardDog.ActiveDog = DogMastiff

	got, ok := ring.Since(1)
	if !ok || len(got) != 1 {
		t.Fatalf("Since = (%#v, %t), want one retained delta", got, ok)
	}
	if got[0].Plots[0].State != StateTilled {
		t.Fatalf("stored plot state = %d, want %d", got[0].Plots[0].State, StateTilled)
	}
	if got[0].GuardDog == nil || got[0].GuardDog.ActiveDog != DogMutt {
		t.Fatalf("stored guard dog = %#v, want mutt", got[0].GuardDog)
	}

	got[0].Plots[0].State = StateGrowing
	got[0].GuardDog.ActiveDog = DogShepherd
	again, ok := ring.Since(1)
	if !ok || again[0].Plots[0].State != StateTilled ||
		again[0].GuardDog == nil || again[0].GuardDog.ActiveDog != DogMutt {
		t.Fatalf("ring was mutated through returned delta: %#v", again)
	}
}
