package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	friendRelationLocalTTL      = 2 * time.Minute
	friendRelationLocalMissTTL  = time.Minute
	friendRelationRedisTTL      = 10 * time.Minute
	friendRelationRedisMissTTL  = 2 * time.Minute
	friendListLocalTTL          = 2 * time.Minute
	friendListRedisTTL          = 10 * time.Minute
	friendRelationCacheShards   = 64
	friendRelationCacheCapacity = 65_536
	friendListCacheCapacity     = 8_192
	friendCacheWriteWorkers     = 2
	friendCacheWriteQueue       = 8_192
	friendCacheStateShards      = 1_024
	friendRedisProbeWindow      = 256
	friendRedisMinimumHits      = 13 // about 5%; below this, MySQL is cheaper than an extra network hop
	friendRedisBypassDuration   = 2 * time.Second
)

const friendInvalidationChannel = "friend:invalidation:v1"

type friendCacheRedis interface {
	Get(context.Context, string) *redis.StringCmd
	Set(context.Context, string, any, time.Duration) *redis.StatusCmd
	Del(context.Context, ...string) *redis.IntCmd
}

type friendInvalidationBus interface {
	Publish(context.Context, string) error
	Subscribe(context.Context, func(string)) error
}

type redisFriendInvalidationBus struct {
	client *redis.Client
}

func (bus redisFriendInvalidationBus) Publish(ctx context.Context, message string) error {
	return bus.client.Publish(ctx, friendInvalidationChannel, message).Err()
}

func (bus redisFriendInvalidationBus) Subscribe(ctx context.Context, handle func(string)) error {
	pubsub := bus.client.Subscribe(ctx, friendInvalidationChannel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(ctx); err != nil {
		return err
	}
	messages := pubsub.Channel(redis.WithChannelSize(1_024))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message, ok := <-messages:
			if !ok {
				return errors.New("friend invalidation subscription closed")
			}
			handle(message.Payload)
		}
	}
}

type friendRelationKey struct {
	lo uint64
	hi uint64
}

type friendRelationEntry struct {
	value     bool
	expiresAt int64
}

type friendRelationShard struct {
	mu      sync.RWMutex
	entries map[friendRelationKey]friendRelationEntry
}

type friendListCacheEntry struct {
	value     []FriendRow
	expiresAt int64
}

type friendRelationCall struct {
	done  chan struct{}
	value bool
	err   error
}

type friendListCall struct {
	done  chan struct{}
	value []FriendRow
	err   error
}

type friendCacheWrite struct {
	kind     uint8
	key      string
	value    string
	ttl      time.Duration
	version  uint64
	relation friendRelationKey
	listUID  uint64
}

const (
	friendCacheWriteRelation uint8 = iota + 1
	friendCacheWriteList
)

type friendCacheStateShard struct {
	mu      sync.Mutex
	version atomic.Uint64
}

type friendInvalidationMessage struct {
	Source string `json:"source"`
	UID    string `json:"uid"`
	Peer   string `json:"peer_uid"`
	Known  bool   `json:"known"`
	Value  bool   `json:"value"`
}

// FriendInvalidationSource exposes cache changes committed by another Social
// replica. The callback is used to fan those changes out to this replica's
// connected Gateway and Farm watchers.
type FriendInvalidationSource interface {
	WatchFriendInvalidations(context.Context, func(uint64, uint64))
}

// cachedFriendStore is owned by Social. Reads are local -> Redis -> MySQL;
// mutations commit to MySQL first and then invalidate both cache levels.
type cachedFriendStore struct {
	inner    FriendStore
	redis    friendCacheRedis
	bus      friendInvalidationBus
	sourceID string

	relationShards  [friendRelationCacheShards]friendRelationShard
	listMu          sync.RWMutex
	lists           map[uint64]friendListCacheEntry
	relationState   [friendCacheStateShards]friendCacheStateShard
	listState       [friendCacheStateShards]friendCacheStateShard
	unsafeMu        sync.RWMutex
	unsafeRelations map[friendRelationKey]struct{}
	unsafeLists     map[uint64]struct{}

	flightMu        sync.Mutex
	relationFlights map[friendRelationKey]*friendRelationCall
	listFlights     map[uint64]*friendListCall
	writes          chan friendCacheWrite
	redisAttempts   atomic.Uint64
	redisHits       atomic.Uint64
	redisBypassTill atomic.Int64
}

