package room

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"farm/server/domain/farm"
)

func TestRuntimeSerializesConcurrentCallsForSameUID(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(42, "alice"))
	runtime := NewRuntime(store, time.Hour)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	secondRan := make(chan struct{})
	errs := make(chan error, 2)

	go func() {
		errs <- runtime.Do(42, func(actor *FarmActor) error {
			close(firstEntered)
			<-releaseFirst
			actor.Aggregate.Coin++
			actor.MarkDirty()
			return nil
		})
	}()

	<-firstEntered
	go func() {
		close(secondStarted)
		errs <- runtime.Do(42, func(actor *FarmActor) error {
			actor.Aggregate.Coin++
			actor.MarkDirty()
			close(secondRan)
			return nil
		})
	}()

	<-secondStarted
	select {
	case <-secondRan:
		t.Fatal("second callback ran before the first callback completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Runtime.Do: %v", err)
		}
	}

	if got, want := store.aggregate.Coin, int64(1002); got != want {
		t.Fatalf("coin = %d, want %d", got, want)
	}
	if got, want := store.loadCalls(), 1; got != want {
		t.Fatalf("LoadFarm calls = %d, want %d", got, want)
	}
}

func TestRuntimeRejectsColdLoadAtResidentCapacity(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(42, "alice"))
	runtime := NewRuntime(store, time.Hour)
	runtime.SetMaxResident(1)

	if err := runtime.Do(42, func(*FarmActor) error { return nil }); err != nil {
		t.Fatalf("first Runtime.Do: %v", err)
	}
	if err := runtime.Do(43, func(*FarmActor) error { return nil }); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second UID error = %v, want ErrCapacity", err)
	}
	if err := runtime.Do(42, func(*FarmActor) error { return nil }); err != nil {
		t.Fatalf("resident UID was rejected: %v", err)
	}
}

func TestRuntimeFlushesAndUnloadsIdleActor(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(7, "bob"))
	runtime := NewRuntime(store, 10*time.Millisecond)

	if err := runtime.Do(7, func(actor *FarmActor) error {
		actor.Aggregate.Coin++
		actor.MarkDirty()
		return nil
	}); err != nil {
		t.Fatalf("first Runtime.Do: %v", err)
	}

	select {
	case <-store.saved:
	case <-time.After(time.Second):
		t.Fatal("idle actor was not flushed")
	}

	if err := runtime.Do(7, func(*FarmActor) error { return nil }); err != nil {
		t.Fatalf("second Runtime.Do: %v", err)
	}
	if got, want := store.loadCalls(), 2; got != want {
		t.Fatalf("LoadFarm calls after idle unload = %d, want %d", got, want)
	}
}

func TestRuntimeRecoversCallbackPanicAndUnblocksCaller(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(11, "dave"))
	runtime := NewRuntime(store, time.Hour)

	errCh := make(chan error, 1)
	go func() {
		errCh <- runtime.Do(11, func(*FarmActor) error {
			panic("boom")
		})
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Runtime.Do returned nil after callback panic")
		}
		if got := err.Error(); !strings.Contains(got, "callback panic") || !strings.Contains(got, "boom") {
			t.Fatalf("Runtime.Do error = %v, want wrapped panic", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller blocked after callback panic")
	}

	if err := runtime.Do(11, func(actor *FarmActor) error {
		actor.Aggregate.Coin++
		actor.MarkDirty()
		return nil
	}); err != nil {
		t.Fatalf("Runtime.Do after panic unload: %v", err)
	}
	if got, want := store.loadCalls(), 2; got != want {
		t.Fatalf("LoadFarm calls after panic unload = %d, want %d", got, want)
	}
	if got, want := store.aggregate.Coin, int64(1001); got != want {
		t.Fatalf("coin after rebuild = %d, want %d", got, want)
	}
}

