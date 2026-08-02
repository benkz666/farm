package farm

import (
	"encoding/json"
	"strings"
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

func TestSnapshotAndPatchExposeHealthStolenCountFertMask(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:           StateGrowing,
		CropID:          1,
		SeasonIndex:     0,
		SeasonTotal:     1,
		StageCount:      3,
		FertMask:        0b101,
		StolenCount:     4,
		SeasonDuration:  1_000,
		MatureAt:        2_000,
		LastSettleAt:    1_500,
		LastWaterAt:     1_000,
		AccruedWeighted: 44 * 1_000, // 健康度 = 100 - 44 = 56
		Stealers:        []uint64{9, 8},
	}

	snap := agg.Snapshot()
	if snap.Plots[0].Health != 56 {
		t.Fatalf("snapshot health = %d, want 56", snap.Plots[0].Health)
	}
	if snap.Plots[0].StolenCount != 4 {
		t.Fatalf("snapshot stolen_count = %d, want 4", snap.Plots[0].StolenCount)
	}
	if snap.Plots[0].FertMask != 0b101 {
		t.Fatalf("snapshot fert_mask = %03b, want 101", snap.Plots[0].FertMask)
	}

	result := ActionResult{Patch: agg.patchOf(0)}
	patch := agg.PatchFromAction(result)
	if patch.Plot == nil {
		t.Fatal("patch plot is nil")
	}
	if patch.Plot.Health != 56 || patch.Plot.StolenCount != 4 || patch.Plot.FertMask != 0b101 {
		t.Fatalf("patch fields = health=%d stolen=%d fert=%03b",
			patch.Plot.Health, patch.Plot.StolenCount, patch.Plot.FertMask)
	}

	payload, err := json.Marshal(snap.Plots[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"health", "stolen_count", "fert_mask"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("json missing %q in %s", key, payload)
		}
	}
}

func TestAggregateJSONOmitsHazardSalt(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.HazardSalt = DeriveHazardSalt("must-not-persist")
	payload, err := json.Marshal(agg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), "must-not-persist") || strings.Contains(string(payload), "HazardSalt") || strings.Contains(string(payload), "hazard_salt") {
		t.Fatalf("HazardSalt leaked into JSON: %s", payload)
	}
}

func TestActionPatchClonesStealersSlice(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:    StateMature,
		CropID:   1,
		Stealers: []uint64{11, 12},
	}

	patch := agg.patchOf(0)
	if len(patch.Plot.Stealers) != 2 {
		t.Fatalf("patch stealers len = %d, want 2", len(patch.Plot.Stealers))
	}
	patch.Plot.Stealers[0] = 99
	patch.Plot.Stealers = append(patch.Plot.Stealers, 13)

	if agg.Plots[0].Stealers[0] != 11 {
		t.Fatalf("actor Stealers[0] = %d, want 11 (escaped)", agg.Plots[0].Stealers[0])
	}
	if len(agg.Plots[0].Stealers) != 2 {
		t.Fatalf("actor Stealers len = %d, want 2 after append on patch", len(agg.Plots[0].Stealers))
	}
}

func TestMultiSeasonHarvestSnapshotAndPatchContainSeasonIndex(t *testing.T) {
	agg := NewAggregate(1, "alice")
	agg.Plots[0] = Plot{
		State:          StateMature,
		CropID:         4, // 苹果（历史持久化 numeric ID）
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
