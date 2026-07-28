package gateway

import (
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"farm/server/internal/farm"
	"farm/server/internal/obs"
	"farm/server/internal/wireenv"
)

func TestRoomHubBroadcastsOnlySubscribedRoom(t *testing.T) {
	hub := NewRoomHub()
	var ownerFirst, ownerSecond, otherRoom []farm.FarmDelta

	hub.Subscribe(11, 1, func(delta farm.FarmDelta, _ []byte) {
		ownerFirst = append(ownerFirst, delta)
	})
	hub.Subscribe(11, 2, func(delta farm.FarmDelta, _ []byte) {
		ownerSecond = append(ownerSecond, delta)
	})
	hub.Subscribe(12, 3, func(delta farm.FarmDelta, _ []byte) {
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
	hub.Subscribe(11, 1, func(farm.FarmDelta, []byte) {
		original++
	})
	hub.Subscribe(11, 1, func(farm.FarmDelta, []byte) {
		replacement++
	})

	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 1})

	if original != 0 || replacement != 1 {
		t.Fatalf("deliveries = original:%d replacement:%d, want 0:1", original, replacement)
	}
}

func TestRoomHubRevokesAllViewerSubscriptions(t *testing.T) {
	hub := NewRoomHub()
	var revoked, retained int
	hub.SubscribeViewer(11, 1, 7, func(farm.FarmDelta, []byte) {
		revoked++
	})
	hub.SubscribeViewer(11, 2, 7, func(farm.FarmDelta, []byte) {
		revoked++
	})
	hub.SubscribeViewer(11, 3, 8, func(farm.FarmDelta, []byte) {
		retained++
	})

	hub.RevokeViewer(11, 7)
	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 1})

	if revoked != 0 || retained != 1 {
		t.Fatalf("deliveries = revoked:%d retained:%d, want 0:1", revoked, retained)
	}
}

func TestRoomHubDeltaMetricsOneLocalBatch(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	hub := NewRoomHub()
	hub.metrics = metrics

	var encodeCalls atomic.Int64
	hub.encodeFarmDelta = func(delta farm.FarmDelta) ([]byte, error) {
		encodeCalls.Add(1)
		return wireenv.EncodeFarmDelta(delta)
	}
	for _, connID := range []uint64{1, 2, 3} {
		hub.Subscribe(11, connID, func(farm.FarmDelta, []byte) {})
	}

	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 1})

	if got := encodeCalls.Load(); got != 1 {
		t.Fatalf("encode calls = %d, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.DeltaBatches); got != 1 {
		t.Fatalf("batches = %v, want 1 (local RoomHub = one batch)", got)
	}
}

func TestRoomHubDeltaMetricsSkipsEmptyTargets(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	hub := NewRoomHub()
	hub.metrics = metrics
	hub.encodeFarmDelta = func(delta farm.FarmDelta) ([]byte, error) {
		return wireenv.EncodeFarmDelta(delta)
	}

	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 1})
	if got := testutil.ToFloat64(metrics.DeltaBatches); got != 0 {
		t.Fatalf("empty room batches = %v, want 0", got)
	}
}
