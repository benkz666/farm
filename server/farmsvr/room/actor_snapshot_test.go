package room

import (
	"bytes"
	"testing"

	"farm/server/domain/farm"
	"farm/server/shared/clientwire"
	"farm/server/shared/outbox"

	"google.golang.org/protobuf/proto"
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

func TestPendingWriteMutationContainsOnlyExactDirtyRows(t *testing.T) {
	agg := farm.NewAggregate(42, "alice")
	agg.Items[farm.SeedItem(1)] = 10
	actor := &FarmActor{Aggregate: agg}
	before := actor.SnapshotItems()
	agg.Items[farm.SeedItem(1)] = 9
	agg.CodexHarvests[1] = 3
	agg.FarmSeq = 7
	actor.RecordItemChanges(before)
	actor.RecordCodexChange(1)
	actor.MarkPlotDirty(2, true, true)
	actor.stampPersistGeneration(1)

	mutation, err := actor.pendingWriteMutation()
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Uid != 42 || mutation.FarmSeq != 7 || mutation.ReplaceItems || len(mutation.Items) != 1 || mutation.Items[0].Count != 9 {
		t.Fatalf("mutation = %#v", mutation)
	}
	if len(mutation.Plots) != 1 || mutation.Plots[0].Index != 2 || len(mutation.Codex) != 1 || mutation.Codex[0].CropId != 1 {
		t.Fatalf("incremental rows = %#v", mutation)
	}
	body, err := proto.Marshal(mutation)
	if err != nil || bytes.Contains(body, []byte(`"owner_uid"`)) {
		t.Fatalf("mutation is not protobuf-only: err=%v body=%q", err, body)
	}
}

func TestMixedPendingPlansRemainIncremental(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	actor.MarkPlotDirty(1, false, false)
	actor.RequireEconomyFlush()
	actor.stampPersistGeneration(1)
	mutation, err := actor.pendingWriteMutation()
	if err != nil {
		t.Fatal(err)
	}
	if mutation.PlayerMask == outbox.PlayerAll || mutation.ReplaceItems || len(mutation.Plots) != 1 {
		t.Fatalf("mixed mutation fell back to full snapshot: %#v", mutation)
	}
}

func TestFullMutationEncodesEmptyNotNullCrossBlobs(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	actor.MarkDirty()
	actor.stampPersistGeneration(1)
	mutation, err := actor.pendingWriteMutation()
	if err != nil {
		t.Fatal(err)
	}
	if mutation.CrossPendingJson == nil || mutation.CrossReceiptJson == nil {
		t.Fatalf("empty NOT NULL blobs encoded as nil: pending=%#v receipts=%#v", mutation.CrossPendingJson, mutation.CrossReceiptJson)
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

func TestSideEffectsAreStampedAndAcknowledgedByGeneration(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	actor.RecordTaskAdvance(outbox.TaskAdvance{DayKey: 20260808, TaskID: 3, Amount: 1})
	actor.RecordCodexReward(farm.CodexProgress{CropID: 1, HarvestCount: 10})
	actor.stampSideEffectGeneration(7)

	if got := actor.pendingTaskAdvances(); len(got) != 1 || got[0].TaskID != 3 {
		t.Fatalf("pending tasks = %#v", got)
	}
	if got := actor.pendingCodexRewards(); len(got) != 1 || got[0].Progress.HarvestCount != 10 {
		t.Fatalf("pending codex rewards = %#v", got)
	}
	actor.ackSideEffects(6)
	if len(actor.pendingTaskAdvances()) != 1 || len(actor.pendingCodexRewards()) != 1 {
		t.Fatal("an older acknowledgement removed newer side effects")
	}
	actor.ackSideEffects(7)
	if len(actor.pendingTaskAdvances()) != 0 || len(actor.pendingCodexRewards()) != 0 {
		t.Fatal("committed side effects were not removed")
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

func TestSnapshotProtoReusesCurrentVersionAndInvalidatesOnWrite(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	first, err := actor.SnapshotProto()
	if err != nil {
		t.Fatal(err)
	}
	second, err := actor.SnapshotProto()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("unchanged aggregate did not reuse protobuf snapshot")
	}

	actor.Aggregate.Coin = 9_007_199_254_740_993
	actor.MarkDirty()
	third, err := actor.SnapshotProto()
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("dirty aggregate reused stale protobuf snapshot")
	}
	if third.Coin != actor.Aggregate.Coin {
		t.Fatalf("coin=%d, want %d", third.Coin, actor.Aggregate.Coin)
	}
}

func TestSnapshotProtoRebuildsWhenAggregateSequenceAdvances(t *testing.T) {
	actor := &FarmActor{Aggregate: farm.NewAggregate(42, "alice")}
	first, err := actor.SnapshotProto()
	if err != nil {
		t.Fatal(err)
	}
	actor.Aggregate.Coin = 1234
	actor.Aggregate.FarmSeq++
	second, err := actor.SnapshotProto()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second.Coin != 1234 {
		t.Fatalf("advanced farm_seq reused stale snapshot: %#v", second)
	}
}

func BenchmarkSnapshotEncoding(b *testing.B) {
	aggregate := farm.NewAggregate(42, "alice")
	actor := &FarmActor{Aggregate: aggregate}
	if _, err := actor.SnapshotProto(); err != nil {
		b.Fatal(err)
	}
	b.Run("rebuild", func(b *testing.B) {
		for range b.N {
			message := clientwire.FarmSnapshotToProto(aggregate.Snapshot())
			encoded, err := proto.Marshal(message)
			if err != nil || len(encoded) == 0 {
				b.Fatal(err)
			}
		}
	})
	b.Run("preencoded", func(b *testing.B) {
		for range b.N {
			message, err := actor.SnapshotProto()
			if err != nil || message.OwnerUid != aggregate.UID {
				b.Fatal(err)
			}
		}
	})
}
