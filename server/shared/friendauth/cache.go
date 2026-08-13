package friendauth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/rpcerr"
	"farm/server/shared/store"
)

const (
	// Invalidation streams keep relationship changes coherent, so the hot true
	// cache can cover the full active-user fixture without a two-second churn.
	defaultTTL      = 30 * time.Second
	defaultCapacity = 65536
	friendListTTL   = 30 * time.Second
	userSearchTTL   = 5 * time.Minute
	searchMissTTL   = 30 * time.Second
	cacheShardCount = 64
)

type cacheEntry struct {
	value     bool
	expiresAt time.Time
}

type friendListEntry struct {
	value     []store.FriendRow
	expiresAt time.Time
}

type userSearchEntry struct {
	value     store.UserSearchRow
	found     bool
	expiresAt time.Time
}

type relationShard struct {
	mu      sync.Mutex
	entries map[pairKey]cacheEntry
}

type friendListShard struct {
	mu      sync.Mutex
	entries map[uint64]friendListEntry
}

type userSearchShard struct {
	mu      sync.Mutex
	entries map[string]userSearchEntry
}

// Cache wraps a FriendStore with a short TTL true-only AreFriends cache.
type Cache struct {
	inner     store.FriendStore
	ttl       time.Duration
	capacity  int
	revision  atomic.Uint64
	relations [cacheShardCount]relationShard
	lists     [cacheShardCount]friendListShard
	searches  [cacheShardCount]userSearchShard
}

type pairKey struct {
	lo uint64
	hi uint64
}

// NewCache constructs a bounded true-only friendship cache.
func NewCache(inner store.FriendStore) *Cache {
	return &Cache{
		inner:    inner,
		ttl:      defaultTTL,
		capacity: defaultCapacity,
	}
}

func pair(a, b uint64) pairKey {
	if a < b {
		return pairKey{lo: a, hi: b}
	}
	return pairKey{lo: b, hi: a}
}

func (c *Cache) AreFriends(ctx context.Context, a, b uint64) (bool, error) {
	if c == nil || c.inner == nil {
		return false, nil
	}
	key := pair(a, b)
	now := time.Now()
	shard := &c.relations[pairShardIndex(key)]
	shard.mu.Lock()
	if entry, ok := shard.entries[key]; ok && entry.value && now.Before(entry.expiresAt) {
		shard.mu.Unlock()
		return true, nil
	}
	shard.mu.Unlock()

	value, err := c.inner.AreFriends(ctx, a, b)
	if err != nil || !value {
		return value, err
	}
	shard.mu.Lock()
	if shard.entries == nil {
		shard.entries = make(map[pairKey]cacheEntry, c.shardCapacity())
	}
	ensureRelationCapacityLocked(shard.entries, c.shardCapacity(), now)
	shard.entries[key] = cacheEntry{value: true, expiresAt: now.Add(c.ttl)}
	shard.mu.Unlock()
	return true, nil
}

func ensureRelationCapacityLocked(entries map[pairKey]cacheEntry, capacity int, now time.Time) {
	if len(entries) < capacity {
		return
	}
	for key, entry := range entries {
		if now.After(entry.expiresAt) {
			delete(entries, key)
		}
	}
	if len(entries) < capacity {
		return
	}
	for key := range entries {
		delete(entries, key)
		return
	}
}

func (c *Cache) Invalidate(a, b uint64) {
	if c == nil {
		return
	}
	key := pair(a, b)
	relation := &c.relations[pairShardIndex(key)]
	relation.mu.Lock()
	delete(relation.entries, key)
	relation.mu.Unlock()
	c.deleteFriendList(a)
	if b != a {
		c.deleteFriendList(b)
	}
	// A single monotonic epoch deliberately invalidates all connection-local
	// leases. Friendship changes are rare, so this is cheaper and less fragile
	// than maintaining a second pair-indexed watcher registry in Gateway.
	c.revision.Add(1)
}

// Revision exposes the invalidation epoch used by Gateway's short-lived room
// authorization lease. It does not expose cache contents.
func (c *Cache) Revision() uint64 {
	if c == nil {
		return 0
	}
	return c.revision.Load()
}

func (c *Cache) AddFriends(ctx context.Context, a, b uint64) error {
	if err := c.inner.AddFriends(ctx, a, b); err != nil {
		return err
	}
	c.Invalidate(a, b)
	return nil
}

func (c *Cache) RemoveFriends(ctx context.Context, a, b uint64) error {
	if err := c.inner.RemoveFriends(ctx, a, b); err != nil {
		return err
	}
	c.Invalidate(a, b)
	return nil
}

func (c *Cache) ListFriends(ctx context.Context, uid uint64) ([]store.FriendRow, error) {
	if c == nil || c.inner == nil {
		return nil, nil
	}
	now := time.Now()
	shard := &c.lists[uidShardIndex(uid)]
	shard.mu.Lock()
	if entry, ok := shard.entries[uid]; ok && now.Before(entry.expiresAt) {
		result := cloneFriends(entry.value)
		shard.mu.Unlock()
		return result, nil
	}
	delete(shard.entries, uid)
	shard.mu.Unlock()

	result, err := c.inner.ListFriends(ctx, uid)
	if err != nil {
		return nil, err
	}
	shard.mu.Lock()
	if shard.entries == nil {
		shard.entries = make(map[uint64]friendListEntry, c.shardCapacity())
	}
	ensureMapCapacityLocked(shard.entries, c.shardCapacity())
	shard.entries[uid] = friendListEntry{value: cloneFriends(result), expiresAt: now.Add(friendListTTL)}
	shard.mu.Unlock()
	return result, nil
}

