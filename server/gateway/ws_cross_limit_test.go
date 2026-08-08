package gateway

import "testing"

func TestCrossInFlightLimitBoundsAndReleasesSlots(t *testing.T) {
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{}, WithCrossInFlightLimit(1))
	if !gateway.acquireCrossSlot() {
		t.Fatal("first cross slot was rejected")
	}
	if gateway.acquireCrossSlot() {
		t.Fatal("cross limit admitted a second request")
	}
	gateway.releaseCrossSlot()
	if !gateway.acquireCrossSlot() {
		t.Fatal("released cross slot was not reusable")
	}
}

func TestCrossInFlightLimitCanBeDisabled(t *testing.T) {
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{}, WithCrossInFlightLimit(0))
	for range 4096 {
		if !gateway.acquireCrossSlot() {
			t.Fatal("disabled cross limit rejected a request")
		}
	}
}
