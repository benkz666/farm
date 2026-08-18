package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	// Search results change only when an account is created or its public
	// profile is renamed. Keep the old hot-path lifetime while bounding a
	// not-found result so a newly registered account becomes visible quickly.
	userSearchLocalTTL      = 5 * time.Minute
	userSearchLocalMissTTL  = 30 * time.Second
	userSearchCacheShards   = 64
	userSearchCacheCapacity = 65_536
)

type userSearchCacheEntry struct {
	value     UserSearchRow
	found     bool
	expiresAt int64
}

type userSearchCacheShard struct {
	mu        sync.RWMutex
	entries   map[string]userSearchCacheEntry
	positions map[string]int
	slots     []string
	next      int
}

type userSearchCall struct {
	done  chan struct{}
	value UserSearchRow
	err   error
}

type userSearchFlightShard struct {
	mu    sync.Mutex
	calls map[string]*userSearchCall
}

func userSearchShardIndex(username string) uint64 {
	hash := uint64(1469598103934665603)
	for index := 0; index < len(username); index++ {
		hash ^= uint64(username[index])
		hash *= 1099511628211
	}
	return hash & (userSearchCacheShards - 1)
}

func (cache *cachedFriendStore) localUserSearch(username string, now int64) (UserSearchRow, bool, bool) {
	shard := &cache.searchShards[userSearchShardIndex(username)]
	shard.mu.RLock()
	entry, ok := shard.entries[username]
	shard.mu.RUnlock()
	if !ok || now >= entry.expiresAt {
		if ok {
			shard.mu.Lock()
			if current, exists := shard.entries[username]; exists && now >= current.expiresAt {
				shard.remove(username)
			}
			shard.mu.Unlock()
		}
		return UserSearchRow{}, false, false
	}
	return entry.value, entry.found, true
}

func (cache *cachedFriendStore) putLocalUserSearch(username string, value UserSearchRow, found bool, now time.Time) {
	shard := &cache.searchShards[userSearchShardIndex(username)]
	shard.mu.Lock()
	if _, exists := shard.entries[username]; !exists {
		perShard := userSearchCacheCapacity / userSearchCacheShards
		if len(shard.slots) < perShard {
			shard.positions[username] = len(shard.slots)
			shard.slots = append(shard.slots, username)
		} else {
			if shard.next >= len(shard.slots) {
				shard.next = 0
			}
			victim := shard.slots[shard.next]
			delete(shard.entries, victim)
			delete(shard.positions, victim)
			shard.slots[shard.next] = username
			shard.positions[username] = shard.next
			shard.next++
		}
	}
	ttl := userSearchLocalTTL
	if !found {
		ttl = userSearchLocalMissTTL
	}
	shard.entries[username] = userSearchCacheEntry{
		value: value, found: found, expiresAt: now.Add(ttl).UnixNano(),
	}
	shard.mu.Unlock()
}

func (shard *userSearchCacheShard) remove(username string) {
	position, exists := shard.positions[username]
	if !exists {
		delete(shard.entries, username)
		return
	}
	last := len(shard.slots) - 1
	moved := shard.slots[last]
	shard.slots[position] = moved
	shard.positions[moved] = position
	shard.slots = shard.slots[:last]
	delete(shard.positions, username)
	delete(shard.entries, username)
	if len(shard.slots) == 0 || shard.next >= len(shard.slots) {
		shard.next = 0
	}
}

func (cache *cachedFriendStore) coalesceUserSearch(
	ctx context.Context,
	username string,
	load func() (UserSearchRow, error),
) (UserSearchRow, error) {
	shard := &cache.searchFlights[userSearchShardIndex(username)]
	shard.mu.Lock()
	if call := shard.calls[username]; call != nil {
		shard.mu.Unlock()
		select {
		case <-call.done:
			return call.value, call.err
		case <-ctx.Done():
			return UserSearchRow{}, ctx.Err()
		}
	}
	call := &userSearchCall{done: make(chan struct{})}
	shard.calls[username] = call
	shard.mu.Unlock()

	call.value, call.err = load()
	shard.mu.Lock()
	delete(shard.calls, username)
	close(call.done)
	shard.mu.Unlock()
	return call.value, call.err
}

// FindUserByUsername restores the process-local hot and negative caches that
// existed before Social became the sole owner of friend business. Keeping it
// here preserves the thin-Gateway architecture without sending every search
// request through to MySQL.
func (cache *cachedFriendStore) FindUserByUsername(ctx context.Context, username string) (UserSearchRow, error) {
	if cache == nil || cache.inner == nil {
		return UserSearchRow{}, ErrAccountNotFound
	}
	if value, found, ok := cache.localUserSearch(username, time.Now().UnixNano()); ok {
		if !found {
			return UserSearchRow{}, ErrAccountNotFound
		}
		return value, nil
	}
	return cache.coalesceUserSearch(ctx, username, func() (UserSearchRow, error) {
		if value, found, ok := cache.localUserSearch(username, time.Now().UnixNano()); ok {
			if !found {
				return UserSearchRow{}, ErrAccountNotFound
			}
			return value, nil
		}
		value, err := cache.inner.FindUserByUsername(ctx, username)
		if err != nil && !errors.Is(err, ErrAccountNotFound) {
			return UserSearchRow{}, err
		}
		cache.putLocalUserSearch(username, value, err == nil, time.Now())
		return value, err
	})
}