func (c *Cache) CountFriends(ctx context.Context, uid uint64) (int, error) {
	return c.inner.CountFriends(ctx, uid)
}

func (c *Cache) FindUserByUsername(ctx context.Context, username string) (store.UserSearchRow, error) {
	if c == nil || c.inner == nil {
		return store.UserSearchRow{}, store.ErrAccountNotFound
	}
	now := time.Now()
	shard := &c.searches[stringShardIndex(username)]
	shard.mu.Lock()
	if entry, ok := shard.entries[username]; ok && now.Before(entry.expiresAt) {
		shard.mu.Unlock()
		if !entry.found {
			return store.UserSearchRow{}, store.ErrAccountNotFound
		}
		return entry.value, nil
	}
	delete(shard.entries, username)
	shard.mu.Unlock()

	result, err := c.inner.FindUserByUsername(ctx, username)
	if err != nil && !errors.Is(err, store.ErrAccountNotFound) {
		return store.UserSearchRow{}, err
	}
	entry := userSearchEntry{value: result, found: err == nil, expiresAt: now.Add(userSearchTTL)}
	if err != nil {
		entry.expiresAt = now.Add(searchMissTTL)
	}
	shard.mu.Lock()
	if shard.entries == nil {
		shard.entries = make(map[string]userSearchEntry, c.shardCapacity())
	}
	ensureMapCapacityLocked(shard.entries, c.shardCapacity())
	shard.entries[username] = entry
	shard.mu.Unlock()
	return result, err
}

func cloneFriends(friends []store.FriendRow) []store.FriendRow {
	return append([]store.FriendRow(nil), friends...)
}

func ensureMapCapacityLocked[K comparable, V any](entries map[K]V, capacity int) {
	if len(entries) < capacity {
		return
	}
	for key := range entries {
		delete(entries, key)
		return
	}
}

func (c *Cache) shardCapacity() int {
	capacity := c.capacity
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	capacity = (capacity + cacheShardCount - 1) / cacheShardCount
	if capacity < 1 {
		return 1
	}
	return capacity
}

func (c *Cache) deleteFriendList(uid uint64) {
	shard := &c.lists[uidShardIndex(uid)]
	shard.mu.Lock()
	delete(shard.entries, uid)
	shard.mu.Unlock()
}

func uidShardIndex(uid uint64) uint64 {
	uid ^= uid >> 30
	uid *= 0xbf58476d1ce4e5b9
	uid ^= uid >> 27
	uid *= 0x94d049bb133111eb
	return (uid ^ (uid >> 31)) & (cacheShardCount - 1)
}

func pairShardIndex(key pairKey) uint64 {
	return uidShardIndex(key.lo ^ key.hi*0x9e3779b97f4a7c15)
}

func stringShardIndex(value string) uint64 {
	hash := uint64(1469598103934665603)
	for index := 0; index < len(value); index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}
	return hash & (cacheShardCount - 1)
}

func (c *Cache) entryCount() int {
	total := 0
	for index := range c.relations {
		shard := &c.relations[index]
		shard.mu.Lock()
		total += len(shard.entries)
		shard.mu.Unlock()
	}
	return total
}

func (c *Cache) CreateFriendRequest(ctx context.Context, fromUID, toUID uint64) error {
	return c.inner.CreateFriendRequest(ctx, fromUID, toUID)
}

func (c *Cache) ListIncomingFriendRequests(ctx context.Context, uid uint64) ([]store.FriendRequestRow, error) {
	return c.inner.ListIncomingFriendRequests(ctx, uid)
}

func (c *Cache) AcceptFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	if err := c.inner.AcceptFriendRequest(ctx, toUID, fromUID); err != nil {
		return err
	}
	c.Invalidate(toUID, fromUID)
	return nil
}

func (c *Cache) RejectFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	return c.inner.RejectFriendRequest(ctx, toUID, fromUID)
}

// WatchInvalidations keeps the cache coherent with Social server broadcasts.
// uid=0 subscribes to all invalidations (wildcard).
func (c *Cache) WatchInvalidations(ctx context.Context, uid uint64, watch func(context.Context, uint64) (<-chan *farmv1.FriendInvalidation, error)) {
	if c == nil || watch == nil {
		return
	}
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		stream, err := watch(ctx, uid)
		if err != nil {
			if !sleepWithContext(ctx, backoff) {
				return
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 250 * time.Millisecond
		for {
			select {
			case <-ctx.Done():
				return
			case invalidation, ok := <-stream:
				if !ok {
					goto reconnect
				}
				if invalidation == nil {
					continue
				}
				c.Invalidate(invalidation.Uid, invalidation.PeerUid)
			}
		}
	reconnect:
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// GRPCWatch wraps SocialService.WatchFriendInvalidations for Cache consumers.
func GRPCWatch(client farmv1.SocialServiceClient) func(context.Context, uint64) (<-chan *farmv1.FriendInvalidation, error) {
	return func(ctx context.Context, uid uint64) (<-chan *farmv1.FriendInvalidation, error) {
		stream, err := client.WatchFriendInvalidations(ctx, &farmv1.UidRequest{Uid: uid})
		if err != nil {
			return nil, rpcerr.FromGRPC(err)
		}
		out := make(chan *farmv1.FriendInvalidation, 8)
		go func() {
			defer close(out)
			for {
				msg, recvErr := stream.Recv()
				if recvErr != nil {
					return
				}
				out <- msg
			}
		}()
		return out, nil
	}
}

var _ store.FriendStore = (*Cache)(nil)