func TestRuntimeReturnsLoadErrorAndUnloadsActor(t *testing.T) {
	loadErr := errors.New("farm unavailable")
	store := newMemoryFarmStore(farm.NewAggregate(9, "carol"))
	store.loadErr = loadErr
	runtime := NewRuntime(store, time.Hour)

	callbackRan := false
	err := runtime.Do(9, func(*FarmActor) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, loadErr) {
		t.Fatalf("Runtime.Do error = %v, want %v", err, loadErr)
	}
	if callbackRan {
		t.Fatal("callback ran after LoadFarm failed")
	}

	store.mu.Lock()
	store.loadErr = nil
	store.mu.Unlock()
	if err := runtime.Do(9, func(*FarmActor) error { return nil }); err != nil {
		t.Fatalf("Runtime.Do after load failure: %v", err)
	}
	if got, want := store.loadCalls(), 2; got != want {
		t.Fatalf("LoadFarm calls after failed load = %d, want %d", got, want)
	}
}

func TestRuntimeCoalescesConcurrentColdLoads(t *testing.T) {
	store := &batchLoadFarmStore{memoryFarmStore: *newMemoryFarmStore(farm.NewAggregate(1, "batch"))}
	runtime := NewRuntime(store, time.Hour)
	const requests = 64
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(uid uint64) {
			defer wait.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			agg, err := runtime.loadAggregate(ctx, uid)
			if err == nil && (agg == nil || agg.UID != uid) {
				err = errors.New("batch loader returned the wrong aggregate")
			}
			errs <- err
		}(uint64(index + 1))
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	batchCalls, largestBatch := store.batchCalls, store.largestBatch
	store.mu.Unlock()
	if batchCalls >= requests || largestBatch <= 1 {
		t.Fatalf("cold loads were not coalesced: calls=%d largest=%d", batchCalls, largestBatch)
	}
}

// 只在空闲时落盘是不够的：在线玩家的 Actor 永不空闲，所以写回周期到点也必须落盘，
// 否则被强杀时丢掉的是整段在线期间的改动。
func TestRuntimeFlushesDirtyActorOnFlushIntervalWithoutIdling(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(21, "erin"))
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(20 * time.Millisecond)

	if err := runtime.Do(21, func(actor *FarmActor) error {
		actor.Aggregate.Coin += 5
		actor.MarkDirty()
		return nil
	}); err != nil {
		t.Fatalf("Runtime.Do: %v", err)
	}

	select {
	case <-store.saved:
	case <-time.After(2 * time.Second):
		t.Fatal("dirty actor was not flushed on the write-behind interval")
	}
	if got, want := store.coin(), int64(1005); got != want {
		t.Fatalf("persisted coin = %d, want %d", got, want)
	}
	// Actor 仍应驻留，写回不等于卸载。
	if err := runtime.Do(21, func(*FarmActor) error { return nil }); err != nil {
		t.Fatalf("Runtime.Do after flush: %v", err)
	}
	if got, want := store.loadCalls(), 1; got != want {
		t.Fatalf("LoadFarm calls after flush = %d, want %d", got, want)
	}
}

func TestRuntimeShutdownFlushesResidentActors(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(31, "frank"))
	// 空闲与写回都设成远期，确保落盘只可能来自疏散。
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(time.Hour)

	if err := runtime.Do(31, func(actor *FarmActor) error {
		actor.Aggregate.Coin += 7
		actor.MarkDirty()
		return nil
	}); err != nil {
		t.Fatalf("Runtime.Do: %v", err)
	}
	if got := store.saveCalls(); got != 0 {
		t.Fatalf("SaveFarm calls before shutdown = %d, want 0", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Runtime.Shutdown: %v", err)
	}

	if got, want := store.saveCalls(), 1; got != want {
		t.Fatalf("SaveFarm calls after shutdown = %d, want %d", got, want)
	}
	if got, want := store.coin(), int64(1007); got != want {
		t.Fatalf("persisted coin = %d, want %d", got, want)
	}
}

func TestRuntimeShutdownRetriesTransientFlushFailure(t *testing.T) {
	saveErr := errors.New("mysql temporarily unavailable")
	store := newMemoryFarmStore(farm.NewAggregate(35, "retry-drain"))
	store.transientSaveFailures = 1
	store.transientSaveErr = saveErr
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(time.Hour)

	if err := runtime.Do(35, func(actor *FarmActor) error {
		actor.Aggregate.Coin += 9
		actor.MarkDirty()
		return nil
	}); err != nil {
		t.Fatalf("Runtime.Do: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Runtime.Shutdown: %v", err)
	}
	if got, want := store.saveCalls(), 2; got != want {
		t.Fatalf("SaveFarm calls after transient failure = %d, want %d", got, want)
	}
	if got, want := store.coin(), int64(1009); got != want {
		t.Fatalf("persisted coin = %d, want %d", got, want)
	}
}

