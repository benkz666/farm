package friendauth

import (
	"context"
	"sync"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/rpcerr"
	"farm/server/shared/store"
)

const (
	defaultTTL      = 2 * time.Second
	defaultCapacity = 4096
)

type cacheEntry struct {
	value     bool
	expiresAt time.Time
}

// Cache wraps a FriendStore with a short TTL true-only AreFriends cache.
type Cache struct {
	inner    store.FriendStore
	ttl      time.Duration
	capacity int
	mu       sync.Mutex
	entries  map[pairKey]cacheEntry
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
		entries:  make(map[pairKey]cacheEntry, defaultCapacity),
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
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && entry.value && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return true, nil
	}
	c.mu.Unlock()

	value, err := c.inner.AreFriends(ctx, a, b)
	if err != nil || !value {
		return value, err
	}
	c.mu.Lock()
	c.ensureCapacityLocked(now)
	c.entries[key] = cacheEntry{value: true, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return true, nil
}

func (c *Cache) ensureCapacityLocked(now time.Time) {
	if len(c.entries) < c.capacity {
		return
	}
	c.evictExpiredLocked(now)
	if len(c.entries) < c.capacity {
		return
	}
	c.evictOneLocked()
}

func (c *Cache) Invalidate(a, b uint64) {
	if c == nil {
		return
	}
	key := pair(a, b)
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *Cache) evictExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

func (c *Cache) evictOneLocked() {
	for key := range c.entries {
		delete(c.entries, key)
		return
	}
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
	return c.inner.ListFriends(ctx, uid)
}

func (c *Cache) CountFriends(ctx context.Context, uid uint64) (int, error) {
	return c.inner.CountFriends(ctx, uid)
}

func (c *Cache) FindUserByUsername(ctx context.Context, username string) (store.UserSearchRow, error) {
	return c.inner.FindUserByUsername(ctx, username)
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
