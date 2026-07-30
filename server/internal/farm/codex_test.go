package farm

import (
	"testing"

	"farm/server/internal/pkgerr"
)

func TestCodexProgressMilestones(t *testing.T) {
	tests := []struct {
		count      uint32
		wantTier   string
		wantTarget uint32
	}{
		{count: 1, wantTier: "wood", wantTarget: 10},
		{count: 9, wantTier: "wood", wantTarget: 10},
		{count: 10, wantTier: "bronze", wantTarget: 20},
		{count: 19, wantTier: "bronze", wantTarget: 20},
		{count: 20, wantTier: "silver", wantTarget: 50},
		{count: 49, wantTier: "silver", wantTarget: 50},
		{count: 50, wantTier: "gold", wantTarget: 0},
		{count: 77, wantTier: "gold", wantTarget: 0},
	}
	for _, tt := range tests {
		got := CodexProgressOf(1, tt.count)
		if got.Tier != tt.wantTier || got.NextTarget != tt.wantTarget {
			t.Fatalf("count %d => (%q, %d), want (%q, %d)",
				tt.count, got.Tier, got.NextTarget, tt.wantTier, tt.wantTarget)
		}
	}
}

func TestSuccessfulHarvestCountsOnceRegardlessOfYield(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateMature,
		CropID:         1,
		FinalYield:     16,
		SeasonDuration: 60_000,
		MatureAt:       actionNow,
	}

	result := agg.ApplyPlotAction(PlotAction{Kind: Harvest, PlotIndex: 0}, actionNow)
	if result.Err != pkgerr.OK {
		t.Fatalf("Harvest Err=%d, want OK", result.Err)
	}
	if got := agg.CodexHarvests[1]; got != 1 {
		t.Fatalf("CodexHarvests[1]=%d, want 1 action rather than 16 fruit", got)
	}
	if result.Patch.Codex == nil || result.Patch.Codex.HarvestCount != 1 ||
		result.Patch.Codex.Tier != "wood" || result.Patch.Codex.NextTarget != 10 {
		t.Fatalf("codex patch = %#v, want first-harvest wood plaque", result.Patch.Codex)
	}

	failed := agg.ApplyPlotAction(PlotAction{Kind: Harvest, PlotIndex: 0}, actionNow+1)
	if failed.Err == pkgerr.OK {
		t.Fatal("second harvest on residue unexpectedly succeeded")
	}
	if got := agg.CodexHarvests[1]; got != 1 {
		t.Fatalf("failed harvest changed count to %d, want 1", got)
	}
}
