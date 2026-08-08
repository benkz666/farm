package farm

import (
	"testing"

	"farm/server/shared/gameconfig"
)

func TestNextAdvanceAtChoosesRiskBoundaryBeforeMaturity(t *testing.T) {
	aggregate := NewAggregate(7, "timer")
	aggregate.Plots[0] = Plot{
		State:          StateGrowing,
		SeasonStartAt:  1_000,
		SeasonDuration: 10_000,
		MatureAt:       11_000,
	}

	if got, want := aggregate.NextAdvanceAt(1_500), int64(2_000); got != want {
		t.Fatalf("NextAdvanceAt = %d, want %d", got, want)
	}

	aggregate.Plots[0].WeedNextWin = gameconfig.RiskWindowsPerSeason
	aggregate.Plots[0].PestNextWin = gameconfig.RiskWindowsPerSeason
	if got, want := aggregate.NextAdvanceAt(1_500), int64(11_000); got != want {
		t.Fatalf("NextAdvanceAt without hazards = %d, want %d", got, want)
	}
}

func TestNextAdvanceAtIgnoresNonGrowingPlots(t *testing.T) {
	aggregate := NewAggregate(8, "idle")
	if got := aggregate.NextAdvanceAt(1_000); got != 0 {
		t.Fatalf("NextAdvanceAt = %d, want 0", got)
	}
}
