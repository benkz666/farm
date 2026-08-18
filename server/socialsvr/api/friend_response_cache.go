package api

import (
	"context"
	"sync"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"

	"google.golang.org/protobuf/proto"
)

const (
	// Steal hints are deliberately weak-consistent. A one-second process-local
	// response cache removes a Redis MGET and response rebuild from every
	// FriendList hit while keeping hint staleness tightly bounded.
	friendResponseTTL      = time.Second
	friendResponseShards   = 64
	friendResponseCapacity = 16_384
)

type friendResponseEntry struct {
	value     *cachedCommandResponse
	expiresAt int64
}

type friendResponseCall struct {
	done        chan struct{}
	value       *cachedCommandResponse
	err         error
	invalidated bool
}

type friendResponseShard struct {
	mu        sync.Mutex
	entries   map[uint64]friendResponseEntry
	calls     map[uint64]*friendResponseCall
	positions map[uint64]int
	slots     []uint64
	next      int
}

type friendResponseCache struct {
	shards [friendResponseShards]friendResponseShard
}

type cachedCommandResponse struct {
	message  *publicv3.CommandResponse
	prepared []byte
}

func prepareCommandResponse(message *publicv3.CommandResponse) (*cachedCommandResponse, error) {
	prepared, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	return &cachedCommandResponse{message: message, prepared: prepared}, nil
}

func newFriendResponseCache() *friendResponseCache {
	cache := &friendResponseCache{}
	perShard := friendResponseCapacity / friendResponseShards
	for index := range cache.shards {
		cache.shards[index].entries = make(map[uint64]friendResponseEntry, perShard)
		cache.shards[index].calls = make(map[uint64]*friendResponseCall)
		cache.shards[index].positions = make(map[uint64]int, perShard)
		cache.shards[index].slots = make([]uint64, 0, perShard)
	}
	return cache
}

func friendResponseShardIndex(uid uint64) uint64 {
	uid ^= uid >> 30
	uid *= 0xbf58476d1ce4e5b9
	uid ^= uid >> 27
	uid *= 0x94d049bb133111eb
	return (uid ^ (uid >> 31)) & (friendResponseShards - 1)
}

func (cache *friendResponseCache) get(
	ctx context.Context,
	uid uint64,
	load func() (*cachedCommandResponse, error),
) (*cachedCommandResponse, error) {
	if cache == nil {
		return load()
	}
	shard := &cache.shards[friendResponseShardIndex(uid)]
	now := time.Now().UnixNano()
	shard.mu.Lock()
	if entry, ok := shard.entries[uid]; ok && now < entry.expiresAt {
		shard.mu.Unlock()
		return entry.value, nil
	}
	if call := shard.calls[uid]; call != nil {
		shard.mu.Unlock()
		select {
		case <-call.done:
			return call.value, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &friendResponseCall{done: make(chan struct{})}
	shard.calls[uid] = call
	shard.mu.Unlock()

	call.value, call.err = load()
	shard.mu.Lock()
	delete(shard.calls, uid)
	if call.err == nil && call.value != nil && !call.invalidated {
		shard.put(uid, friendResponseEntry{
			value: call.value, expiresAt: time.Now().Add(friendResponseTTL).UnixNano(),
		})
	}
	close(call.done)
	shard.mu.Unlock()
	return call.value, call.err
}

func (shard *friendResponseShard) put(uid uint64, entry friendResponseEntry) {
	if _, exists := shard.entries[uid]; !exists {
		perShard := friendResponseCapacity / friendResponseShards
		if len(shard.slots) < perShard {
			shard.positions[uid] = len(shard.slots)
			shard.slots = append(shard.slots, uid)
		} else {
			if shard.next >= len(shard.slots) {
				shard.next = 0
			}
			victim := shard.slots[shard.next]
			delete(shard.entries, victim)
			delete(shard.positions, victim)
			shard.slots[shard.next] = uid
			shard.positions[uid] = shard.next
			shard.next++
		}
	}
	shard.entries[uid] = entry
}

func (cache *friendResponseCache) invalidate(uids ...uint64) {
	if cache == nil {
		return
	}
	for _, uid := range uids {
		if uid == 0 {
			continue
		}
		shard := &cache.shards[friendResponseShardIndex(uid)]
		shard.mu.Lock()
		if call := shard.calls[uid]; call != nil {
			// A mutation can race an in-flight cache fill. Mark only this UID's
			// fill stale so unrelated users in the same shard still cache normally.
			call.invalidated = true
		}
		shard.remove(uid)
		shard.mu.Unlock()
	}
}

func (shard *friendResponseShard) remove(uid uint64) {
	position, exists := shard.positions[uid]
	if !exists {
		delete(shard.entries, uid)
		return
	}
	last := len(shard.slots) - 1
	moved := shard.slots[last]
	shard.slots[position] = moved
	shard.positions[moved] = position
	shard.slots = shard.slots[:last]
	delete(shard.positions, uid)
	delete(shard.entries, uid)
	if len(shard.slots) == 0 || shard.next >= len(shard.slots) {
		shard.next = 0
	}
}
