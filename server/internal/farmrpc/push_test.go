package farmrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
)

func TestFanoutPublisherPushesEverySubscribedConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 8, "gateway-1"); err != nil {
		t.Fatalf("Subscribe first connection: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 9, "gateway-0"); err != nil {
		t.Fatalf("Subscribe second connection: %v", err)
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}

	if err := publisher.Publish(t.Context(), delta, connreg.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := connIDsByGateway(pusher.batches())
	want := map[string][]uint64{
		"gateway-1": {8},
		"gateway-0": {9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches by gateway = %#v, want %#v", got, want)
	}
}

func TestFanoutPublisherSkipsOriginatingConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-1"); err != nil {
		t.Fatalf("Subscribe first connection: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-0"); err != nil {
		t.Fatalf("Subscribe originator connection: %v", err)
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}

	if err := publisher.Publish(t.Context(), delta, connreg.ConnRef{ConnID: 1, GatewayID: "gateway-0"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := connIDsByGateway(pusher.batches())
	want := map[string][]uint64{"gateway-1": {1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches by gateway = %#v, want %#v", got, want)
	}
}

func TestFanoutPublisherSkipsExpiredRoomSubscribers(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend,
		connreg.WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		connreg.WithLeaseTTL(time.Minute),
	)
	if err := registry.Subscribe(t.Context(), 42, 8, "gateway-1"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nowMs += int64(time.Minute / time.Millisecond)

	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	if err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}, connreg.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := connIDsByGateway(pusher.batches()); len(got) != 0 {
		t.Fatalf("expired subscriber still fanned out: %#v", got)
	}
}

func TestHTTPDeltaPusherSendsAuthenticatedPushRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deltaPushBatchPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, deltaPushBatchPath)
		}
		if r.Header.Get("Authorization") != "Bearer internal-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	pusher := NewHTTPDeltaPusher(map[string]string{"gateway-0": server.URL}, "internal-token")

	if err := pusher.PushBatch(t.Context(), "gateway-0", PushBatch{
		ConnIDs:  []uint64{7},
		Envelope: []byte(`{"cmd":9000,"client_seq":0,"err":0,"payload":{"owner_uid":42}}`),
	}); err != nil {
		t.Fatalf("PushBatch: %v", err)
	}
}

func connIDsByGateway(items []pushedBatch) map[string][]uint64 {
	out := make(map[string][]uint64, len(items))
	for _, item := range items {
		ids := append([]uint64(nil), item.batch.ConnIDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		out[item.gatewayID] = ids
	}
	return out
}

type registryBackend struct {
	mu    sync.Mutex
	zsets map[string]map[string]int64
}

func newRegistryBackend() *registryBackend {
	return &registryBackend{zsets: make(map[string]map[string]int64)}
}

func (b *registryBackend) Upsert(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zsets[key] == nil {
		b.zsets[key] = make(map[string]int64)
	}
	for existing, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], existing)
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	return nil
}

func (*registryBackend) Delete(context.Context, string, string) error { return nil }

func (b *registryBackend) AliveMembers(_ context.Context, key string, nowUnixMilli int64) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	alive := make([]string, 0, len(b.zsets[key]))
	for member, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], member)
			continue
		}
		alive = append(alive, member)
	}
	return alive, nil
}
