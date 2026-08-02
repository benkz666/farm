package farm_test

import (
	"testing"

	"farm/server/platform/farm"
)

func TestVisitorSafeFarmSnapshotStripsPersonalEconomy(t *testing.T) {
	full := farm.FarmSnapshotJSON{
		OwnerUID:      99,
		Nickname:      "bob",
		Level:         7,
		Exp:           1400,
		Coin:          99999,
		UnlockedPlots: 8,
		Plots:         []farm.PlotSnapshot{{Index: 0, State: 1}},
		Bag:           map[string]uint32{"seed:1": 3},
		Warehouse:     map[string]uint32{"fruit:1": 5},
	}

	safe := farm.VisitorSafeFarmSnapshot(full)
	if safe.OwnerUID != 99 || safe.Nickname != "bob" || safe.Level != 7 {
		t.Fatalf("public fields = %#v", safe)
	}
	if safe.UnlockedPlots != 8 || len(safe.Plots) != 1 {
		t.Fatalf("farm view fields = %#v", safe)
	}
	if safe.Coin != 0 || safe.Exp != 0 {
		t.Fatalf("economy not redacted: coin=%d exp=%d", safe.Coin, safe.Exp)
	}
	if safe.Bag != nil || safe.Warehouse != nil {
		t.Fatalf("bag/warehouse must be nil for visitors: bag=%v warehouse=%v", safe.Bag, safe.Warehouse)
	}
	// 原快照不被原地修改
	if full.Coin != 99999 || full.Bag["seed:1"] != 3 {
		t.Fatalf("source mutated: %#v", full)
	}
}
