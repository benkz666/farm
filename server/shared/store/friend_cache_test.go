package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type friendCacheStoreStub struct {
	mu           sync.Mutex
	relation     bool
	relationErr  error
	relationHits int
	list         []FriendRow
	listHits     int
	loadStarted  chan struct{}
	loadRelease  chan struct{}
}

func (stub *friendCacheStoreStub) AreFriends(context.Context, uint64, uint64) (bool, error) {
	stub.mu.Lock()
	stub.relationHits++
	started, release := stub.loadStarted, stub.loadRelease
	value, err := stub.relation, stub.relationErr
	stub.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return value, err
}

func (stub *friendCacheStoreStub) AddFriends(context.Context, uint64, uint64) error { return nil }
func (stub *friendCacheStoreStub) RemoveFriends(context.Context, uint64, uint64) error {
	return nil
}
func (stub *friendCacheStoreStub) ListFriends(context.Context, uint64) ([]FriendRow, error) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.listHits++
	return cloneFriendRows(stub.list), nil
}
func (stub *friendCacheStoreStub) CountFriends(context.Context, uint64) (int, error) {
	return 0, nil
}
func (stub *friendCacheStoreStub) FindUserByUsername(context.Context, string) (UserSearchRow, error) {
	return UserSearchRow{}, nil
}
func (stub *friendCacheStoreStub) CreateFriendRequest(context.Context, uint64, uint64) error {
	return nil
}
func (stub *friendCacheStoreStub) ListIncomingFriendRequests(context.Context, uint64) ([]FriendRequestRow, error) {
	return nil, nil
}
func (stub *friendCacheStoreStub) AcceptFriendRequest(context.Context, uint64, uint64) error {
	return nil
}
func (stub *friendCacheStoreStub) RejectFriendRequest(context.Context, uint64, uint64) error {
	return nil
}

type friendCacheRedisStub struct {
	mu           sync.Mutex
	values       map[string]string
	gets         int
	sets         int
	deletes      int
	getErr       error
	setErr       error
	delErr       error
	blockNextSet bool
	setStarted   chan struct{}
	setRelease   chan struct{}
}

func newFriendCacheRedisStub() *friendCacheRedisStub {
	return &friendCacheRedisStub{values: make(map[string]string)}
}

