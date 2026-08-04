package presence

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRegistryRegistersLooksUpAndUnregistersConnection(t *testing.T) {
	backend := newMemoryBackend()
	registry := NewWithBackend(backend)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after Register: %v", err)
	}
	want := []ConnRef{{ConnID: 7, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup after Register = %#v, want %#v", got, want)
	}

	if err := registry.Unregister(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	got, err = registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after Unregister: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup after Unregister = %#v, want empty", got)
	}
}

func TestRegistryRejectsSecondLiveConnectionFromAnotherGateway(t *testing.T) {
	backend := newMemoryBackend()
	registry := NewWithBackend(backend)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 1, "gateway-0"); err != nil {
		t.Fatalf("Register gateway-0: %v", err)
	}
	if err := registry.Register(ctx, 42, 1, "gateway-1"); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("Register gateway-1 = %v, want ErrAlreadyConnected", err)
	}

	got, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := []ConnRef{{ConnID: 1, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %#v, want %#v", got, want)
	}

	if err := registry.Unregister(ctx, 42, 1, "gateway-0"); err != nil {
		t.Fatalf("Unregister gateway-0: %v", err)
	}
	got, err = registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after Unregister: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup after Unregister = %#v, want empty", got)
	}
	if err := registry.Register(ctx, 42, 1, "gateway-1"); err != nil {
		t.Fatalf("Register gateway-1 after release: %v", err)
	}
}

func TestRegistryReplaceConnectionEvictsPreviousOwner(t *testing.T) {
	backend := newMemoryBackend()
	registry := NewWithBackend(backend)
	ctx := context.Background()

	evicted, err := registry.ReplaceConnection(ctx, 42, 7, "gateway-0")
	if err != nil {
		t.Fatalf("first ReplaceConnection: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("first ReplaceConnection evicted = %#v, want empty", evicted)
	}

	evicted, err = registry.ReplaceConnection(ctx, 42, 8, "gateway-1")
	if err != nil {
		t.Fatalf("second ReplaceConnection: %v", err)
	}
	wantEvicted := []ConnRef{{ConnID: 7, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(evicted, wantEvicted) {
		t.Fatalf("second ReplaceConnection evicted = %#v, want %#v", evicted, wantEvicted)
	}
	got, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := []ConnRef{{ConnID: 8, GatewayID: "gateway-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %#v, want %#v", got, want)
	}

	evicted, err = registry.ReplaceConnection(ctx, 42, 8, "gateway-1")
	if err != nil {
		t.Fatalf("renew ReplaceConnection: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("renew ReplaceConnection evicted = %#v, want empty", evicted)
	}
}

func TestRegistryConcurrentClaimsAllowExactlyOneConnection(t *testing.T) {
	backend := newMemoryBackend()
	registry := NewWithBackend(backend)
	const contenders = 16

	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 1; i <= contenders; i++ {
		wg.Add(1)
		go func(connID uint64) {
			defer wg.Done()
			<-start
			results <- registry.Register(context.Background(), 42, connID, "gateway-0")
		}(uint64(i))
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyConnected):
			conflicts++
		default:
			t.Fatalf("unexpected Register error: %v", err)
		}
	}
	if successes != 1 || conflicts != contenders-1 {
		t.Fatalf("claims: successes=%d conflicts=%d, want 1/%d", successes, conflicts, contenders-1)
	}
	refs, err := registry.Lookup(context.Background(), 42)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Lookup = %#v, want exactly one owner", refs)
	}
}

func TestDefaultLeaseTTLExceedsWSReadTimeout(t *testing.T) {
	const wsReadTimeout = 90 * time.Second
	if DefaultLeaseTTL <= wsReadTimeout {
		t.Fatalf("DefaultLeaseTTL = %s, want > wsReadTimeout %s", DefaultLeaseTTL, wsReadTimeout)
	}
}

func TestLookupOmitsExpiredLifecycleLease(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	nowMs += int64(time.Minute / time.Millisecond)
	got, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after expiry: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup after expiry = %#v, want empty", got)
	}
}

func TestLookupSubscribersOmitsExpiredRoomLease(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Subscribe(ctx, 99, 8, "gateway-0"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got, err := registry.LookupSubscribers(ctx, 99)
	if err != nil {
		t.Fatalf("LookupSubscribers before expiry: %v", err)
	}
	want := []ConnRef{{ConnID: 8, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LookupSubscribers before expiry = %#v, want %#v", got, want)
	}

	nowMs += int64(time.Minute / time.Millisecond)
	got, err = registry.LookupSubscribers(ctx, 99)
	if err != nil {
		t.Fatalf("LookupSubscribers after expiry: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LookupSubscribers after expiry = %#v, want empty", got)
	}
}

func TestRegisterRenewsLeaseExpiry(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Advance almost to expiry, then renew.
	nowMs += int64(50 * time.Second / time.Millisecond)
	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register renew: %v", err)
	}
	// Without renew this would be expired; with renew the lease ends at now+60s.
	nowMs += int64(50 * time.Second / time.Millisecond)
	got, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after renew window: %v", err)
	}
	want := []ConnRef{{ConnID: 7, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup after renew = %#v, want %#v", got, want)
	}

	nowMs += int64(time.Minute / time.Millisecond)
	got, err = registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after final expiry: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup after final expiry = %#v, want empty", got)
	}
}