var friendCacheInstanceSequence atomic.Uint64

func newCachedFriendStore(inner FriendStore, client friendCacheRedis) *cachedFriendStore {
	return newCachedFriendStoreWithBus(inner, client, nil)
}

func newCachedFriendStoreWithBus(inner FriendStore, client friendCacheRedis, bus friendInvalidationBus) *cachedFriendStore {
	cache := &cachedFriendStore{
		inner:           inner,
		redis:           client,
		bus:             bus,
		sourceID:        strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(friendCacheInstanceSequence.Add(1), 10),
		lists:           make(map[uint64]friendListCacheEntry, friendListCacheCapacity),
		unsafeRelations: make(map[friendRelationKey]struct{}),
		unsafeLists:     make(map[uint64]struct{}),
		relationFlights: make(map[friendRelationKey]*friendRelationCall),
		listFlights:     make(map[uint64]*friendListCall),
	}
	if client != nil {
		cache.writes = make(chan friendCacheWrite, friendCacheWriteQueue)
		for range friendCacheWriteWorkers {
			go cache.writeRedisLoop()
		}
	}
	perShard := friendRelationCacheCapacity / friendRelationCacheShards
	for i := range cache.relationShards {
		cache.relationShards[i].entries = make(map[friendRelationKey]friendRelationEntry, perShard)
	}
	return cache
}

func relationKey(a, b uint64) friendRelationKey {
	lo, hi := friendshipPair(a, b)
	return friendRelationKey{lo: lo, hi: hi}
}

func relationRedisKey(key friendRelationKey) string {
	return "friend:relation:v1:" + strconv.FormatUint(key.lo, 10) + ":" + strconv.FormatUint(key.hi, 10)
}

func friendListRedisKey(uid uint64) string {
	return "friend:list:v1:" + strconv.FormatUint(uid, 10)
}

func relationShardIndex(key friendRelationKey) uint64 {
	// Both UIDs participate so sequential fixture IDs do not collapse onto a
	// single shard. The final mask is valid because the shard count is a power of two.
	return relationShardIndexValue(key) & (friendRelationCacheShards - 1)
}

func relationStateIndex(key friendRelationKey) uint64 {
	return relationShardIndexValue(key) & (friendCacheStateShards - 1)
}

func relationShardIndexValue(key friendRelationKey) uint64 {
	x := key.lo ^ (key.hi + 0x9e3779b97f4a7c15 + (key.lo << 6) + (key.lo >> 2))
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	return x ^ (x >> 31)
}

func listStateIndex(uid uint64) uint64 {
	x := uid
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return (x ^ (x >> 31)) & (friendCacheStateShards - 1)
}

func (cache *cachedFriendStore) localRelation(key friendRelationKey, now int64) (bool, bool) {
	shard := &cache.relationShards[relationShardIndex(key)]
	shard.mu.RLock()
	entry, ok := shard.entries[key]
	shard.mu.RUnlock()
	if !ok || now >= entry.expiresAt {
		if ok {
			shard.mu.Lock()
			if current, exists := shard.entries[key]; exists && now >= current.expiresAt {
				delete(shard.entries, key)
			}
			shard.mu.Unlock()
		}
		return false, false
	}
	return entry.value, true
}

func (cache *cachedFriendStore) putLocalRelation(key friendRelationKey, value bool, now time.Time) {
	ttl := friendRelationLocalTTL
	if !value {
		ttl = friendRelationLocalMissTTL
	}
	shard := &cache.relationShards[relationShardIndex(key)]
	shard.mu.Lock()
	perShard := friendRelationCacheCapacity / friendRelationCacheShards
	if _, exists := shard.entries[key]; !exists && len(shard.entries) >= perShard {
		for candidate, entry := range shard.entries {
			if now.UnixNano() >= entry.expiresAt {
				delete(shard.entries, candidate)
			}
		}
	}
	if _, exists := shard.entries[key]; !exists && len(shard.entries) >= perShard {
		for candidate := range shard.entries {
			delete(shard.entries, candidate)
			break
		}
	}
	shard.entries[key] = friendRelationEntry{value: value, expiresAt: now.Add(ttl).UnixNano()}
	shard.mu.Unlock()
}

