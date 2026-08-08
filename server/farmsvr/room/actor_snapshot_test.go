package room

import (
	"bytes"
	"encoding/json"
	"testing"

	"farm/server/domain/farm"
	"farm/server/shared/outbox"
)

func TestPersistPlanKeepsReducedModeUntilMixedMutation(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	actor.RequireEconomyFlush()
	if got := actor.pendingPersistPlan().Mode; got != outbox.PersistEconomy {
		t.Fatalf("economy plan mode = %d", got)
	}
	actor.RequireEconomyFlush()
	if got := actor.pendingPersistPlan().Mode; got != outbox.PersistEconomy {
		t.Fatalf("merged economy plan mode = %d", got)
	}
	actor.MarkDirty()
	if got := actor.pendingPersistPlan().Mode; got != outbox.PersistFull {
		t.Fatalf("mixed plan mode = %d, want full", got)
	}
}

func TestCrossOwnerPlanKeepsOutboxAtomic(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	event := outbox.Event{EventID: "event-1", Payload: []byte{1}}
	actor.RequireCrossOwnerFlush(3, event)
	actor.stampOutboxGeneration(1)
	plan := actor.pendingPersistPlan()
	if plan.Mode != outbox.PersistCrossOwner || plan.PlotIndex != 3 {
		t.Fatalf("cross owner plan = %#v", plan)
	}
	if events := actor.pendingOutboxEvents(); len(events) != 1 || events[0].EventID != event.EventID {
		t.Fatalf("pending outbox = %#v", events)
	}
}

func TestPlotPlanMergesOnlySamePlot(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	actor.MarkPlotDirty(2, false, false)
	actor.MarkPlotDirty(2, true, true)
	plan := actor.pendingPersistPlan()
	if plan.Mode != outbox.PersistPlot || plan.PlotIndex != 2 || !plan.IncludeItems || !plan.IncludeCodex {
		t.Fatalf("merged plot plan = %#v", plan)
	}

	actor.MarkPlotDirty(3, false, false)
	if got := actor.pendingPersistPlan().Mode; got != outbox.PersistFull {
		t.Fatalf("multi-plot plan mode = %d, want full", got)
	}
}

func TestEncodedSnapshotReusesCurrentVersionAndInvalidatesOnWrite(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	first, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	second, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || &first[0] != &second[0] {
		t.Fatal("unchanged aggregate did not reuse encoded snapshot")
	}

	actor.Aggregate.Coin = 9_007_199_254_740_993
	actor.MarkDirty()
	third, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if &first[0] == &third[0] {
		t.Fatal("dirty aggregate reused stale encoded snapshot")
	}
	var decoded farm.FarmSnapshotJSON
	if err := json.Unmarshal(third, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coin != actor.Aggregate.Coin {
		t.Fatalf("coin=%d, want %d", decoded.Coin, actor.Aggregate.Coin)
	}
}

func TestEncodedSnapshotRebuildsWhenAggregateSequenceAdvances(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	first, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	actor.Aggregate.Coin = 1234
	actor.Aggregate.FarmSeq++
	second, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("advanced farm_seq reused stale snapshot")
	}
	var decoded farm.FarmSnapshotJSON
	if err := json.Unmarshal(second, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Coin != 1234 {
		t.Fatalf("decoded snapshot = %#v", decoded)
	}
}

func TestSnapshotEncodingVersionsAreIndependent(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	jsonBefore, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a version advance before a caller asks only for Protobuf. The
	// Protobuf cache must not make the older JSON cache look current.
	actor.Aggregate.Coin = 4321
	actor.Aggregate.FarmSeq++
	if _, err := actor.EncodedSnapshotProto(); err != nil {
		t.Fatal(err)
	}
	jsonAfter, err := actor.EncodedSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(jsonBefore, jsonAfter) {
		t.Fatal("protobuf rebuild incorrectly promoted stale JSON cache")
	}
}

func BenchmarkSnapshotEncoding(b *testing.B) {
	aggregate := farm.NewAggregate(42, "alice")
	actor := &FarmActor{Aggregate: aggregate}
	if _, err := actor.EncodedSnapshot(); err != nil {
		b.Fatal(err)
	}
	b.Run("rebuild", func(b *testing.B) {
		for range b.N {
			encoded, err := json.Marshal(aggregate.Snapshot())
			if err != nil || len(encoded) == 0 {
				b.Fatal(err)
			}
		}
	})
	b.Run("preencoded", func(b *testing.B) {
		for range b.N {
			encoded, err := actor.EncodedSnapshot()
			if err != nil || !bytes.HasPrefix(encoded, []byte(`{"owner_uid":`)) {
				b.Fatal(err)
			}
		}
	})
}
