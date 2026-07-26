package actor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"farm/server/internal/farm"
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
			return nil
		})
	}()

	<-firstEntered
	go func() {
		close(secondStarted)
		errs <- runtime.Do(42, func(actor *FarmActor) error {
			actor.Aggregate.Coin++
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

func TestRuntimeFlushesAndUnloadsIdleActor(t *testing.T) {
	store := newMemoryFarmStore(farm.NewAggregate(7, "bob"))
	runtime := NewRuntime(store, 10*time.Millisecond)

	if err := runtime.Do(7, func(actor *FarmActor) error {
		actor.Aggregate.Coin++
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

type memoryFarmStore struct {
	mu        sync.Mutex
	aggregate *farm.Aggregate
	loadCount int
	loadErr   error
	saved     chan struct{}
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

func (s *memoryFarmStore) SaveFarm(_ context.Context, aggregate *farm.Aggregate) error {
	s.mu.Lock()
	s.aggregate = aggregate
	s.mu.Unlock()

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
