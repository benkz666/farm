package store

import (
	"sync"
	"time"
)

const (
	defaultReadCacheTTL = 30 * time.Second
	// The formal performance fixture keeps 15,000 accounts hot at once. Keep
	// enough headroom for that working set without making the cache unbounded.
	defaultReadCacheCapacity = 32_768
	readCacheShardCount      = 64
	taskCacheStateShardCount = 1024
)

type ttlValue[V any] struct {
	value     V
	expiresAt time.Time
}

// boundedTTLCache is bounded and process-local. Database state is still
// authoritative; this only absorbs repeated hot reads owned by the same
// service process.
type ttlCacheShard[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]ttlValue[V]
}

type boundedTTLCache[K comparable, V any] struct {
	shards   [readCacheShardCount]ttlCacheShard[K, V]
	ttl      time.Duration
	capacity int
}

func (cache *boundedTTLCache[K, V]) get(key K, now time.Time) (V, bool) {
	shard := &cache.shards[cacheShardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	entry, ok := shard.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !now.Before(entry.expiresAt) {
		delete(shard.entries, key)
		var zero V
		return zero, false
	}
	return entry.value, true
}

func (cache *boundedTTLCache[K, V]) put(key K, value V, now time.Time) {
	shard := &cache.shards[cacheShardIndex(key)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	capacity := cache.capacity
	if capacity <= 0 {
		capacity = defaultReadCacheCapacity
	}
	capacity = max(1, (capacity+readCacheShardCount-1)/readCacheShardCount)
	if shard.entries == nil {
		shard.entries = make(map[K]ttlValue[V], capacity)
	}
	ttl := cache.ttl
	if ttl <= 0 {
		ttl = defaultReadCacheTTL
	}
	if _, exists := shard.entries[key]; !exists && len(shard.entries) >= capacity {
		for candidate, entry := range shard.entries {
			if !now.Before(entry.expiresAt) {
				delete(shard.entries, candidate)
			}
		}
	}
	if _, exists := shard.entries[key]; !exists && len(shard.entries) >= capacity {
		for candidate := range shard.entries {
			delete(shard.entries, candidate)
			break
		}
	}
	shard.entries[key] = ttlValue[V]{value: value, expiresAt: now.Add(ttl)}
}

func (cache *boundedTTLCache[K, V]) delete(key K) {
	shard := &cache.shards[cacheShardIndex(key)]
	shard.mu.Lock()
	delete(shard.entries, key)
	shard.mu.Unlock()
}

type taskReadKey struct {
	uid    uint64
	dayKey int64
}

type taskCacheState struct {
	mu      sync.Mutex
	version uint64
}

func taskStateIndex(key taskReadKey) uint64 {
	return cacheHash(key) & (taskCacheStateShardCount - 1)
}

func (s *Store) taskCacheGeneration(key taskReadKey) uint64 {
	state := &s.taskCacheState[taskStateIndex(key)]
	state.mu.Lock()
	version := state.version
	state.mu.Unlock()
	return version
}

func (s *Store) putTaskReadIfCurrent(key taskReadKey, generation uint64, tasks []Task) bool {
	state := &s.taskCacheState[taskStateIndex(key)]
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.version != generation {
		return false
	}
	s.taskRead.put(key, cloneTasks(tasks), time.Now())
	return true
}

func (s *Store) invalidateTaskCache(key taskReadKey) {
	state := &s.taskCacheState[taskStateIndex(key)]
	state.mu.Lock()
	state.version++
	s.taskRead.delete(key)
	state.mu.Unlock()
}

func cacheShardIndex[K comparable](key K) uint64 {
	return cacheHash(key) & (readCacheShardCount - 1)
}

func cacheHash[K comparable](key K) uint64 {
	var value uint64
	switch typed := any(key).(type) {
	case uint64:
		value = typed
	case taskReadKey:
		value = typed.uid ^ uint64(typed.dayKey)*0x9e3779b97f4a7c15
	default:
		panic("store: unsupported local cache key type")
	}
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func cloneTasks(tasks []Task) []Task {
	return append([]Task(nil), tasks...)
}
