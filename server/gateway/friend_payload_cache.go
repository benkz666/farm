package gateway

import (
	"encoding/json"
	"sync"
	"time"
)

const (
	friendPayloadShardCount = 64
	friendPayloadCapacity   = 8192
	friendPayloadTTL        = 30 * time.Second
	searchPayloadTTL        = 5 * time.Minute
)

type friendPayloadEntry struct {
	items     []friendJSON
	payload   json.RawMessage
	expiresAt time.Time
}

type searchPayloadEntry struct {
	item      searchUserResponseItem
	payload   json.RawMessage
	expiresAt time.Time
}

type friendPayloadShard struct {
	mu       sync.Mutex
	friends  map[uint64]friendPayloadEntry
	searches map[string]searchPayloadEntry
}

// friendPayloadCache keeps already encoded client payloads behind independent
// shards. Entries retain their exact source values, so a friendship, nickname
// or steal-hint change is observed before an old encoding can be reused.
type friendPayloadCache struct {
	shards [friendPayloadShardCount]friendPayloadShard
}

func (cache *friendPayloadCache) friendList(uid uint64, items []friendJSON) json.RawMessage {
	if cache == nil {
		return marshalPayload(friendListResponse{Friends: items})
	}
	now := time.Now()
	shard := &cache.shards[payloadUIDShard(uid)]
	shard.mu.Lock()
	if entry, ok := shard.friends[uid]; ok && now.Before(entry.expiresAt) && equalFriendItems(entry.items, items) {
		payload := entry.payload
		shard.mu.Unlock()
		return payload
	}
	shard.mu.Unlock()

	payload := marshalPayload(friendListResponse{Friends: items})
	shard.mu.Lock()
	if shard.friends == nil {
		shard.friends = make(map[uint64]friendPayloadEntry, friendPayloadCapacity/friendPayloadShardCount)
	}
	evictPayloadEntry(shard.friends, friendPayloadCapacity/friendPayloadShardCount)
	shard.friends[uid] = friendPayloadEntry{
		items:     append([]friendJSON(nil), items...),
		payload:   payload,
		expiresAt: now.Add(friendPayloadTTL),
	}
	shard.mu.Unlock()
	return payload
}

func (cache *friendPayloadCache) search(username string, item searchUserResponseItem) json.RawMessage {
	if cache == nil {
		return marshalPayload(searchUserResponse{Users: []searchUserResponseItem{item}})
	}
	now := time.Now()
	shard := &cache.shards[payloadStringShard(username)]
	shard.mu.Lock()
	if entry, ok := shard.searches[username]; ok && now.Before(entry.expiresAt) && entry.item == item {
		payload := entry.payload
		shard.mu.Unlock()
		return payload
	}
	shard.mu.Unlock()

	payload := marshalPayload(searchUserResponse{Users: []searchUserResponseItem{item}})
	shard.mu.Lock()
	if shard.searches == nil {
		shard.searches = make(map[string]searchPayloadEntry, friendPayloadCapacity/friendPayloadShardCount)
	}
	evictPayloadEntry(shard.searches, friendPayloadCapacity/friendPayloadShardCount)
	shard.searches[username] = searchPayloadEntry{item: item, payload: payload, expiresAt: now.Add(searchPayloadTTL)}
	shard.mu.Unlock()
	return payload
}

func equalFriendItems(left, right []friendJSON) bool {
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

func evictPayloadEntry[K comparable, V any](entries map[K]V, capacity int) {
	if len(entries) < capacity {
		return
	}
	for key := range entries {
		delete(entries, key)
		return
	}
}

func payloadUIDShard(uid uint64) uint64 {
	uid ^= uid >> 30
	uid *= 0xbf58476d1ce4e5b9
	uid ^= uid >> 27
	uid *= 0x94d049bb133111eb
	return (uid ^ (uid >> 31)) & (friendPayloadShardCount - 1)
}

func payloadStringShard(value string) uint64 {
	hash := uint64(1469598103934665603)
	for index := 0; index < len(value); index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}
	return hash & (friendPayloadShardCount - 1)
}