func TestSubscribeRenewsRoomLeaseExpiry(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Subscribe(ctx, 99, 8, "gateway-0"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nowMs += int64(50 * time.Second / time.Millisecond)
	if err := registry.Subscribe(ctx, 99, 8, "gateway-0"); err != nil {
		t.Fatalf("Subscribe renew: %v", err)
	}
	nowMs += int64(50 * time.Second / time.Millisecond)
	got, err := registry.LookupSubscribers(ctx, 99)
	if err != nil {
		t.Fatalf("LookupSubscribers after renew: %v", err)
	}
	want := []ConnRef{{ConnID: 8, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LookupSubscribers after renew = %#v, want %#v", got, want)
	}
}

func TestStaleLeaseDoesNotHitDifferentConnectionID(t *testing.T) {
	// Same gatewayID, old connID=1 left behind; a restarted process uses connID=9001.
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Hour),
	)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 1, "gateway-0"); err != nil {
		t.Fatalf("Register stale: %v", err)
	}
	if err := registry.Subscribe(ctx, 42, 1, "gateway-0"); err != nil {
		t.Fatalf("Subscribe stale: %v", err)
	}
	if err := registry.Register(ctx, 42, 9001, "gateway-0"); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("Register new before stale lease expiry = %v, want ErrAlreadyConnected", err)
	}

	lifecycle, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(lifecycle) != 1 || lifecycle[0].ConnID != 1 {
		t.Fatalf("Lookup = %#v, want only the live old lease", lifecycle)
	}

	nowMs += int64(time.Hour / time.Millisecond)
	if err := registry.Register(ctx, 42, 9001, "gateway-0"); err != nil {
		t.Fatalf("Register new after stale lease expiry: %v", err)
	}
	lifecycle, err = registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after replacement: %v", err)
	}
	if len(lifecycle) != 1 || lifecycle[0].ConnID != 9001 {
		t.Fatalf("Lookup after replacement = %#v, want only connID 9001", lifecycle)
	}

	rooms, err := registry.LookupSubscribers(ctx, 42)
	if err != nil {
		t.Fatalf("LookupSubscribers: %v", err)
	}
	for _, ref := range rooms {
		if ref.ConnID == 9001 {
			t.Fatalf("new process room lease should be absent until Subscribe: %#v", rooms)
		}
	}
}

func TestUpsertSetsKeyTTLSoKeyVanishesWithoutLookup(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !backend.keyExists(connectionKey(42), nowMs) {
		t.Fatal("lifecycle key missing immediately after Register")
	}

	// Fallback is 2*lease (+ margin): still present after one member lease.
	nowMs += int64(time.Minute / time.Millisecond)
	if !backend.keyExists(connectionKey(42), nowMs) {
		t.Fatal("lifecycle key vanished at 1*leaseTTL; fallback must outlive a single member")
	}

	// No Lookup: key must disappear once the 2*leaseTTL fallback elapses.
	nowMs += int64((time.Minute + keyTTLSafetyMargin) / time.Millisecond)
	if backend.keyExists(connectionKey(42), nowMs) {
		t.Fatal("lifecycle key still present after key fallback TTL with no Lookup")
	}
}

func TestKeyFallbackTTLExceedsSingleMemberLease(t *testing.T) {
	lease := time.Minute
	fallback := keyFallbackTTL(lease)
	if fallback <= lease {
		t.Fatalf("keyFallbackTTL(%s) = %s, want > single-member lease", lease, fallback)
	}
	if want := 2*lease + keyTTLSafetyMargin; fallback != want {
		t.Fatalf("keyFallbackTTL(%s) = %s, want %s", lease, fallback, want)
	}
}

func TestKeyFallbackTTLOverflowSafe(t *testing.T) {
	got := keyFallbackTTL(time.Duration(math.MaxInt64))
	if got <= 0 {
		t.Fatalf("keyFallbackTTL(MaxInt64) = %d, want positive clamped duration", got)
	}
	if got != time.Duration(math.MaxInt64) {
		t.Fatalf("keyFallbackTTL(MaxInt64) = %d, want MaxInt64 clamp", got)
	}
	if got := keyFallbackTTL(0); got != keyTTLSafetyMargin {
		t.Fatalf("keyFallbackTTL(0) = %s, want margin %s", got, keyTTLSafetyMargin)
	}
}

