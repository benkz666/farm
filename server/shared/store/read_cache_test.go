package store

import (
	"testing"
	"time"
)

func TestNewConfiguresReadCachesForFormalFixture(t *testing.T) {
	storage := New(nil, nil, 0)
	if defaultReadCacheCapacity < 15_000 {
		t.Fatalf("read cache capacity = %d, does not fit formal fixture", defaultReadCacheCapacity)
	}
	if storage.taskRead.capacity != defaultReadCacheCapacity || storage.taskEncoded.capacity != defaultReadCacheCapacity {
		t.Fatalf(
			"task cache capacities = structured:%d encoded:%d, want %d",
			storage.taskRead.capacity,
			storage.taskEncoded.capacity,
			defaultReadCacheCapacity,
		)
	}
	if storage.mailbox.local.capacity != mailLocalCacheCapacity || storage.mailbox.encoded.capacity != mailLocalCacheCapacity {
		t.Fatalf(
			"mail cache capacities = structured:%d encoded:%d, want %d",
			storage.mailbox.local.capacity,
			storage.mailbox.encoded.capacity,
			mailLocalCacheCapacity,
		)
	}
	if storage.mailbox.local.ttl != mailLocalCacheTTL || storage.mailbox.encoded.ttl != mailLocalCacheTTL {
		t.Fatalf(
			"mail cache TTLs = structured:%s encoded:%s, want %s",
			storage.mailbox.local.ttl,
			storage.mailbox.encoded.ttl,
			mailLocalCacheTTL,
		)
	}
}

func TestInvalidateTaskCacheDropsStructuredAndEncodedViews(t *testing.T) {
	storage := &Store{}
	key := taskReadKey{uid: 42, dayKey: 20260807}
	storage.taskRead.put(key, []Task{{ID: TaskDailyLoginID}}, time.Now())
	storage.taskEncoded.put(key, []byte(`[{"id":4}]`), time.Now())
	storage.invalidateTaskCache(key)
	if tasks, ok := storage.taskRead.get(key, time.Now()); ok {
		t.Fatalf("structured task cache survived invalidation: %#v", tasks)
	}
	if encoded, ok := storage.taskEncoded.get(key, time.Now()); ok {
		t.Fatalf("encoded task cache survived invalidation: %s", encoded)
	}
}

func TestTaskCacheGenerationRejectsStalePut(t *testing.T) {
	storage := &Store{}
	key := taskReadKey{uid: 42, dayKey: 20260807}
	generation := storage.taskCacheGeneration(key)
	storage.invalidateTaskCache(key)
	if storage.putTaskEncodedIfCurrent(key, generation, []byte(`[]`)) {
		t.Fatal("stale encoded task view was cached")
	}
}

func TestTaskCacheStateUsesMoreThanReadCacheShardCount(t *testing.T) {
	seen := make(map[uint64]struct{})
	for uid := uint64(1); uid <= 4096; uid++ {
		seen[taskStateIndex(taskReadKey{uid: uid, dayKey: 20260807})] = struct{}{}
	}
	if len(seen) <= readCacheShardCount {
		t.Fatalf("task generation barriers only used %d shards", len(seen))
	}
}
