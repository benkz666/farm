package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"farm/server/shared/store"
)

type recordingStealHints struct {
	mu      sync.Mutex
	values  map[uint64]bool
	batches int
}

func (fake *recordingStealHints) SetStealHint(_ context.Context, uid uint64, value bool) error {
	return fake.SetStealHints(context.Background(), map[uint64]bool{uid: value})
}

func (fake *recordingStealHints) SetStealHints(_ context.Context, values map[uint64]bool) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.values == nil {
		fake.values = make(map[uint64]bool)
	}
	for uid, value := range values {
		fake.values[uid] = value
	}
	fake.batches++
	return nil
}

func (fake *recordingStealHints) GetStealHints(_ context.Context, uids []uint64) (map[uint64]bool, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	result := make(map[uint64]bool, len(uids))
	for _, uid := range uids {
		if value, ok := fake.values[uid]; ok {
			result[uid] = value
		}
	}
	return result, nil
}

func TestAsyncStealHintStoreCoalescesLatestValues(t *testing.T) {
	base := &recordingStealHints{}
	writer := store.NewAsyncStealHintStore(base)
	for i := 0; i < 100; i++ {
		if err := writer.SetStealHint(t.Context(), 42, i%2 == 0); err != nil {
			t.Fatalf("SetStealHint: %v", err)
		}
	}
	if err := writer.SetStealHint(t.Context(), 42, true); err != nil {
		t.Fatalf("SetStealHint final: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := writer.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	base.mu.Lock()
	defer base.mu.Unlock()
	if !base.values[42] {
		t.Fatalf("value=%v, want final true", base.values[42])
	}
	if base.batches <= 0 || base.batches >= 101 {
		t.Fatalf("batches=%d, want coalesced writes", base.batches)
	}
}
