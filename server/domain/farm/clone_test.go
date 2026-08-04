package farm

import (
	"sync"
	"testing"
)

func TestAggregateCloneDeepCopiesMaps(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.AddItem(SeedItem(1), 3)
	agg.CodexHarvests[1] = 5
	agg.CrossPending = map[uint64]CrossReservation{
		7: {ReqID: 7, OwnerUID: 2, FrozenCoin: 10, ReservedAt: 100},
	}
	agg.CrossReceipts = map[uint64]CrossReceipt{
		9: {ReqID: 9, VisitorUID: 3, OwnerUID: 1, Code: 0, CreatedAt: 200},
	}

	snap := agg.Clone()
	agg.Coin++
	agg.Items[SeedItem(1)] = 99
	agg.CodexHarvests[1] = 88
	agg.CrossPending[7] = CrossReservation{ReqID: 7, OwnerUID: 2, FrozenCoin: 999}
	agg.CrossReceipts[9] = CrossReceipt{ReqID: 9, VisitorUID: 3, OwnerUID: 1, Code: 1}

	if snap.Coin == agg.Coin {
		t.Fatal("Clone must not share Coin with source")
	}
	if snap.Items[SeedItem(1)] == agg.Items[SeedItem(1)] {
		t.Fatal("Clone must not share Items map entries with source")
	}
	if snap.CodexHarvests[1] == agg.CodexHarvests[1] {
		t.Fatal("Clone must not share CodexHarvests with source")
	}
	if snap.CrossPending[7].FrozenCoin == agg.CrossPending[7].FrozenCoin {
		t.Fatal("Clone must not share CrossPending with source")
	}
	if snap.CrossReceipts[9].Code == agg.CrossReceipts[9].Code {
		t.Fatal("Clone must not share CrossReceipts with source")
	}
}

func TestAggregateCloneRaceSafe(t *testing.T) {
	agg := NewAggregate(42, "race")
	agg.AddItem(SeedItem(1), 1)
	snap := agg.Clone()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = snap.Coin
			_ = snap.Items[SeedItem(1)]
			_ = snap.CodexHarvests[1]
			_, _ = snap.CrossPending[uint64(i)]
			_, _ = snap.CrossReceipts[uint64(i)]
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			agg.Coin++
			agg.AddItem(SeedItem(uint16(i%10+1)), 1)
			if agg.CodexHarvests == nil {
				agg.CodexHarvests = make(map[uint16]uint32)
			}
			agg.CodexHarvests[uint16(i%5+1)] = uint32(i)
			if agg.CrossPending == nil {
				agg.CrossPending = make(map[uint64]CrossReservation)
			}
			agg.CrossPending[uint64(i)] = CrossReservation{ReqID: uint64(i), OwnerUID: 99}
		}
	}()
	wg.Wait()
}
