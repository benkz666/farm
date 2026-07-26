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

	Advance(&p, p.MatureAt, advanceConfig(t, p.CropID))

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

	Advance(&p, now, advanceConfig(t, p.CropID))

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

	Advance(&p, now, advanceConfig(t, p.CropID))

	const wantAccrued = 44 * 150
	if p.AccruedWeighted != wantAccrued {
		t.Fatalf("AccruedWeighted = %d, want %d", p.AccruedWeighted, wantAccrued)
	}
	if p.LastSettleAt != now {
		t.Fatalf("LastSettleAt = %d, want %d", p.LastSettleAt, now)
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
	return NewAdvanceConfig(crop)
}
