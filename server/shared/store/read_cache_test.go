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
	if storage.taskRead.capacity != defaultReadCacheCapacity {
		t.Fatalf("task cache capacity = %d, want %d", storage.taskRead.capacity, defaultReadCacheCapacity)
	}
	if storage.mailbox.local.capacity != mailLocalCacheCapacity {
		t.Fatalf("mail cache capacity = %d, want %d", storage.mailbox.local.capacity, mailLocalCacheCapacity)
	}
	if storage.mailbox.local.ttl != mailLocalCacheTTL {
		t.Fatalf("mail cache TTL = %s, want %s", storage.mailbox.local.ttl, mailLocalCacheTTL)
	}
}

func TestInvalidateTaskCacheDropsStructuredView(t *testing.T) {
	storage := &Store{}
	key := taskReadKey{uid: 42, dayKey: 20260807}
	storage.taskRead.put(key, []Task{{ID: TaskDailyLoginID}}, time.Now())
	storage.invalidateTaskCache(key)
	if tasks, ok := storage.taskRead.get(key, time.Now()); ok {
		t.Fatalf("structured task cache survived invalidation: %#v", tasks)
	}
}

func TestTaskCacheGenerationRejectsStalePut(t *testing.T) {
	storage := &Store{}
	key := taskReadKey{uid: 42, dayKey: 20260807}
	generation := storage.taskCacheGeneration(key)
	storage.invalidateTaskCache(key)
	if storage.putTaskReadIfCurrent(key, generation, []Task{{ID: TaskDailyLoginID}}) {
		t.Fatal("stale task view was cached")
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
