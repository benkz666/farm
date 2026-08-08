package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/outbox"
)

type batchRecordingStore struct {
	mu        sync.Mutex
	batches   [][]*farm.Aggregate
	commits   [][]outbox.FarmCommit
	saveDelay time.Duration
	saveErr   error
}

func (s *batchRecordingStore) LoadFarm(_ context.Context, uid uint64) (*farm.Aggregate, error) {
	return farm.NewAggregate(uid, "test"), nil
}

func (s *batchRecordingStore) SaveFarm(ctx context.Context, agg *farm.Aggregate) error {
	return s.SaveFarms(ctx, []*farm.Aggregate{agg})
}

func (s *batchRecordingStore) SaveFarms(_ context.Context, snapshots []*farm.Aggregate) error {
	return s.CommitFarms(context.Background(), outboxCommitsFromSnapshots(snapshots))
}

func (s *batchRecordingStore) CommitFarms(_ context.Context, commits []outbox.FarmCommit) error {
	if s.saveDelay > 0 {
		time.Sleep(s.saveDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	cp := make([]outbox.FarmCommit, len(commits))
	for i, commit := range commits {
		cp[i] = outbox.FarmCommit{
			Snapshot: commit.Snapshot.Clone(),
			Outbox:   append([]outbox.Event(nil), commit.Outbox...),
			Plan:     commit.Plan,
		}
	}
	s.commits = append(s.commits, cp)
	snapshots := make([]*farm.Aggregate, len(commits))
	for i, commit := range commits {
		snapshots[i] = commit.Snapshot
	}
	s.batches = append(s.batches, snapshots)
	return nil
}

func outboxCommitsFromSnapshots(snapshots []*farm.Aggregate) []outbox.FarmCommit {
	commits := make([]outbox.FarmCommit, len(snapshots))
	for i, snap := range snapshots {
		commits[i] = outbox.FarmCommit{Snapshot: snap}
	}
	return commits
}

func (s *batchRecordingStore) lastOutboxCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commits) == 0 {
		return 0
	}
	return len(s.commits[len(s.commits)-1][0].Outbox)
}

func (s *batchRecordingStore) lastPlan() outbox.PersistPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commits) == 0 || len(s.commits[len(s.commits)-1]) == 0 {
		return outbox.PersistPlan{}
	}
	return s.commits[len(s.commits)-1][0].Plan
}

func (s *batchRecordingStore) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func (s *batchRecordingStore) lastBatchSize() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return 0
	}
	return len(s.batches[len(s.batches)-1])
}

