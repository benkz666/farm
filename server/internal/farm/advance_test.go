package farm

import (
	"testing"

	"farm/server/internal/gameconf"
)

const (
	testSeasonStart    int64 = 1_000
	testSeasonDuration int64 = 1_000
)

func TestAdvanceMaturesAndFinalizesYield(t *testing.T) {
	p := growingPlot()
	cfg := advanceConfig(t, p.CropID)
	cfg.WeedThreshold = 0
	cfg.PestThreshold = 0

	Advance(&p, p.MatureAt, cfg)

	if p.State != StateMature {
		t.Fatalf("State = %d, want StateMature", p.State)
	}
	if p.FinalYield != 14 {
		t.Fatalf("FinalYield = %d, want 14", p.FinalYield)
	}
	if p.LastSettleAt != p.MatureAt {
		t.Fatalf("LastSettleAt = %d, want %d", p.LastSettleAt, p.MatureAt)
	}
	if p.HarvestRound != 1 {
		t.Fatalf("HarvestRound = %d, want 1", p.HarvestRound)
	}
}

func TestAdvanceWithersAfterMatureGracePeriod(t *testing.T) {
	p := growingPlot()
	now := p.MatureAt + 3*p.SeasonDuration
	cfg := advanceConfig(t, p.CropID)
	cfg.WeedThreshold = 0
	cfg.PestThreshold = 0

	Advance(&p, now, cfg)

	if p.State != StateWithered {
		t.Fatalf("State = %d, want StateWithered", p.State)
	}
	if p.FinalYield != 0 {
		t.Fatalf("FinalYield = %d, want 0", p.FinalYield)
	}
	if p.CropID != 0 {
		t.Fatalf("CropID = %d, want 0", p.CropID)
	}
}

func TestAdvanceAccruesDryHealthPenaltyWhileGrowing(t *testing.T) {
	p := growingPlot()
	now := testSeasonStart + 500
	cfg := advanceConfig(t, p.CropID)
	cfg.WeedThreshold = 0
	cfg.PestThreshold = 0

	Advance(&p, now, cfg)

	const wantAccrued = 44 * 150
	if p.AccruedWeighted != wantAccrued {
		t.Fatalf("AccruedWeighted = %d, want %d", p.AccruedWeighted, wantAccrued)
	}
	if p.LastSettleAt != now {
		t.Fatalf("LastSettleAt = %d, want %d", p.LastSettleAt, now)
	}
}

func TestAdvanceHazardDeterministicAcrossSegmentedAndDirect(t *testing.T) {
	cfg := forceWeedConfig(t, 1)
	cfg.OwnerUID = 42
	cfg.PlotIndex = 3

	direct := growingPlot()
	direct.PlantNonce = 7
	direct.LastWaterAt = testSeasonStart + testSeasonDuration // 排除缺水，只观察草
	Advance(&direct, direct.MatureAt-1, cfg)

	if direct.WeedSince == 0 {
		t.Fatal("expected forced weed threshold to spawn weed before mature")
	}

	segmented := growingPlot()
	segmented.PlantNonce = 7
	segmented.LastWaterAt = testSeasonStart + testSeasonDuration
	mid := testSeasonStart + testSeasonDuration/2
	Advance(&segmented, mid, cfg)
	Advance(&segmented, segmented.MatureAt-1, cfg)

	if segmented.WeedSince != direct.WeedSince {
		t.Fatalf("WeedSince segmented=%d direct=%d", segmented.WeedSince, direct.WeedSince)
	}
	if segmented.WeedNextWin != direct.WeedNextWin {
		t.Fatalf("WeedNextWin segmented=%d direct=%d", segmented.WeedNextWin, direct.WeedNextWin)
	}
	if segmented.AccruedWeighted != direct.AccruedWeighted {
		t.Fatalf("AccruedWeighted segmented=%d direct=%d", segmented.AccruedWeighted, direct.AccruedWeighted)
	}
}

func TestAdvanceHazardReplaySameInputsSameResult(t *testing.T) {
	cfg := advanceConfig(t, 1)
	cfg.OwnerUID = 1001
	cfg.PlotIndex = 2

	run := func() Plot {
		p := growingPlot()
		p.PlantNonce = 99
		p.LastWaterAt = testSeasonStart + testSeasonDuration
		Advance(&p, p.MatureAt-1, cfg)
		return p
	}

	a, b := run(), run()
	if a.WeedNextWin == 0 && a.PestNextWin == 0 {
		t.Fatal("expected risk windows to be scanned during advance")
	}
	if a.WeedSince != b.WeedSince || a.PestSince != b.PestSince ||
		a.WeedNextWin != b.WeedNextWin || a.PestNextWin != b.PestNextWin ||
		a.AccruedWeighted != b.AccruedWeighted {
		t.Fatalf("replay diverged:\n a=%+v\n b=%+v", a, b)
	}
}

