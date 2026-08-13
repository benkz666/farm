package room

import (
	"context"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/outbox"
)

type pairCommitStore struct {
	mu          sync.Mutex
	aggregates  map[uint64]*farm.Aggregate
	commitCalls int
	batchSizes  []int
}

func (store *pairCommitStore) LoadFarm(_ context.Context, uid uint64) (*farm.Aggregate, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.aggregates[uid].Clone(), nil
}

func (store *pairCommitStore) SaveFarm(ctx context.Context, aggregate *farm.Aggregate) error {
	return store.SaveFarms(ctx, []*farm.Aggregate{aggregate})
}

func (store *pairCommitStore) SaveFarms(_ context.Context, snapshots []*farm.Aggregate) error {
	commits := make([]outbox.FarmCommit, 0, len(snapshots))
	for _, snapshot := range snapshots {
		commits = append(commits, outbox.FarmCommit{Snapshot: snapshot})
	}
	return store.CommitFarms(context.Background(), commits)
}

func (store *pairCommitStore) CommitFarms(_ context.Context, commits []outbox.FarmCommit) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.commitCalls++
	store.batchSizes = append(store.batchSizes, len(commits))
	for _, commit := range commits {
		if commit.Snapshot != nil {
			store.aggregates[commit.Snapshot.UID] = commit.Snapshot.Clone()
		}
	}
	return nil
}

func TestRuntimeDoPairDurableCommitsBothUIDsOnce(t *testing.T) {
	first := farm.NewAggregate(7, "visitor")
	second := farm.NewAggregate(9, "owner")
	store := &pairCommitStore{aggregates: map[uint64]*farm.Aggregate{7: first, 9: second}}
	runtime := NewRuntime(store, time.Hour)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = runtime.Shutdown(ctx)
	})

	if err := runtime.DoPairDurable(7, 9, func(visitor, owner *FarmActor) error {
		visitor.Aggregate.Coin += 10
		visitor.RequireEconomyFlush()
		owner.Aggregate.Coin += 20
		owner.RequireEconomyFlush()
		return nil
	}); err != nil {
		t.Fatalf("DoPairDurable: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.commitCalls != 1 || len(store.batchSizes) != 1 || store.batchSizes[0] != 2 {
		t.Fatalf("commit calls=%d batches=%v, want one two-UID commit", store.commitCalls, store.batchSizes)
	}
	if got := store.aggregates[7].Coin; got != first.Coin+10 {
		t.Fatalf("visitor coin=%d, want %d", got, first.Coin+10)
	}
	if got := store.aggregates[9].Coin; got != second.Coin+20 {
		t.Fatalf("owner coin=%d, want %d", got, second.Coin+20)
	}
}
