package farm

import (
	"encoding/json"
	"testing"
)

func TestInitialSnapshotShape(t *testing.T) {
	agg := NewAggregate(1, "alice")
	snap := agg.Snapshot()

	if len(snap.Plots) != 18 {
		t.Fatalf("len(plots)=%d, want 18", len(snap.Plots))
	}
	if snap.Plots[0].State != 0 {
		t.Fatalf("plots[0].State=%d, want 0 (wasteland)", snap.Plots[0].State)
	}
	if snap.UnlockedPlots != 6 {
		t.Fatalf("UnlockedPlots=%d, want 6", snap.UnlockedPlots)
	}
	if snap.Coin != 1000 {
		t.Fatalf("Coin=%d, want 1000", snap.Coin)
	}
}

func TestMultiSeasonHarvestSnapshotAndPatchContainSeasonIndex(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateMature,
		CropID:         4,
		SeasonIndex:    0,
		SeasonTotal:    2,
		StageCount:     4,
		FinalYield:     23,
		SeasonDuration: 120_000,
		MatureAt:       10_000,
		LastSettleAt:   10_000,
		LastWaterAt:    10_000,
	}

	result := agg.ApplyPlotAction(PlotAction{Kind: Harvest, PlotIndex: 0}, 15_000)
	if result.Err != 0 {
		t.Fatalf("harvest error = %d, want 0", result.Err)
	}

	for name, source := range map[string]any{
		"snapshot": agg.Snapshot(),
		"patch":    agg.PatchFromAction(result),
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(source)
			if err != nil {
				t.Fatalf("marshal %s: %v", name, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatalf("decode %s: %v", name, err)
			}
			plot := firstPlot(t, decoded)
			if got := plot["season_index"]; got != float64(1) {
				t.Fatalf("season_index = %#v, want 1", got)
			}
			if got := plot["season_total"]; got != float64(2) {
				t.Fatalf("season_total = %#v, want 2", got)
			}
		})
	}
}

func firstPlot(t *testing.T, source map[string]any) map[string]any {
	t.Helper()

	if plots, ok := source["plots"].([]any); ok && len(plots) > 0 {
		plot, ok := plots[0].(map[string]any)
		if !ok {
			t.Fatalf("snapshot plot = %#v, want object", plots[0])
		}
		return plot
	}
	plot, ok := source["plot"].(map[string]any)
	if !ok {
		t.Fatalf("patch plot = %#v, want object", source["plot"])
	}
	return plot
}
