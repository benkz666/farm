package farmrpc

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	"farm/server/shared/clientwire"
	"farm/server/shared/store"
)

type registryBackend struct {
	mu         sync.Mutex
	zsets      map[string]map[string]int64
	batchCalls int
}

func newRegistryBackend() *registryBackend {
	return &registryBackend{zsets: make(map[string]map[string]int64)}
}

func (backend *registryBackend) Upsert(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.zsets[key] == nil {
		backend.zsets[key] = make(map[string]int64)
	}
	for existing, expiresAt := range backend.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(backend.zsets[key], existing)
		}
	}
	backend.zsets[key][member] = expiresAtUnixMilli
	return nil
}

func (backend *registryBackend) Claim(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) (bool, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.zsets[key] == nil {
		backend.zsets[key] = make(map[string]int64)
	}
	for existing, expiresAt := range backend.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(backend.zsets[key], existing)
		}
	}
	if len(backend.zsets[key]) > 0 {
		if _, renewing := backend.zsets[key][member]; !renewing || len(backend.zsets[key]) != 1 {
			return false, nil
		}
	}
	backend.zsets[key][member] = expiresAtUnixMilli
	return true, nil
}

func (backend *registryBackend) Replace(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) ([]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.zsets[key] == nil {
		backend.zsets[key] = make(map[string]int64)
	}
	evicted := make([]string, 0, len(backend.zsets[key]))
	for existing, expiresAt := range backend.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(backend.zsets[key], existing)
			continue
		}
		if existing != member {
			evicted = append(evicted, existing)
			delete(backend.zsets[key], existing)
		}
	}
	backend.zsets[key][member] = expiresAtUnixMilli
	return evicted, nil
}

func (backend *registryBackend) Delete(_ context.Context, key, member string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	delete(backend.zsets[key], member)
	return nil
}

func (backend *registryBackend) AliveMembers(_ context.Context, key string, nowUnixMilli int64) ([]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	alive := make([]string, 0, len(backend.zsets[key]))
	for member, expiresAt := range backend.zsets[key] {
		if expiresAt > nowUnixMilli {
			alive = append(alive, member)
		}
	}
	sort.Strings(alive)
	return alive, nil
}

func (backend *registryBackend) AliveMembersBatch(_ context.Context, keys []string, nowUnixMilli int64) (map[string][]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.batchCalls++
	result := make(map[string][]string, len(keys))
	for _, key := range keys {
		for member, expiresAt := range backend.zsets[key] {
			if expiresAt > nowUnixMilli {
				result[key] = append(result[key], member)
			}
		}
		sort.Strings(result[key])
	}
	return result, nil
}

func (backend *registryBackend) batchCallCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.batchCalls
}

type pushedBatch struct {
	gatewayID string
	batch     PushBatch
}

type recordingBatchPusher struct {
	mu    sync.Mutex
	items []pushedBatch
}

func (pusher *recordingBatchPusher) PushBatch(_ context.Context, gatewayID string, batch PushBatch) error {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	pusher.items = append(pusher.items, pushedBatch{gatewayID: gatewayID, batch: batch})
	return nil
}

func (pusher *recordingBatchPusher) batches() []pushedBatch {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	return append([]pushedBatch(nil), pusher.items...)
}

type gatedBatchPusher struct {
	slowGateway string
	onFast      func()
	holdSlow    chan struct{}
}

func (pusher *gatedBatchPusher) PushBatch(_ context.Context, gatewayID string, _ PushBatch) error {
	if gatewayID == pusher.slowGateway {
		<-pusher.holdSlow
		return nil
	}
	if pusher.onFast != nil {
		pusher.onFast()
	}
	return nil
}

type failingBatchPusher struct {
	failGateway string
	mu          sync.Mutex
	attempted   map[string]struct{}
}

func (pusher *failingBatchPusher) PushBatch(_ context.Context, gatewayID string, _ PushBatch) error {
	pusher.mu.Lock()
	if pusher.attempted == nil {
		pusher.attempted = make(map[string]struct{})
	}
	pusher.attempted[gatewayID] = struct{}{}
	pusher.mu.Unlock()
	if gatewayID == pusher.failGateway {
		return context.DeadlineExceeded
	}
	return nil
}

func (pusher *failingBatchPusher) saw(gatewayID string) bool {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	_, ok := pusher.attempted[gatewayID]
	return ok
}

type pushedTaskNotify struct {
	ref  presence.ConnRef
	uid  uint64
	task store.Task
}

type recordingTaskNotifyPusher struct {
	mu        sync.Mutex
	items     []pushedTaskNotify
	published chan struct{}
}

func (pusher *recordingTaskNotifyPusher) PushTaskNotify(_ context.Context, ref presence.ConnRef, uid uint64, task store.Task) error {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	pusher.items = append(pusher.items, pushedTaskNotify{ref: ref, uid: uid, task: task})
	if pusher.published != nil {
		select {
		case pusher.published <- struct{}{}:
		default:
		}
	}
	return nil
}

func (pusher *recordingTaskNotifyPusher) notifications() []pushedTaskNotify {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	return append([]pushedTaskNotify(nil), pusher.items...)
}

type pushedMailNotify struct {
	ref  presence.ConnRef
	uid  uint64
	kind string
}

type recordingMailNotifyPusher struct {
	mu    sync.Mutex
	items []pushedMailNotify
}

func (pusher *recordingMailNotifyPusher) PushMailNotify(_ context.Context, ref presence.ConnRef, uid uint64, kind string) error {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	pusher.items = append(pusher.items, pushedMailNotify{ref: ref, uid: uid, kind: kind})
	return nil
}

func (pusher *recordingMailNotifyPusher) notifications() []pushedMailNotify {
	pusher.mu.Lock()
	defer pusher.mu.Unlock()
	return append([]pushedMailNotify(nil), pusher.items...)
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

func equalUint64(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func assertFarmDeltaEnvelope(t *testing.T, envelope []byte, delta farm.FarmDelta) {
	t.Helper()
	decoded, err := clientwire.DecodeFarmDelta(envelope)
	if err != nil {
		t.Fatalf("DecodeFarmDelta: %v", err)
	}
	if decoded.OwnerUID != delta.OwnerUID || decoded.FarmSeq != delta.FarmSeq {
		t.Fatalf("decoded = %#v, want %#v", decoded, delta)
	}
}

type countingFarmDeltaEncoder struct {
	calls atomic.Int64
}

func (encoder *countingFarmDeltaEncoder) EncodeFarmDelta(delta farm.FarmDelta) ([]byte, error) {
	encoder.calls.Add(1)
	return clientwire.EncodeFarmDelta(delta)
}