func (stub *friendCacheRedisStub) Get(_ context.Context, key string) *redis.StringCmd {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.gets++
	if stub.getErr != nil {
		return redis.NewStringResult("", stub.getErr)
	}
	value, ok := stub.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (stub *friendCacheRedisStub) Set(_ context.Context, key string, value any, _ time.Duration) *redis.StatusCmd {
	stub.mu.Lock()
	stub.sets++
	if stub.setErr != nil {
		err := stub.setErr
		stub.mu.Unlock()
		return redis.NewStatusResult("", err)
	}
	block, started, release := stub.blockNextSet, stub.setStarted, stub.setRelease
	stub.blockNextSet = false
	stub.mu.Unlock()
	if block {
		if started != nil {
			close(started)
		}
		if release != nil {
			<-release
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	switch typed := value.(type) {
	case string:
		stub.values[key] = typed
	case []byte:
		stub.values[key] = string(typed)
	}
	return redis.NewStatusResult("OK", nil)
}

func (stub *friendCacheRedisStub) Del(_ context.Context, keys ...string) *redis.IntCmd {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.deletes++
	if stub.delErr != nil {
		return redis.NewIntResult(0, stub.delErr)
	}
	var removed int64
	for _, key := range keys {
		if _, ok := stub.values[key]; ok {
			delete(stub.values, key)
			removed++
		}
	}
	return redis.NewIntResult(removed, nil)
}

func TestCachedFriendStoreCachesPositiveAndNegativeRelationsLocally(t *testing.T) {
	for _, value := range []bool{false, true} {
		t.Run(map[bool]string{false: "negative", true: "positive"}[value], func(t *testing.T) {
			inner := &friendCacheStoreStub{relation: value}
			redisStub := newFriendCacheRedisStub()
			cache := newCachedFriendStore(inner, redisStub)
			for range 2 {
				got, err := cache.AreFriends(t.Context(), 11, 22)
				if err != nil || got != value {
					t.Fatalf("AreFriends = %v, %v; want %v, nil", got, err, value)
				}
			}
			waitForFriendCacheWrites(t, redisStub, 1)
			if inner.relationHits != 1 || redisStub.gets != 1 || redisStub.sets != 1 {
				t.Fatalf("inner=%d redis gets=%d sets=%d, want 1/1/1", inner.relationHits, redisStub.gets, redisStub.sets)
			}
		})
	}
}

func waitForFriendCacheWrites(t *testing.T, redisStub *friendCacheRedisStub, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		redisStub.mu.Lock()
		sets := redisStub.sets
		redisStub.mu.Unlock()
		if sets >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Redis SET count did not reach %d", want)
}

func TestCachedFriendStoreUsesRedisBeforeAuthoritativeStore(t *testing.T) {
	inner := &friendCacheStoreStub{relationErr: errors.New("must not be called")}
	redisStub := newFriendCacheRedisStub()
	key := relationKey(31, 17)
	redisStub.values[relationRedisKey(key)] = "1"
	cache := newCachedFriendStore(inner, redisStub)

	for range 2 {
		got, err := cache.AreFriends(t.Context(), 31, 17)
		if err != nil || !got {
			t.Fatalf("AreFriends = %v, %v; want true, nil", got, err)
		}
	}
	if inner.relationHits != 0 || redisStub.gets != 1 {
		t.Fatalf("inner=%d redis gets=%d, want 0/1", inner.relationHits, redisStub.gets)
	}
}

func TestCachedFriendStoreTemporarilyBypassesRedisDuringUniqueMissStorm(t *testing.T) {
	inner := &friendCacheStoreStub{}
	redisStub := newFriendCacheRedisStub()
	cache := newCachedFriendStore(inner, redisStub)

	for uid := uint64(1); uid <= 1_000; uid++ {
		if _, err := cache.AreFriends(t.Context(), uid, uid+10_000); err != nil {
			t.Fatal(err)
		}
	}
	redisStub.mu.Lock()
	gets := redisStub.gets
	redisStub.mu.Unlock()
	if gets < friendRedisProbeWindow || gets > friendRedisProbeWindow+8 {
		t.Fatalf("Redis GETs=%d, want one %d-request probe window before bypass", gets, friendRedisProbeWindow)
	}
	if inner.relationHits != 1_000 {
		t.Fatalf("authoritative hits=%d, want 1000", inner.relationHits)
	}
}

func TestCachedFriendStoreTemporarilyBypassesRedisDuringUniqueListMissStorm(t *testing.T) {
	inner := &friendCacheStoreStub{}
	redisStub := newFriendCacheRedisStub()
	cache := newCachedFriendStore(inner, redisStub)

	for uid := uint64(1); uid <= 1_000; uid++ {
		if _, err := cache.ListFriends(t.Context(), uid); err != nil {
			t.Fatal(err)
		}
	}
	redisStub.mu.Lock()
	gets := redisStub.gets
	redisStub.mu.Unlock()
	if gets < friendRedisProbeWindow || gets > friendRedisProbeWindow+8 {
		t.Fatalf("Redis GETs=%d, want one %d-request probe window before bypass", gets, friendRedisProbeWindow)
	}
	if inner.listHits != 1_000 {
		t.Fatalf("authoritative hits=%d, want 1000", inner.listHits)
	}
}

func TestCachedFriendStoreListCacheCoversUniformFixtureWithoutEviction(t *testing.T) {
	cache := newCachedFriendStore(&friendCacheStoreStub{}, nil)
	now := time.Now()
	const accounts = 15_000
	for uid := uint64(1); uid <= accounts; uid++ {
		cache.putLocalList(uid, []FriendRow{{UID: uid + accounts, Nickname: "friend"}}, now)
	}

	for uid := uint64(1); uid <= accounts; uid++ {
		value, ok := cache.localList(uid, now.UnixNano())
		if !ok || len(value) != 1 || value[0].UID != uid+accounts {
			t.Fatalf("uid=%d cache value=%#v hit=%v", uid, value, ok)
		}
	}

	total := 0
	for i := range cache.listShards {
		shard := &cache.listShards[i]
		shard.mu.RLock()
		total += len(shard.entries)
		if len(shard.entries) > friendListCacheCapacity/friendListCacheShards {
			t.Fatalf("shard %d contains %d entries", i, len(shard.entries))
		}
		shard.mu.RUnlock()
	}
	if total != accounts {
		t.Fatalf("cached entries=%d, want %d", total, accounts)
	}
}

func TestCachedFriendStoreCoalescesConcurrentRelationMisses(t *testing.T) {
	inner := &friendCacheStoreStub{
		relation:    true,
		loadStarted: make(chan struct{}, 1),
		loadRelease: make(chan struct{}),
	}
	cache := newCachedFriendStore(inner, nil)
	var succeeded atomic.Int64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, err := cache.AreFriends(t.Context(), 7, 8)
			if err == nil && value {
				succeeded.Add(1)
			}
		}()
	}
	select {
	case <-inner.loadStarted:
	case <-time.After(time.Second):
		t.Fatal("authoritative load did not start")
	}
	close(inner.loadRelease)
	wait.Wait()
	if succeeded.Load() != 32 || inner.relationHits != 1 {
		t.Fatalf("succeeded=%d inner=%d, want 32/1", succeeded.Load(), inner.relationHits)
	}
}

func TestCachedFriendStoreMutationInvalidatesRelationAndBothLists(t *testing.T) {
	inner := &friendCacheStoreStub{
		relation: true,
		list:     []FriendRow{{UID: 2, Nickname: "two"}},
	}
	redisStub := newFriendCacheRedisStub()
	cache := newCachedFriendStore(inner, redisStub)
	if _, err := cache.AreFriends(t.Context(), 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ListFriends(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ListFriends(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if err := cache.RemoveFriends(t.Context(), 1, 2); err != nil {
		t.Fatal(err)
	}

	inner.mu.Lock()
	inner.relation = false
	inner.list = nil
	inner.mu.Unlock()
	value, err := cache.AreFriends(t.Context(), 1, 2)
	if err != nil || value {
		t.Fatalf("AreFriends after mutation = %v, %v; want false, nil", value, err)
	}
	list, err := cache.ListFriends(t.Context(), 1)
	if err != nil || len(list) != 0 {
		t.Fatalf("ListFriends after mutation = %#v, %v; want empty", list, err)
	}
	if inner.relationHits != 1 || inner.listHits != 3 {
		t.Fatalf("inner relation/list hits=%d/%d, want 1/3", inner.relationHits, inner.listHits)
	}
}

func TestCachedFriendStoreMutationWritesExactNegativeRelation(t *testing.T) {
	inner := &friendCacheStoreStub{}
	redisStub := newFriendCacheRedisStub()
	key := relationKey(10, 20)
	redisStub.values[relationRedisKey(key)] = "1"
	cache := newCachedFriendStore(inner, redisStub)
	cache.putLocalRelation(key, true, time.Now())

	if err := cache.RemoveFriends(t.Context(), 10, 20); err != nil {
		t.Fatal(err)
	}
	redisStub.mu.Lock()
	value := redisStub.values[relationRedisKey(key)]
	redisStub.mu.Unlock()
	if value != "0" {
		t.Fatalf("shared relation cache = %q, want exact negative value", value)
	}
	if got, err := cache.AreFriends(t.Context(), 10, 20); err != nil || got {
		t.Fatalf("AreFriends after removal = %v, %v; want false, nil", got, err)
	}
	if inner.relationHits != 0 {
		t.Fatalf("authoritative hits=%d, want mutation-populated local hit", inner.relationHits)
	}
}

func TestCachedFriendStoreBypassesStaleRedisAfterInvalidationFailure(t *testing.T) {
	inner := &friendCacheStoreStub{
		list: []FriendRow{{UID: 30, Nickname: "fresh"}},
	}
	redisStub := newFriendCacheRedisStub()
	key := relationKey(10, 20)
	redisStub.values[relationRedisKey(key)] = "1"
	redisStub.values[friendListRedisKey(10)] = `[{"UID":99,"Nickname":"stale"}]`
	redisStub.setErr = errors.New("redis set failed")
	redisStub.delErr = errors.New("redis del failed")
	cache := newCachedFriendStore(inner, redisStub)
	cache.putLocalRelation(key, true, time.Now())

	if err := cache.RemoveFriends(t.Context(), 10, 20); err != nil {
		t.Fatal(err)
	}
	cache.deleteLocalRelation(key)
	value, err := cache.AreFriends(t.Context(), 10, 20)
	if err != nil || value {
		t.Fatalf("AreFriends = %v, %v; want authoritative false after Redis failure", value, err)
	}
	list, err := cache.ListFriends(t.Context(), 10)
	if err != nil || len(list) != 1 || list[0].UID != 30 {
		t.Fatalf("ListFriends = %#v, %v; want authoritative fresh list", list, err)
	}
	redisStub.mu.Lock()
	gets := redisStub.gets
	redisStub.mu.Unlock()
	if gets != 0 {
		t.Fatalf("Redis GETs=%d, want unsafe keys to bypass stale Redis", gets)
	}
}

func TestCachedFriendStoreSerializesReadBackfillWithMutation(t *testing.T) {
	inner := &friendCacheStoreStub{relation: true}
	redisStub := newFriendCacheRedisStub()
	redisStub.blockNextSet = true
	redisStub.setStarted = make(chan struct{})
	redisStub.setRelease = make(chan struct{})
	cache := newCachedFriendStore(inner, redisStub)

	if value, err := cache.AreFriends(t.Context(), 7, 8); err != nil || !value {
		t.Fatalf("AreFriends = %v, %v; want true, nil", value, err)
	}
	select {
	case <-redisStub.setStarted:
	case <-time.After(time.Second):
		t.Fatal("asynchronous Redis backfill did not start")
	}
	done := make(chan error, 1)
	go func() { done <- cache.RemoveFriends(t.Context(), 7, 8) }()
	close(redisStub.setRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	redisStub.mu.Lock()
	value := redisStub.values[relationRedisKey(relationKey(7, 8))]
	redisStub.mu.Unlock()
	if value != "0" {
		t.Fatalf("final Redis relation = %q, want mutation value 0", value)
	}
}

type friendInvalidationBusStub struct {
	mu          sync.Mutex
	subscribers []func([]byte)
	ready       chan struct{}
	readyOnce   sync.Once
}

func (bus *friendInvalidationBusStub) Publish(_ context.Context, message []byte) error {
	bus.mu.Lock()
	subscribers := append([]func([]byte){}, bus.subscribers...)
	bus.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber(message)
	}
	return nil
}

func (bus *friendInvalidationBusStub) Subscribe(ctx context.Context, handle func([]byte)) error {
	bus.mu.Lock()
	bus.subscribers = append(bus.subscribers, handle)
	bus.readyOnce.Do(func() { close(bus.ready) })
	bus.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func TestCachedFriendStorePropagatesInvalidationAcrossInstances(t *testing.T) {
	inner := &friendCacheStoreStub{relation: true}
	redisStub := newFriendCacheRedisStub()
	bus := &friendInvalidationBusStub{ready: make(chan struct{})}
	first := newCachedFriendStoreWithBus(inner, redisStub, bus)
	second := newCachedFriendStoreWithBus(inner, redisStub, bus)
	if value, err := second.AreFriends(t.Context(), 51, 52); err != nil || !value {
		t.Fatalf("warm second instance = %v, %v", value, err)
	}

	watchCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	notified := make(chan struct{}, 1)
	go second.WatchFriendInvalidations(watchCtx, func(uid, peerUID uint64) {
		if uid == 51 && peerUID == 52 {
			notified <- struct{}{}
		}
	})
	select {
	case <-bus.ready:
	case <-time.After(time.Second):
		t.Fatal("second instance did not subscribe")
	}
	inner.mu.Lock()
	inner.relation = false
	inner.mu.Unlock()
	if err := first.RemoveFriends(t.Context(), 51, 52); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("cross-instance invalidation was not delivered")
	}
	if value, err := second.AreFriends(t.Context(), 51, 52); err != nil || value {
		t.Fatalf("second instance after invalidation = %v, %v; want false, nil", value, err)
	}
}

func TestCachedFriendStoreListUsesRedisAndReturnsCopies(t *testing.T) {
	inner := &friendCacheStoreStub{}
	redisStub := newFriendCacheRedisStub()
	redisStub.values[friendListRedisKey(42)] = `[{"UID":99,"Nickname":"ninety-nine"}]`
	cache := newCachedFriendStore(inner, redisStub)

	first, err := cache.ListFriends(t.Context(), 42)
	if err != nil || len(first) != 1 {
		t.Fatalf("first ListFriends = %#v, %v", first, err)
	}
	first[0].Nickname = "mutated"
	second, err := cache.ListFriends(t.Context(), 42)
	if err != nil || len(second) != 1 || second[0].Nickname != "ninety-nine" {
		t.Fatalf("second ListFriends = %#v, %v", second, err)
	}
	if inner.listHits != 0 || redisStub.gets != 1 {
		t.Fatalf("inner=%d redis gets=%d, want 0/1", inner.listHits, redisStub.gets)
	}
}