func TestCommitterBatchesAcrossUIDs(t *testing.T) {
	store := &batchRecordingStore{}
	cfg := defaultCommitterConfig()
	cfg.Window = 5 * time.Millisecond
	committer := NewCommitter(store, cfg)

	chs := make([]<-chan CommitResult, 0, 3)
	for uid := uint64(1); uid <= 3; uid++ {
		agg := farm.NewAggregate(uid, "u")
		resultCh, err := committer.Enqueue(uid, 1, agg.Clone(), nil, false)
		if err != nil {
			t.Fatalf("Enqueue uid %d: %v", uid, err)
		}
		chs = append(chs, resultCh)
	}
	for i, ch := range chs {
		res := <-ch
		if res.Err != nil {
			t.Fatalf("commit uid %d: %v", i+1, res.Err)
		}
	}

	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("no batch committed")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := store.lastBatchSize(); got != 3 {
		t.Fatalf("batch size = %d, want 3", got)
	}
	if err := committer.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestCommitterCarriesSpecializedPlan(t *testing.T) {
	storage := &batchRecordingStore{}
	committer := NewCommitter(storage, defaultCommitterConfig())
	aggregate := farm.NewAggregate(7, "seven")
	plan := outbox.PersistPlan{Mode: outbox.PersistCrossVisitor, IncludeItems: true}
	result, err := committer.EnqueuePlan(7, 1, aggregate, nil, plan, true)
	if err != nil {
		t.Fatalf("EnqueuePlan: %v", err)
	}
	if committed := <-result; committed.Err != nil {
		t.Fatalf("commit: %v", committed.Err)
	}
	if got := storage.lastPlan(); got != plan {
		t.Fatalf("persist plan = %#v, want %#v", got, plan)
	}
	if err := committer.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestCommitterCoalescesSameUID(t *testing.T) {
	store := &batchRecordingStore{}
	cfg := defaultCommitterConfig()
	cfg.Window = 10 * time.Millisecond
	committer := NewCommitter(store, cfg)

	agg1 := farm.NewAggregate(9, "nine")
	agg1.Coin = 100
	ch1, err := committer.Enqueue(9, 1, agg1.Clone(), nil, true)
	if err != nil {
		t.Fatalf("Enqueue gen1: %v", err)
	}

	agg2 := agg1.Clone()
	agg2.Coin = 200
	ch2, err := committer.Enqueue(9, 2, agg2.Clone(), nil, true)
	if err != nil {
		t.Fatalf("Enqueue gen2: %v", err)
	}

	res1 := <-ch1
	res2 := <-ch2
	if res1.Err != nil || res2.Err != nil {
		t.Fatalf("commit errors: %v / %v", res1.Err, res2.Err)
	}
	if res1.Generation != 2 || res2.Generation != 2 {
		t.Fatalf("generations = %d / %d, want 2", res1.Generation, res2.Generation)
	}

	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("no batch committed")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := store.lastBatchSize(); got != 1 {
		t.Fatalf("batch size = %d, want 1", got)
	}
	store.mu.Lock()
	coin := store.batches[0][0].Coin
	store.mu.Unlock()
	if coin != 200 {
		t.Fatalf("persisted coin = %d, want 200", coin)
	}
	if err := committer.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestCommitterRetriesAfterFailure(t *testing.T) {
	store := &batchRecordingStore{saveErr: errors.New("db down")}
	cfg := defaultCommitterConfig()
	cfg.Window = 2 * time.Millisecond
	cfg.MinBackoff = 5 * time.Millisecond
	committer := NewCommitter(store, cfg)

	agg := farm.NewAggregate(5, "fail")
	ch, err := committer.Enqueue(5, 1, agg.Clone(), nil, true)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	res := <-ch
	if res.Err == nil {
		t.Fatal("expected save error")
	}
	if !errors.Is(res.Err, store.saveErr) {
		t.Fatalf("error = %v, want %v", res.Err, store.saveErr)
	}
	if err := committer.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestCommitterCarriesOutboxEventsInBatch(t *testing.T) {
	store := &batchRecordingStore{}
	committer := NewCommitter(store, defaultCommitterConfig())
	event := outbox.Event{EventID: "e1", TargetUID: 1, Kind: outbox.KindCrossResult, Payload: []byte("x")}
	agg := farm.NewAggregate(1, "one")
	ch, err := committer.Enqueue(1, 1, agg.Clone(), []outbox.Event{event}, true)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res := <-ch; res.Err != nil {
		t.Fatalf("commit: %v", res.Err)
	}
	if got := store.lastOutboxCount(); got != 1 {
		t.Fatalf("outbox count = %d, want 1", got)
	}
	if err := committer.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestRuntimeSyncFlushClearsAckedOutboxEvents(t *testing.T) {
	store := &batchRecordingStore{}
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(time.Hour)

	if err := runtime.Do(88, func(a *FarmActor) error {
		a.Aggregate.Coin = 1
		a.RecordOutbox(outbox.Event{
			EventID:   "cross_result:9:7:1",
			TargetUID: 7,
			Kind:      outbox.KindCrossResult,
			Payload:   []byte("payload"),
		})
		a.RequireFlush()
		return nil
	}); err != nil {
		t.Fatalf("first Do: %v", err)
	}

	if err := runtime.Do(88, func(a *FarmActor) error {
		if pending := len(a.pendingOutboxEvents()); pending != 0 {
			t.Fatalf("pending outbox after durable ack = %d", pending)
		}
		return nil
	}); err != nil {
		t.Fatalf("second Do: %v", err)
	}
}

func TestRuntimeConcurrentSyncFlushOutboxNoRace(t *testing.T) {
	store := &batchRecordingStore{saveDelay: 50 * time.Millisecond}
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = runtime.Do(99, func(a *FarmActor) error {
				a.Aggregate.Coin += int64(n)
				a.RecordOutbox(outbox.Event{
					EventID:   fmt.Sprintf("cross_result:9:7:%d", n),
					TargetUID: 7,
					Kind:      outbox.KindCrossResult,
					Payload:   []byte("x"),
				})
				a.RequireFlush()
				return nil
			})
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestRuntimeConcurrentSyncFlushDoesNotBlockMailbox(t *testing.T) {
	base := newMemoryFarmStore(farm.NewAggregate(60, "slow-owner"))
	slow := &slowMemoryFarmStore{memoryFarmStore: base, delay: 200 * time.Millisecond}
	runtime := NewRuntime(slow, time.Hour)
	runtime.SetFlushInterval(time.Hour)

	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_ = runtime.Do(60, func(actor *FarmActor) error {
				actor.Aggregate.Coin++
				actor.RequireFlush()
				return nil
			})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent owner apply blocked mailbox")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestRuntimeMailboxNotBlockedBySlowSaveFarms(t *testing.T) {
	base := newMemoryFarmStore(farm.NewAggregate(50, "slow"))
	slow := &slowMemoryFarmStore{memoryFarmStore: base, delay: 200 * time.Millisecond}
	runtime := NewRuntime(slow, time.Hour)
	runtime.SetFlushInterval(time.Hour)

	if err := runtime.Do(50, func(actor *FarmActor) error {
		actor.Aggregate.Coin++
		actor.RequireFlush()
		return nil
	}); err != nil {
		t.Fatalf("first Do: %v", err)
	}

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runtime.Do(50, func(actor *FarmActor) error {
			close(secondEntered)
			actor.Aggregate.Coin++
			return nil
		})
	}()

	select {
	case <-secondEntered:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second callback blocked while SaveFarms is slow")
	}

	if err := <-secondDone; err != nil {
		t.Fatalf("second Do: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

type slowMemoryFarmStore struct {
	*memoryFarmStore
	delay time.Duration
}

func (s *slowMemoryFarmStore) SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error {
	time.Sleep(s.delay)
	return s.memoryFarmStore.SaveFarms(ctx, snapshots)
}
