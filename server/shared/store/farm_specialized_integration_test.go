//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/outbox"
)

func TestSpecializedFarmCommitPlans(t *testing.T) {
	storage := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := storage.SaveAccount(ctx, uid, "specialized_"+time.Now().Format("150405.000000"), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	aggregate, err := storage.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm: %v", err)
	}
	aggregate.Plots[0].State = farm.StateTilled
	if err := storage.SaveFarm(ctx, aggregate); err != nil {
		t.Fatalf("seed full farm: %v", err)
	}

	// Economy persists coin/items but must not rewrite plots.
	aggregate.Coin = 777
	aggregate.Items[farm.DogFoodItem()] = 9
	aggregate.Plots[0].State = farm.StateMature
	aggregate.FarmSeq++
	if err := storage.SaveEconomy(ctx, aggregate); err != nil {
		t.Fatalf("SaveEconomy: %v", err)
	}
	if err := storage.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatal(err)
	}
	reloaded, err := storage.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Coin != 777 || reloaded.Items[farm.DogFoodItem()] != 9 || reloaded.Plots[0].State != farm.StateTilled {
		t.Fatalf("economy reload coin=%d food=%d plot=%d", reloaded.Coin, reloaded.Items[farm.DogFoodItem()], reloaded.Plots[0].State)
	}

	// A local plot plan persists the player hot fields, one selected plot and
	// only the explicitly requested inventory/codex tables.
	reloaded.Level = 3
	reloaded.Exp = 123
	reloaded.Plots[0].State = farm.StateGrowing
	reloaded.Plots[1].State = farm.StateMature // must remain untouched
	reloaded.Items[farm.DogFoodItem()] = 11
	reloaded.CodexHarvests[1] = 2
	reloaded.FarmSeq++
	if err := storage.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: reloaded,
		Plan: outbox.PersistPlan{
			Mode: outbox.PersistPlot, PlotIndex: 0, IncludeItems: true, IncludeCodex: true,
		},
	}}); err != nil {
		t.Fatalf("CommitFarms local plot: %v", err)
	}
	if err := storage.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatal(err)
	}
	reloaded, err = storage.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Level != 3 || reloaded.Exp != 123 || reloaded.Items[farm.DogFoodItem()] != 11 ||
		reloaded.CodexHarvests[1] != 2 || reloaded.Plots[0].State != farm.StateGrowing ||
		reloaded.Plots[1].State != farm.StateWasteland {
		t.Fatalf("local plot reload level=%d exp=%d food=%d codex=%d plot0=%d plot1=%d",
			reloaded.Level, reloaded.Exp, reloaded.Items[farm.DogFoodItem()], reloaded.CodexHarvests[1],
			reloaded.Plots[0].State, reloaded.Plots[1].State)
	}

	// Reservation persists daily/cross state and deliberately skips inventory.
	reloaded.Coin = 700
	reloaded.Daily = farm.DailyState{DayID: 99, MaintainCnt: 3}
	reloaded.CrossPending = map[uint64]farm.CrossReservation{
		11: {ReqID: 11, OwnerUID: uid + 1, ReservedAt: 123},
	}
	reloaded.Items[farm.DogFoodItem()] = 17
	reloaded.FarmSeq++
	if err := storage.SaveCrossVisitor(ctx, reloaded, false); err != nil {
		t.Fatalf("SaveCrossVisitor: %v", err)
	}
	if err := storage.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatal(err)
	}
	reloaded, err = storage.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Coin != 700 || reloaded.Daily.DayID != 99 || len(reloaded.CrossPending) != 1 || reloaded.Items[farm.DogFoodItem()] != 11 {
		t.Fatalf("visitor reload coin=%d daily=%#v pending=%#v food=%d", reloaded.Coin, reloaded.Daily, reloaded.CrossPending, reloaded.Items[farm.DogFoodItem()])
	}

	// Owner commit atomically persists one plot, receipt and result outbox.
	reloaded.Plots[0].State = farm.StateWasteland
	reloaded.Plots[1].State = farm.StateTilled // must remain untouched by the one-plot plan
	reloaded.CrossReceipts = map[uint64]farm.CrossReceipt{
		12: {ReqID: 12, VisitorUID: uid + 1, OwnerUID: uid, Code: 0, CreatedAt: time.Now().UnixMilli()},
	}
	reloaded.FarmSeq++
	event, err := outbox.NewCrossResultEvent(uid, &farmv1.CrossResult{
		ReqId: 12, VisitorUid: uid + 1, OwnerUid: uid,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.MarkOutboxPublished(context.Background(), event.EventID) })
	if err := storage.CommitCrossOwner(ctx, reloaded, 0, event); err != nil {
		t.Fatalf("CommitCrossOwner: %v", err)
	}
	if err := storage.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatal(err)
	}
	reloaded, err = storage.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Plots[0].State != farm.StateWasteland || reloaded.Plots[1].State != farm.StateWasteland || len(reloaded.CrossReceipts) != 1 {
		t.Fatalf("owner reload plot0=%d plot1=%d receipts=%#v", reloaded.Plots[0].State, reloaded.Plots[1].State, reloaded.CrossReceipts)
	}
	rows, err := storage.ClaimDueOutbox(ctx, 4, time.Now().Add(time.Second).UnixMilli())
	if err != nil || len(rows) != 1 || rows[0].EventID != event.EventID {
		t.Fatalf("outbox rows=%#v err=%v", rows, err)
	}
}
