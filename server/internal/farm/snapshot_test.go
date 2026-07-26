package farm

import "testing"

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