func TestRuntimeRejectsCallsAfterShutdown(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(32, "grace"))
	runtime := NewRuntime(store, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Runtime.Shutdown: %v", err)
	}

	callbackRan := false
	err := runtime.Do(32, func(*FarmActor) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, ErrDraining) {
		t.Fatalf("Runtime.Do after shutdown = %v, want ErrDraining", err)
	}
	if callbackRan {
		t.Fatal("callback ran after shutdown")
	}
}

// A 档语义：购买这类玩家已付出代价的操作必须落盘成功才算成功。
func TestRuntimeRequireFlushReportsStoreFailure(t *testing.T) {
	saveErr := errors.New("mysql unavailable")
	store := newMemoryFarmStore(farm.NewAggregate(33, "henry"))
	store.saveErr = saveErr
	runtime := NewRuntime(store, time.Hour)

	err := runtime.Do(33, func(actor *FarmActor) error {
		actor.Aggregate.Coin -= 100
		actor.MarkDirty()
		actor.RequireFlush()
		return nil
	})

	if !errors.Is(err, saveErr) {
		t.Fatalf("Runtime.Do error = %v, want %v", err, saveErr)
	}
}

// Do 的超时只覆盖「请求尚未被接收」的阶段，所以返回 ErrBusy 时保证没有副作用。
func TestRuntimeBusyActorShedsCallWithoutRunningCallback(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(34, "iris"))
	runtime := NewRuntime(store, time.Hour)
	runtime.SetTimeouts(20*time.Millisecond, time.Second)

	entered := make(chan struct{})
	release := make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		blocked <- runtime.Do(34, func(*FarmActor) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	shedRan := false
	err := runtime.Do(34, func(*FarmActor) error {
		shedRan = true
		return nil
	})

	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Runtime.Do on busy actor = %v, want ErrBusy", err)
	}
	if shedRan {
		t.Fatal("shed callback must not run: ErrBusy promises no side effects")
	}

	close(release)
	if err := <-blocked; err != nil {
		t.Fatalf("blocking Runtime.Do: %v", err)
	}
}

func TestResultCacheEvictsOldestBeyondCapacity(t *testing.T) {
	var actor FarmActor
	for reqID := uint64(1); reqID <= resultCacheCapacity+1; reqID++ {
		actor.CacheResult(reqID, reqID)
	}

	if _, ok := actor.CachedResult(1); ok {
		t.Fatal("oldest entry must be evicted once capacity is exceeded")
	}
	for reqID := uint64(2); reqID <= resultCacheCapacity+1; reqID++ {
		value, ok := actor.CachedResult(reqID)
		if !ok || value != reqID {
			t.Fatalf("cached result for %d = %v ok=%v", reqID, value, ok)
		}
	}
}

