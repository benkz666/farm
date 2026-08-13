package room

import (
	"context"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/outbox"
)

type pairBatchCaptureStore struct {
	mu         sync.Mutex
	calls      int
	batchSizes []int
}

func (*pairBatchCaptureStore) SaveFarms(context.Context, []*farm.Aggregate) error { return nil }

func (store *pairBatchCaptureStore) CommitFarms(_ context.Context, commits []outbox.FarmCommit) error {
	store.mu.Lock()
	store.calls++
	store.batchSizes = append(store.batchSizes, len(commits))
	store.mu.Unlock()
	return nil
}

func TestPairCommitterCombinesConcurrentDurableRequests(t *testing.T) {
	store := &pairBatchCaptureStore{}
	committer := newPairCommitter(store, pairCommitterConfig{
		Window: 20 * time.Millisecond, MaxBatch: 32, QueueCap: 64, IOTimeout: time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := committer.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	const requests = 16
	start := make(chan struct{})
	errors := make(chan error, requests)
	var ready sync.WaitGroup
	ready.Add(requests)
	for index := 0; index < requests; index++ {
		go func(index int) {
			ready.Done()
			<-start
			uid := uint64(index*2 + 1)
			errors <- committer.Commit([]outbox.FarmCommit{
				{Snapshot: &farm.Aggregate{UID: uid}},
				{Snapshot: &farm.Aggregate{UID: uid + 1}},
			})
		}(index)
	}
	ready.Wait()
	close(start)
	for range requests {
		if err := <-errors; err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.calls >= requests {
		t.Fatalf("CommitFarms calls = %d, want fewer than %d; batches=%v", store.calls, requests, store.batchSizes)
	}
	total := 0
	largest := 0
	for _, size := range store.batchSizes {
		total += size
		largest = max(largest, size)
	}
	if total != requests*2 || largest <= 2 {
		t.Fatalf("batch sizes = %v, want total %d and at least one combined batch", store.batchSizes, requests*2)
	}
}

func TestPairCommitterRejectsNewWorkAfterShutdown(t *testing.T) {
	committer := newPairCommitter(&pairBatchCaptureStore{}, pairCommitterConfig{IOTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := committer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := committer.Commit([]outbox.FarmCommit{{Snapshot: &farm.Aggregate{UID: 1}}}); err == nil {
		t.Fatal("commit after shutdown succeeded")
	}
}