func (cache *cachedFriendStore) deleteLocalRelation(key friendRelationKey) {
	shard := &cache.relationShards[relationShardIndex(key)]
	shard.mu.Lock()
	delete(shard.entries, key)
	shard.mu.Unlock()
}

func (cache *cachedFriendStore) localList(uid uint64, now int64) ([]FriendRow, bool) {
	cache.listMu.RLock()
	entry, ok := cache.lists[uid]
	cache.listMu.RUnlock()
	if !ok || now >= entry.expiresAt {
		if ok {
			cache.listMu.Lock()
			if current, exists := cache.lists[uid]; exists && now >= current.expiresAt {
				delete(cache.lists, uid)
			}
			cache.listMu.Unlock()
		}
		return nil, false
	}
	return cloneFriendRows(entry.value), true
}

func (cache *cachedFriendStore) putLocalList(uid uint64, value []FriendRow, now time.Time) {
	cache.listMu.Lock()
	if _, exists := cache.lists[uid]; !exists && len(cache.lists) >= friendListCacheCapacity {
		for candidate, entry := range cache.lists {
			if now.UnixNano() >= entry.expiresAt {
				delete(cache.lists, candidate)
			}
		}
	}
	if _, exists := cache.lists[uid]; !exists && len(cache.lists) >= friendListCacheCapacity {
		for candidate := range cache.lists {
			delete(cache.lists, candidate)
			break
		}
	}
	cache.lists[uid] = friendListCacheEntry{value: cloneFriendRows(value), expiresAt: now.Add(friendListLocalTTL).UnixNano()}
	cache.listMu.Unlock()
}

func (cache *cachedFriendStore) deleteLocalLists(a, b uint64) {
	cache.listMu.Lock()
	delete(cache.lists, a)
	delete(cache.lists, b)
	cache.listMu.Unlock()
}

func (cache *cachedFriendStore) AreFriends(ctx context.Context, a, b uint64) (bool, error) {
	if cache == nil || cache.inner == nil {
		return false, nil
	}
	key := relationKey(a, b)
	if value, ok := cache.localRelation(key, time.Now().UnixNano()); ok {
		return value, nil
	}
	return cache.coalesceRelation(ctx, key, func() (bool, error) {
		if value, ok := cache.localRelation(key, time.Now().UnixNano()); ok {
			return value, nil
		}
		state := &cache.relationState[relationStateIndex(key)]
		startedVersion := state.version.Load()
		if !cache.isUnsafeRelation(key) && cache.shouldReadRelationRedis(time.Now()) {
			encoded, err := cache.redis.Get(ctx, relationRedisKey(key)).Result()
			if err == nil && (encoded == "1" || encoded == "0") {
				cache.redisHits.Add(1)
				value := encoded == "1"
				state.mu.Lock()
				current := state.version.Load()
				if current == startedVersion && !cache.isUnsafeRelation(key) {
					cache.putLocalRelation(key, value, time.Now())
					state.mu.Unlock()
					return value, nil
				}
				state.mu.Unlock()
			}
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
		}

		value, err := cache.inner.AreFriends(ctx, a, b)
		if err != nil {
			return false, err
		}
		ttl := friendRelationRedisTTL
		encoded := "1"
		if !value {
			ttl = friendRelationRedisMissTTL
			encoded = "0"
		}
		state.mu.Lock()
		if state.version.Load() == startedVersion {
			cache.putLocalRelation(key, value, time.Now())
			cache.enqueueRedisWrite(friendCacheWrite{
				kind: friendCacheWriteRelation, key: relationRedisKey(key), value: encoded, ttl: ttl,
				version: startedVersion, relation: key,
			})
		}
		state.mu.Unlock()
		return value, nil
	})
}