func TestRuntimeInjectsHazardSaltOnLoad(t *testing.T) {
	agg := farm.NewAggregate(3, "salt")
	if agg.HazardSalt != 0 {
		t.Fatalf("fresh aggregate HazardSalt = %d, want 0", agg.HazardSalt)
	}
	store := newMemoryFarmStore(agg)
	runtime := NewRuntime(store, time.Hour)
	want := farm.DeriveHazardSalt("runtime-inject-secret")
	runtime.SetHazardSalt(want)

	var got uint64
	if err := runtime.Do(3, func(actor *FarmActor) error {
		got = actor.Aggregate.HazardSalt
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got != want {
		t.Fatalf("injected HazardSalt = %d, want %d", got, want)
	}
}

func TestRuntimeReadOnlySkipsWriteBehind(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(55, "reader"))
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(15 * time.Millisecond)

	for i := 0; i < 5; i++ {
		if err := runtime.Do(55, func(actor *FarmActor) error {
			_ = actor.Aggregate.Coin
			_ = actor.Aggregate.CodexSnapshot()
			return nil
		}); err != nil {
			t.Fatalf("read-only Do %d: %v", i, err)
		}
	}

	time.Sleep(80 * time.Millisecond)
	if got := store.saveCalls(); got != 0 {
		t.Fatalf("SaveFarms calls after read-only traffic = %d, want 0", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := store.saveCalls(); got != 0 {
		t.Fatalf("SaveFarms calls after shutdown = %d, want 0", got)
	}
}

func TestRuntimeMarkDirtyWriteBehind(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(56, "writer"))
	runtime := NewRuntime(store, time.Hour)
	runtime.SetFlushInterval(15 * time.Millisecond)

	if err := runtime.Do(56, func(actor *FarmActor) error {
		actor.Aggregate.Coin += 3
		actor.MarkDirty()
		return nil
	}); err != nil {
		t.Fatalf("Runtime.Do: %v", err)
	}

	select {
	case <-store.saved:
	case <-time.After(2 * time.Second):
		t.Fatal("dirty actor was not flushed on write-behind interval")
	}
	if got, want := store.coin(), int64(1003); got != want {
		t.Fatalf("persisted coin = %d, want %d", got, want)
	}
}

func TestRuntimeSameSecretYieldsSameSaltAcrossInstances(t *testing.T) {
	secret := "shared-hazard-secret"
	want := farm.DeriveHazardSalt(secret)

	readSalt := func() uint64 {
		runtime := NewRuntime(newMemoryFarmStore(farm.NewAggregate(9, "x")), time.Hour)
		runtime.SetHazardSalt(want)
		var got uint64
		if err := runtime.Do(9, func(actor *FarmActor) error {
			got = actor.Aggregate.HazardSalt
			return nil
		}); err != nil {
			t.Fatalf("Do: %v", err)
		}
		return got
	}

	if a, b := readSalt(), readSalt(); a != b || a != want {
		t.Fatalf("salts = %d / %d, want %d", a, b, want)
	}

	other := NewRuntime(newMemoryFarmStore(farm.NewAggregate(9, "x")), time.Hour)
	other.SetHazardSalt(farm.DeriveHazardSalt("different-hazard-secret"))
	var otherSalt uint64
	if err := other.Do(9, func(actor *FarmActor) error {
		otherSalt = actor.Aggregate.HazardSalt
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if otherSalt == want {
		t.Fatal("different secrets must not inject the same salt")
	}
}

type memoryFarmStore struct {
	mu                    sync.Mutex
	aggregate             *farm.Aggregate
	loadCount             int
	saveCount             int
	loadErr               error
	saveErr               error
	transientSaveFailures int
	transientSaveErr      error
	saved                 chan struct{}
}

type batchLoadFarmStore struct {
	memoryFarmStore
	batchCalls   int
	largestBatch int
}

func (s *batchLoadFarmStore) LoadFarms(_ context.Context, uids []uint64) (map[uint64]*farm.Aggregate, error) {
	s.mu.Lock()
	s.batchCalls++
	if len(uids) > s.largestBatch {
		s.largestBatch = len(uids)
	}
	s.mu.Unlock()
	loaded := make(map[uint64]*farm.Aggregate, len(uids))
	for _, uid := range uids {
		loaded[uid] = farm.NewAggregate(uid, "batch")
	}
	return loaded, nil
}

func newMemoryFarmStore(aggregate *farm.Aggregate) *memoryFarmStore {
	return &memoryFarmStore{
		aggregate: aggregate,
		saved:     make(chan struct{}, 1),
	}
}

func (s *memoryFarmStore) LoadFarm(context.Context, uint64) (*farm.Aggregate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadCount++
	return s.aggregate, s.loadErr
}

func (s *memoryFarmStore) SaveFarm(ctx context.Context, aggregate *farm.Aggregate) error {
	return s.SaveFarms(ctx, []*farm.Aggregate{aggregate})
}

func (s *memoryFarmStore) SaveFarms(_ context.Context, snapshots []*farm.Aggregate) error {
	s.mu.Lock()
	s.saveCount++
	saveErr := s.saveErr
	if s.transientSaveFailures > 0 {
		s.transientSaveFailures--
		saveErr = s.transientSaveErr
	}
	if saveErr == nil && len(snapshots) > 0 {
		s.aggregate = snapshots[len(snapshots)-1]
	}
	s.mu.Unlock()
	if saveErr != nil {
		return saveErr
	}

	select {
	case s.saved <- struct{}{}:
	default:
	}
	return nil
}

func (s *memoryFarmStore) loadCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadCount
}

func (s *memoryFarmStore) saveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveCount
}

func (s *memoryFarmStore) coin() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aggregate.Coin
}
