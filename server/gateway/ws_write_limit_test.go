package gateway

import "testing"

func TestWriteInFlightLimitBoundsAndReleasesSlots(t *testing.T) {
	gateway := New(nil, nil, nil, WithWriteInFlightLimit(1))
	if !gateway.acquireWriteSlot() {
		t.Fatal("first write slot was rejected")
	}
	if gateway.acquireWriteSlot() {
		t.Fatal("write limit admitted excess request")
	}
	gateway.releaseWriteSlot()
	if !gateway.acquireWriteSlot() {
		t.Fatal("released write slot was not reusable")
	}
}

func TestWriteInFlightLimitCanBeDisabled(t *testing.T) {
	gateway := New(nil, nil, nil, WithWriteInFlightLimit(0))
	for range 10_000 {
		if !gateway.acquireWriteSlot() {
			t.Fatal("disabled write guard rejected request")
		}
	}
}
