package farm

import (
	"testing"

	"farm/server/platform/gameconf"
	"farm/server/platform/pkgerr"
)

func TestRecalcLevelMatchesClientRule(t *testing.T) {
	agg := NewAggregate(1, "alice")

	cases := []struct {
		exp   uint32
		level uint16
	}{
		{0, 0},
		{199, 0},
		{200, 1},
		{1999, 9},
		{2000, 10}, // 苹果 UnlockLevel=10
		{2001, 10},
	}
	for _, tt := range cases {
		agg.Exp = tt.exp
		agg.RecalcLevel()
		if agg.Level != tt.level {
			t.Fatalf("Exp=%d → Level=%d, want %d", tt.exp, agg.Level, tt.level)
		}
	}
	if gameconf.ExpPerLevel != 200 {
		t.Fatalf("ExpPerLevel=%d, want 200（与客户端 EXP_PER_LEVEL 一致）", gameconf.ExpPerLevel)
	}
}

func TestAppleUnlockViaAccumulatedExp(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Coin = 10_000
	const appleID uint16 = 4 // 历史持久化 numeric ID：苹果

	// 经验不足时苹果锁定
	agg.Exp = 1999
	agg.RecalcLevel()
	if agg.Level != 9 {
		t.Fatalf("Level=%d, want 9", agg.Level)
	}
	locked := agg.Buy(BuyReq{ItemID: appleID, Quantity: 1})
	if locked.Err != pkgerr.CropLocked {
		t.Fatalf("Buy apple at Lv9 Err=%d, want CropLocked", locked.Err)
	}

	// 再获 1 点经验（模拟锄地等），升到 Lv10 后可买苹果
	agg.Plots[0] = Plot{State: StateWasteland}
	till := agg.ApplyPlotAction(PlotAction{Kind: Till, PlotIndex: 0}, actionNow)
	if till.Err != pkgerr.OK {
		t.Fatalf("Till Err=%d, want OK", till.Err)
	}
	if agg.Exp != 2002 { // 1999 + 3
		t.Fatalf("Exp=%d, want 2002", agg.Exp)
	}
	if agg.Level != 10 {
		t.Fatalf("Level=%d, want 10 after till", agg.Level)
	}

	bought := agg.Buy(BuyReq{ItemID: appleID, Quantity: 1})
	if bought.Err != pkgerr.OK {
		t.Fatalf("Buy apple at Lv10 Err=%d, want OK", bought.Err)
	}
	if got := agg.Items[SeedItem(appleID)]; got != 1 {
		t.Fatalf("apple seed count=%d, want 1", got)
	}

	plant := agg.ApplyPlotAction(PlotAction{Kind: Plant, PlotIndex: 0, Arg: appleID}, actionNow)
	if plant.Err != pkgerr.OK {
		t.Fatalf("Plant apple Err=%d, want OK", plant.Err)
	}
}

func TestHarvestRecalcLevel(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Exp = 1990
	agg.RecalcLevel()
	agg.Plots[0] = Plot{
		State:          StateMature,
		CropID:         1,
		FinalYield:     16,
		SeasonDuration: 10 * gameconf.HourMs(gameconf.TimeProfileDemo),
		MatureAt:       actionNow,
	}

	res := agg.ApplyPlotAction(PlotAction{Kind: Harvest, PlotIndex: 0}, actionNow)
	if res.Err != pkgerr.OK {
		t.Fatalf("Harvest Err=%d, want OK", res.Err)
	}
	// 白萝卜 HarvestExp=15 → 2005 → Lv10
	if agg.Exp != 2005 {
		t.Fatalf("Exp=%d, want 2005", agg.Exp)
	}
	if agg.Level != 10 {
		t.Fatalf("Level=%d, want 10 after harvest", agg.Level)
	}
}
