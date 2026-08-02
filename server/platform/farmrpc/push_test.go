package farmrpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"farm/server/platform/connreg"
	"farm/server/platform/farm"
	"farm/server/platform/store"
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

func TestTaskFanoutPublisherPushesActivePlayerConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	ref := connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingTaskNotifyPusher{published: make(chan struct{}, 1)}
	publisher := newTaskFanoutPublisher(registry, pusher, 5*time.Millisecond)
	task := store.Task{
		ID: 1, DayKey: 20260731, Title: "完成一次播种", Progress: 1, Target: 1, RewardCoin: 20,
	}

	if err := publisher.PublishTaskNotify(t.Context(), 42, task); err != nil {
		t.Fatalf("PublishTaskNotify: %v", err)
	}
	select {
	case <-pusher.published:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced TaskNotify")
	}

	if got := pusher.notifications(); !reflect.DeepEqual(got, []pushedTaskNotify{
		{ref: connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}, uid: 42, task: task},
	}) {
		t.Fatalf("task notifications = %#v", got)
	}
}

func TestTaskFanoutPublisherCoalescesLatestStateForSameTask(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	ref := connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingTaskNotifyPusher{published: make(chan struct{}, 2)}
	publisher := newTaskFanoutPublisher(registry, pusher, 10*time.Millisecond)
	first := store.Task{ID: 5, DayKey: 20260731, Progress: 1, Target: 10}
	latest := store.Task{ID: 5, DayKey: 20260731, Progress: 3, Target: 10}

	if err := publisher.PublishTaskNotify(t.Context(), 42, first); err != nil {
		t.Fatalf("first PublishTaskNotify: %v", err)
	}
	if err := publisher.PublishTaskNotify(t.Context(), 42, latest); err != nil {
		t.Fatalf("latest PublishTaskNotify: %v", err)
	}
	select {
	case <-pusher.published:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced TaskNotify")
	}
	if got := pusher.notifications(); !reflect.DeepEqual(got, []pushedTaskNotify{
		{ref: ref, uid: 42, task: latest},
	}) {
		t.Fatalf("task notifications = %#v, want latest only", got)
	}
	select {
	case <-pusher.published:
		t.Fatal("received duplicate TaskNotify after coalescing")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestTaskFanoutPublisherDoesNotCoalesceAcrossLocalDays(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	ref := connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingTaskNotifyPusher{published: make(chan struct{}, 2)}
	publisher := newTaskFanoutPublisher(registry, pusher, 5*time.Millisecond)
	oldDay := store.Task{ID: 5, DayKey: 20260731, Progress: 9, Target: 10}
	newDay := store.Task{ID: 5, DayKey: 20260801, Progress: 1, Target: 10}

	if err := publisher.PublishTaskNotify(t.Context(), 42, oldDay); err != nil {
		t.Fatalf("old-day PublishTaskNotify: %v", err)
	}
	if err := publisher.PublishTaskNotify(t.Context(), 42, newDay); err != nil {
		t.Fatalf("new-day PublishTaskNotify: %v", err)
	}
	for range 2 {
		select {
		case <-pusher.published:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for cross-day TaskNotify")
		}
	}
	got := pusher.notifications()
	sort.Slice(got, func(i, j int) bool { return got[i].task.DayKey < got[j].task.DayKey })
	if !reflect.DeepEqual(got, []pushedTaskNotify{
		{ref: ref, uid: 42, task: oldDay},
		{ref: ref, uid: 42, task: newDay},
	}) {
		t.Fatalf("cross-day notifications = %#v", got)
	}
}

func TestTaskFanoutPublisherDropsOverflowWithoutDirectDelivery(t *testing.T) {
	registry := connreg.NewWithBackend(newRegistryBackend())
	pusher := &recordingTaskNotifyPusher{}
	publisher := newTaskFanoutPublisher(registry, pusher, time.Hour)
	for index := 0; index < maxPendingTaskNotifies; index++ {
		key := taskNotifyKey{uid: uint64(index + 1), dayKey: 20260731, taskID: store.TaskWaterID}
		publisher.pending[key] = pendingTaskNotify{
			uid:  key.uid,
			task: store.Task{ID: key.taskID, DayKey: key.dayKey},
		}
	}

	if err := publisher.PublishTaskNotify(t.Context(), 999999, store.Task{
		ID: store.TaskPlantID, DayKey: 20260731,
	}); err != nil {
		t.Fatalf("overflow PublishTaskNotify: %v", err)
	}
	if got := publisher.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	if got := pusher.notifications(); len(got) != 0 {
		t.Fatalf("overflow performed direct delivery: %#v", got)
	}
}

func TestMailFanoutPublisherPushesActivePlayerConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	ref := connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingMailNotifyPusher{}
	publisher := NewMailFanoutPublisher(registry, pusher)

	if err := publisher.PublishMailNotify(t.Context(), 42, "friend_request"); err != nil {
		t.Fatalf("PublishMailNotify: %v", err)
	}

	if got := pusher.notifications(); !reflect.DeepEqual(got, []pushedMailNotify{
		{ref: connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}, uid: 42, kind: "friend_request"},
	}) {
		t.Fatalf("mail notifications = %#v", got)
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

func TestHTTPTaskNotifyPusherSendsAuthenticatedPushRequest(t *testing.T) {
	var got TaskNotifyPushRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != taskNotifyPushPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, taskNotifyPushPath)
		}
		if r.Header.Get("Authorization") != "Bearer internal-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode TaskNotify push: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	task := store.Task{ID: 3, DayKey: 20260731, Title: "拜访一次好友农场", Progress: 1, Target: 1, RewardCoin: 40}
	pusher := NewHTTPTaskNotifyPusher(map[string]string{"gateway-0": server.URL}, "internal-token")

	if err := pusher.PushTaskNotify(t.Context(), connreg.ConnRef{ConnID: 7, GatewayID: "gateway-0"}, 42, task); err != nil {
		t.Fatalf("PushTaskNotify: %v", err)
	}
	if got.ConnectionID != 7 || got.UID != 42 || got.Task != task {
		t.Fatalf("TaskNotify push = %#v", got)
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

type pushedTaskNotify struct {
	ref  connreg.ConnRef
	uid  uint64
	task store.Task
}

type pushedMailNotify struct {
	ref  connreg.ConnRef
	uid  uint64
	kind string
}

type recordingTaskNotifyPusher struct {
	mu        sync.Mutex
	items     []pushedTaskNotify
	published chan struct{}
}

func (p *recordingTaskNotifyPusher) PushTaskNotify(_ context.Context, ref connreg.ConnRef, uid uint64, task store.Task) error {
	p.mu.Lock()
	p.items = append(p.items, pushedTaskNotify{ref: ref, uid: uid, task: task})
	p.mu.Unlock()
	if p.published != nil {
		select {
		case p.published <- struct{}{}:
		default:
		}
	}
	return nil
}

func (p *recordingTaskNotifyPusher) notifications() []pushedTaskNotify {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]pushedTaskNotify(nil), p.items...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].ref.GatewayID < out[j].ref.GatewayID
	})
	return out
}

type recordingMailNotifyPusher struct {
	mu    sync.Mutex
	items []pushedMailNotify
}

func (p *recordingMailNotifyPusher) PushMailNotify(_ context.Context, ref connreg.ConnRef, uid uint64, kind string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, pushedMailNotify{ref: ref, uid: uid, kind: kind})
	return nil
}

func (p *recordingMailNotifyPusher) notifications() []pushedMailNotify {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]pushedMailNotify(nil), p.items...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].ref.GatewayID < out[j].ref.GatewayID
	})
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

func (b *registryBackend) Claim(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) (bool, error) {
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
	if len(b.zsets[key]) > 0 {
		if _, renewing := b.zsets[key][member]; !renewing || len(b.zsets[key]) != 1 {
			return false, nil
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	return true, nil
}

func (b *registryBackend) Replace(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zsets[key] == nil {
		b.zsets[key] = make(map[string]int64)
	}
	evicted := make([]string, 0, len(b.zsets[key]))
	for existing, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], existing)
			continue
		}
		if existing != member {
			evicted = append(evicted, existing)
			delete(b.zsets[key], existing)
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	return evicted, nil
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