func TestAdvanceHazardWeedIndependentOfPestThreshold(t *testing.T) {
	base := advanceConfig(t, 1)
	base.OwnerUID = 7
	base.PlotIndex = 1
	base.WeedThreshold = 10000 // 必中草
	base.PestThreshold = 0     // 永不出虫

	withPestOff := growingPlot()
	withPestOff.PlantNonce = 3
	withPestOff.LastWaterAt = testSeasonStart + testSeasonDuration
	Advance(&withPestOff, withPestOff.MatureAt-1, base)

	pestAlways := base
	pestAlways.PestThreshold = 10000
	withPestOn := growingPlot()
	withPestOn.PlantNonce = 3
	withPestOn.LastWaterAt = testSeasonStart + testSeasonDuration
	Advance(&withPestOn, withPestOn.MatureAt-1, pestAlways)

	if withPestOff.WeedSince == 0 || withPestOn.WeedSince == 0 {
		t.Fatal("weed should spawn under forced weed threshold")
	}
	if withPestOff.WeedSince != withPestOn.WeedSince || withPestOff.WeedNextWin != withPestOn.WeedNextWin {
		t.Fatalf("weed outcome depends on pest threshold: off=%d/%d on=%d/%d",
			withPestOff.WeedSince, withPestOff.WeedNextWin, withPestOn.WeedSince, withPestOn.WeedNextWin)
	}
	if withPestOn.PestSince == 0 {
		t.Fatal("expected pest to spawn when pest threshold forced")
	}
	if withPestOff.PestSince != 0 {
		t.Fatal("pest should stay absent when pest threshold is 0")
	}
}

func growingPlot() Plot {
	return Plot{
		State:          StateGrowing,
		CropID:         1,
		SeasonStartAt:  testSeasonStart,
		SeasonDuration: testSeasonDuration,
		MatureAt:       testSeasonStart + testSeasonDuration,
		LastSettleAt:   testSeasonStart,
		LastWaterAt:    testSeasonStart,
	}
}

func advanceConfig(t *testing.T, cropID uint16) AdvanceConfig {
	t.Helper()

	crop, ok := gameconf.CropByID(cropID)
	if !ok {
		t.Fatalf("CropByID(%d) returned false", cropID)
	}
	cfg := NewAdvanceConfig(crop)
	cfg.HazardSalt = DeriveHazardSalt("farm-unit-test-hazard-secret")
	return cfg
}

func forceWeedConfig(t *testing.T, cropID uint16) AdvanceConfig {
	t.Helper()
	cfg := advanceConfig(t, cropID)
	cfg.WeedThreshold = 10000
	cfg.PestThreshold = 0
	return cfg
}

func TestNewAdvanceConfigDoesNotEmbedDefaultHazardSalt(t *testing.T) {
	crop, ok := gameconf.CropByID(1)
	if !ok {
		t.Fatal("CropByID(1) missing")
	}
	cfg := NewAdvanceConfig(crop)
	if cfg.HazardSalt != 0 {
		t.Fatalf("NewAdvanceConfig.HazardSalt = %d, want 0 (must be injected)", cfg.HazardSalt)
	}
}

func TestDeriveHazardSaltStableAndDistinct(t *testing.T) {
	a1 := DeriveHazardSalt("alpha-secret")
	a2 := DeriveHazardSalt("alpha-secret")
	b := DeriveHazardSalt("beta-secret")
	if a1 != a2 {
		t.Fatalf("same secret derived differently: %d vs %d", a1, a2)
	}
	if a1 == b {
		t.Fatal("different secrets derived to the same salt")
	}
	if a1 == 0 || b == 0 {
		t.Fatal("derived salt must be non-zero for these secrets")
	}
}

func TestHazardRollDependsOnSalt(t *testing.T) {
	p := &Plot{PlantNonce: 99, SeasonIndex: 1}
	cfg := AdvanceConfig{OwnerUID: 7, PlotIndex: 2, HazardSalt: DeriveHazardSalt("salt-a")}
	rollA := hazardRoll(cfg, p, HazardWeed, 3)
	cfg.HazardSalt = DeriveHazardSalt("salt-b")
	rollB := hazardRoll(cfg, p, HazardWeed, 3)
	if rollA == rollB {
		t.Fatal("hazardRoll identical for different salts")
	}
}