// shouldReadRelationRedis protects the all-unique cold path. Redis is valuable
// when another Social instance or a restart has already populated a relation,
// but an extra GET on every never-seen pair only reduces authoritative-query
// throughput. A small probe window keeps Redis enabled for real hit rates and
// temporarily bypasses it when the observed hit rate is below about 5%.
func (cache *cachedFriendStore) shouldReadRelationRedis(now time.Time) bool {
	if cache == nil || cache.redis == nil {
		return false
	}
	if now.UnixNano() < cache.redisBypassTill.Load() {
		return false
	}
	attempt := cache.redisAttempts.Add(1)
	if attempt >= friendRedisProbeWindow && cache.redisAttempts.CompareAndSwap(attempt, 0) {
		hits := cache.redisHits.Swap(0)
		if hits < friendRedisMinimumHits {
			cache.redisBypassTill.Store(now.Add(friendRedisBypassDuration).UnixNano())
		}
	}
	return true
}

func (cache *cachedFriendStore) ListFriends(ctx context.Context, uid uint64) ([]FriendRow, error) {
	if cache == nil || cache.inner == nil {
		return nil, nil
	}
	if value, ok := cache.localList(uid, time.Now().UnixNano()); ok {
		return value, nil
	}
	return cache.coalesceList(ctx, uid, func() ([]FriendRow, error) {
		if value, ok := cache.localList(uid, time.Now().UnixNano()); ok {
			return value, nil
		}
		state := &cache.listState[listStateIndex(uid)]
		startedVersion := state.version.Load()
		if cache.redis != nil && !cache.isUnsafeList(uid) {
			encoded, err := cache.redis.Get(ctx, friendListRedisKey(uid)).Bytes()
			if err == nil {
				var value []FriendRow
				if json.Unmarshal(encoded, &value) == nil {
					state.mu.Lock()
					current := state.version.Load()
					if current == startedVersion && !cache.isUnsafeList(uid) {
						cache.putLocalList(uid, value, time.Now())
						state.mu.Unlock()
						return value, nil
					}
					state.mu.Unlock()
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}

		value, err := cache.inner.ListFriends(ctx, uid)
		if err != nil {
			return nil, err
		}
		state.mu.Lock()
		if state.version.Load() == startedVersion {
			cache.putLocalList(uid, value, time.Now())
			if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
				cache.enqueueRedisWrite(friendCacheWrite{
					kind: friendCacheWriteList, key: friendListRedisKey(uid), value: string(encoded), ttl: friendListRedisTTL,
					version: startedVersion, listUID: uid,
				})
			}
		}
		state.mu.Unlock()
		return value, nil
	})
}

func (cache *cachedFriendStore) enqueueRedisWrite(write friendCacheWrite) {
	if cache == nil || cache.writes == nil {
		return
	}
	select {
	case cache.writes <- write:
	default:
		// A saturated cache writer must never apply backpressure to gameplay.
		// The process-local entry still serves the active working set, and a
		// later miss can retry the Redis backfill.
	}
}

func (cache *cachedFriendStore) writeRedisLoop() {
	for write := range cache.writes {
		cache.writeRedis(write)
	}
}

func (cache *cachedFriendStore) writeRedis(write friendCacheWrite) {
	var state *friendCacheStateShard
	switch write.kind {
	case friendCacheWriteRelation:
		state = &cache.relationState[relationStateIndex(write.relation)]
	case friendCacheWriteList:
		state = &cache.listState[listStateIndex(write.listUID)]
	default:
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.version.Load() != write.version {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := cache.redis.Set(ctx, write.key, write.value, write.ttl).Err()
	cancel()
	if err != nil {
		return
	}
	if write.kind == friendCacheWriteRelation {
		cache.clearUnsafeRelation(write.relation)
	} else {
		cache.clearUnsafeList(write.listUID)
	}
}

func (cache *cachedFriendStore) coalesceRelation(ctx context.Context, key friendRelationKey, load func() (bool, error)) (bool, error) {
	cache.flightMu.Lock()
	if call := cache.relationFlights[key]; call != nil {
		cache.flightMu.Unlock()
		select {
		case <-call.done:
			return call.value, call.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	call := &friendRelationCall{done: make(chan struct{})}
	cache.relationFlights[key] = call
	cache.flightMu.Unlock()

	call.value, call.err = load()
	cache.flightMu.Lock()
	delete(cache.relationFlights, key)
	close(call.done)
	cache.flightMu.Unlock()
	return call.value, call.err
}

func (cache *cachedFriendStore) coalesceList(ctx context.Context, uid uint64, load func() ([]FriendRow, error)) ([]FriendRow, error) {
	cache.flightMu.Lock()
	if call := cache.listFlights[uid]; call != nil {
		cache.flightMu.Unlock()
		select {
		case <-call.done:
			return cloneFriendRows(call.value), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &friendListCall{done: make(chan struct{})}
	cache.listFlights[uid] = call
	cache.flightMu.Unlock()

	call.value, call.err = load()
	cache.flightMu.Lock()
	delete(cache.listFlights, uid)
	close(call.done)
	cache.flightMu.Unlock()
	return cloneFriendRows(call.value), call.err
}

func cloneFriendRows(value []FriendRow) []FriendRow {
	return append([]FriendRow(nil), value...)
}

func (cache *cachedFriendStore) isUnsafeRelation(key friendRelationKey) bool {
	cache.unsafeMu.RLock()
	_, unsafe := cache.unsafeRelations[key]
	cache.unsafeMu.RUnlock()
	return unsafe
}

func (cache *cachedFriendStore) markUnsafeRelation(key friendRelationKey) {
	cache.unsafeMu.Lock()
	cache.unsafeRelations[key] = struct{}{}
	cache.unsafeMu.Unlock()
}

func (cache *cachedFriendStore) clearUnsafeRelation(key friendRelationKey) {
	cache.unsafeMu.Lock()
	delete(cache.unsafeRelations, key)
	cache.unsafeMu.Unlock()
}

func (cache *cachedFriendStore) isUnsafeList(uid uint64) bool {
	cache.unsafeMu.RLock()
	_, unsafe := cache.unsafeLists[uid]
	cache.unsafeMu.RUnlock()
	return unsafe
}

func (cache *cachedFriendStore) markUnsafeList(uid uint64) {
	cache.unsafeMu.Lock()
	cache.unsafeLists[uid] = struct{}{}
	cache.unsafeMu.Unlock()
}

func (cache *cachedFriendStore) clearUnsafeList(uid uint64) {
	cache.unsafeMu.Lock()
	delete(cache.unsafeLists, uid)
	cache.unsafeMu.Unlock()
}

func (cache *cachedFriendStore) updateRelationCache(ctx context.Context, a, b uint64, known, value bool) {
	key := relationKey(a, b)
	state := &cache.relationState[relationStateIndex(key)]
	state.mu.Lock()
	state.version.Add(1)
	if known {
		cache.putLocalRelation(key, value, time.Now())
	} else {
		cache.deleteLocalRelation(key)
	}
	if cache.redis == nil {
		state.mu.Unlock()
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	var err error
	if known {
		encoded := "0"
		ttl := friendRelationRedisMissTTL
		if value {
			encoded = "1"
			ttl = friendRelationRedisTTL
		}
		err = cache.redis.Set(writeCtx, relationRedisKey(key), encoded, ttl).Err()
	} else {
		err = cache.redis.Del(writeCtx, relationRedisKey(key)).Err()
	}
	cancel()
	if err != nil {
		cache.markUnsafeRelation(key)
	} else {
		cache.clearUnsafeRelation(key)
	}
	state.mu.Unlock()
}

func (cache *cachedFriendStore) invalidateListCaches(ctx context.Context, a, b uint64) {
	firstIndex, secondIndex := listStateIndex(a), listStateIndex(b)
	if firstIndex > secondIndex {
		firstIndex, secondIndex = secondIndex, firstIndex
	}
	first := &cache.listState[firstIndex]
	first.mu.Lock()
	var second *friendCacheStateShard
	if secondIndex != firstIndex {
		second = &cache.listState[secondIndex]
		second.mu.Lock()
	}
	cache.listState[listStateIndex(a)].version.Add(1)
	if b != a {
		cache.listState[listStateIndex(b)].version.Add(1)
	}
	cache.deleteLocalLists(a, b)
	if cache.redis != nil {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		err := cache.redis.Del(deleteCtx, friendListRedisKey(a), friendListRedisKey(b)).Err()
		cancel()
		if err != nil {
			cache.markUnsafeList(a)
			cache.markUnsafeList(b)
		} else {
			cache.clearUnsafeList(a)
			cache.clearUnsafeList(b)
		}
	}
	if second != nil {
		second.mu.Unlock()
	}
	first.mu.Unlock()
}

func (cache *cachedFriendStore) applyInvalidation(ctx context.Context, a, b uint64, known, value, publish bool) {
	cache.updateRelationCache(ctx, a, b, known, value)
	cache.invalidateListCaches(ctx, a, b)
	if publish {
		cache.publishInvalidation(ctx, a, b, known, value)
	}
}

func (cache *cachedFriendStore) publishInvalidation(ctx context.Context, a, b uint64, known, value bool) {
	if cache.bus == nil {
		return
	}
	encoded, err := json.Marshal(friendInvalidationMessage{
		Source: cache.sourceID,
		UID:    strconv.FormatUint(a, 10),
		Peer:   strconv.FormatUint(b, 10),
		Known:  known,
		Value:  value,
	})
	if err != nil {
		return
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	_ = cache.bus.Publish(publishCtx, string(encoded))
	cancel()
}

// WatchFriendInvalidations consumes Redis Pub/Sub changes from other Social
// replicas, repairs this replica's caches and then notifies its local watchers.
func (cache *cachedFriendStore) WatchFriendInvalidations(ctx context.Context, notify func(uint64, uint64)) {
	if cache == nil || cache.bus == nil {
		return
	}
	for ctx.Err() == nil {
		err := cache.bus.Subscribe(ctx, func(payload string) {
			var message friendInvalidationMessage
			if json.Unmarshal([]byte(payload), &message) != nil || message.Source == cache.sourceID {
				return
			}
			uid, uidErr := strconv.ParseUint(message.UID, 10, 64)
			peerUID, peerErr := strconv.ParseUint(message.Peer, 10, 64)
			if uidErr != nil || peerErr != nil || uid == 0 || peerUID == 0 {
				return
			}
			cache.applyInvalidation(ctx, uid, peerUID, message.Known, message.Value, false)
			if notify != nil {
				notify(uid, peerUID)
			}
		})
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (cache *cachedFriendStore) AddFriends(ctx context.Context, a, b uint64) error {
	if err := cache.inner.AddFriends(ctx, a, b); err != nil {
		return err
	}
	cache.applyInvalidation(ctx, a, b, true, true, true)
	return nil
}

func (cache *cachedFriendStore) RemoveFriends(ctx context.Context, a, b uint64) error {
	if err := cache.inner.RemoveFriends(ctx, a, b); err != nil {
		return err
	}
	cache.applyInvalidation(ctx, a, b, true, false, true)
	return nil
}

func (cache *cachedFriendStore) CountFriends(ctx context.Context, uid uint64) (int, error) {
	return cache.inner.CountFriends(ctx, uid)
}

func (cache *cachedFriendStore) FindUserByUsername(ctx context.Context, username string) (UserSearchRow, error) {
	return cache.inner.FindUserByUsername(ctx, username)
}

func (cache *cachedFriendStore) CreateFriendRequest(ctx context.Context, fromUID, toUID uint64) error {
	if err := cache.inner.CreateFriendRequest(ctx, fromUID, toUID); err != nil {
		return err
	}
	// A reverse pending request can turn this operation into an implicit accept,
	// so read the committed relationship instead of assuming a negative value.
	value, err := cache.inner.AreFriends(ctx, fromUID, toUID)
	cache.applyInvalidation(ctx, fromUID, toUID, err == nil, value, true)
	return nil
}

func (cache *cachedFriendStore) ListIncomingFriendRequests(ctx context.Context, uid uint64) ([]FriendRequestRow, error) {
	return cache.inner.ListIncomingFriendRequests(ctx, uid)
}

func (cache *cachedFriendStore) AcceptFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	if err := cache.inner.AcceptFriendRequest(ctx, toUID, fromUID); err != nil {
		return err
	}
	cache.applyInvalidation(ctx, toUID, fromUID, true, true, true)
	return nil
}

func (cache *cachedFriendStore) RejectFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	return cache.inner.RejectFriendRequest(ctx, toUID, fromUID)
}

var _ FriendStore = (*cachedFriendStore)(nil)
var _ FriendInvalidationSource = (*cachedFriendStore)(nil)