func TestUpsertRenewExtendsKeyTTL(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	nowMs += int64(50 * time.Second / time.Millisecond)
	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register renew: %v", err)
	}

	// Without renew, fallback would end at t0+2m+margin. Renew at t0+50s
	// pushes key expiry to t0+50s+2m+margin.
	nowMs += int64((time.Minute + 50*time.Second) / time.Millisecond)
	if !backend.keyExists(connectionKey(42), nowMs) {
		t.Fatal("lifecycle key vanished before renewed key fallback TTL")
	}
	nowMs += int64((time.Minute + keyTTLSafetyMargin) / time.Millisecond)
	if backend.keyExists(connectionKey(42), nowMs) {
		t.Fatal("lifecycle key still present after renewed key fallback TTL")
	}
}

func TestUpsertRemovesExpiredMembersButKeepsAlivePeers(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newMemoryBackend()
	registry := NewWithBackend(backend,
		WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		WithLeaseTTL(time.Minute),
	)
	ctx := context.Background()

	if err := registry.Subscribe(ctx, 42, 1, "gateway-old"); err != nil {
		t.Fatalf("Subscribe old: %v", err)
	}
	nowMs += int64(30 * time.Second / time.Millisecond)
	if err := registry.Subscribe(ctx, 42, 2, "gateway-alive"); err != nil {
		t.Fatalf("Subscribe alive: %v", err)
	}

	// Advance past gateway-old's member expiry but not gateway-alive's.
	nowMs += int64(40 * time.Second / time.Millisecond) // now = t0+70s; old expired at t0+60s
	if err := registry.Subscribe(ctx, 42, 3, "gateway-new"); err != nil {
		t.Fatalf("Subscribe new (triggers upsert cleanup): %v", err)
	}

	members := backend.members(roomKey(42), nowMs)
	if _, ok := members["gateway-old:1"]; ok {
		t.Fatalf("expired peer still present: %#v", members)
	}
	if _, ok := members["gateway-alive:2"]; !ok {
		t.Fatalf("still-valid peer removed: %#v", members)
	}
	if _, ok := members["gateway-new:3"]; !ok {
		t.Fatalf("new member missing: %#v", members)
	}
}

type memoryBackend struct {
	mu          sync.Mutex
	zsets       map[string]map[string]int64 // member -> expiresAtUnixMilli
	keyExpireAt map[string]int64            // key -> absolute expiry (unix milli)
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{
		zsets:       make(map[string]map[string]int64),
		keyExpireAt: make(map[string]int64),
	}
}

func (b *memoryBackend) Upsert(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireKeysLocked(nowUnixMilli)
	if b.zsets[key] == nil {
		b.zsets[key] = make(map[string]int64)
	}
	for existing, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], existing)
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	b.keyExpireAt[key] = nowUnixMilli + keyTTL.Milliseconds()
	return nil
}

func (b *memoryBackend) Claim(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireKeysLocked(nowUnixMilli)
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
	b.keyExpireAt[key] = nowUnixMilli + keyTTL.Milliseconds()
	return true, nil
}

func (b *memoryBackend) Replace(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireKeysLocked(nowUnixMilli)
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
	b.keyExpireAt[key] = nowUnixMilli + keyTTL.Milliseconds()
	return evicted, nil
}

func (b *memoryBackend) Delete(_ context.Context, key, member string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.zsets[key], member)
	if len(b.zsets[key]) == 0 {
		delete(b.zsets, key)
		delete(b.keyExpireAt, key)
	}
	return nil
}

func (b *memoryBackend) AliveMembers(_ context.Context, key string, nowUnixMilli int64) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireKeysLocked(nowUnixMilli)
	members := b.zsets[key]
	alive := make([]string, 0, len(members))
	for member, expiresAt := range members {
		if expiresAt <= nowUnixMilli {
			delete(members, member)
			continue
		}
		alive = append(alive, member)
	}
	if len(members) == 0 {
		delete(b.zsets, key)
		delete(b.keyExpireAt, key)
	}
	return alive, nil
}

func (b *memoryBackend) expireKeysLocked(nowUnixMilli int64) {
	for key, expireAt := range b.keyExpireAt {
		if expireAt <= nowUnixMilli {
			delete(b.zsets, key)
			delete(b.keyExpireAt, key)
		}
	}
}

func (b *memoryBackend) keyExists(key string, nowUnixMilli int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireKeysLocked(nowUnixMilli)
	_, ok := b.zsets[key]
	return ok
}

func (b *memoryBackend) members(key string, nowUnixMilli int64) map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireKeysLocked(nowUnixMilli)
	out := make(map[string]int64, len(b.zsets[key]))
	for member, expiresAt := range b.zsets[key] {
		out[member] = expiresAt
	}
	return out
}
