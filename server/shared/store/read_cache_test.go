package store

import (
	"testing"
	"time"
)

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
