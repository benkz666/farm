package gateway

import (
	"testing"

	"farm/server/internal/farm"
)

func TestRoomHubBroadcastsOnlySubscribedRoom(t *testing.T) {
	hub := NewRoomHub()
	var ownerFirst, ownerSecond, otherRoom []farm.FarmDelta

	hub.Subscribe(11, 1, func(delta farm.FarmDelta) {
		ownerFirst = append(ownerFirst, delta)
	})
	hub.Subscribe(11, 2, func(delta farm.FarmDelta) {
		ownerSecond = append(ownerSecond, delta)
	})
	hub.Subscribe(12, 3, func(delta farm.FarmDelta) {
		otherRoom = append(otherRoom, delta)
	})

	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 1})

	if len(ownerFirst) != 1 || len(ownerSecond) != 1 {
		t.Fatalf("owner-room deliveries = %d, %d, want 1, 1", len(ownerFirst), len(ownerSecond))
	}
	if len(otherRoom) != 0 {
		t.Fatalf("other-room deliveries = %d, want 0", len(otherRoom))
	}

	hub.Unsubscribe(11, 1)
	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 2})

	if len(ownerFirst) != 1 {
		t.Fatalf("unsubscribed delivery count = %d, want 1", len(ownerFirst))
	}
	if len(ownerSecond) != 2 {
		t.Fatalf("remaining subscriber delivery count = %d, want 2", len(ownerSecond))
	}
}

func TestRoomHubReplacesExistingConnectionSubscription(t *testing.T) {
	hub := NewRoomHub()
	var original, replacement int
	hub.Subscribe(11, 1, func(farm.FarmDelta) {
		original++
	})
	hub.Subscribe(11, 1, func(farm.FarmDelta) {
		replacement++
	})

	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 1})

	if original != 0 || replacement != 1 {
		t.Fatalf("deliveries = original:%d replacement:%d, want 0:1", original, replacement)
	}
}
