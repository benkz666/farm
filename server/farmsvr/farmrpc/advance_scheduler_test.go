package farmrpc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	"farm/server/shared/presence"
)

func TestFarmAdvanceSchedulerRunsLatestDeadlineOnce(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	scheduler := newFarmAdvanceScheduler(
		func() int64 { return time.Now().UnixMilli() },
		func(uid uint64) {
			if uid != 42 {
				t.Errorf("uid=%d", uid)
			}
			calls.Add(1)
			done <- struct{}{}
		},
	)
	defer scheduler.Close()

	now := time.Now().UnixMilli()
	scheduler.Schedule(42, now+200)
	scheduler.Schedule(42, now+20)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduled advance did not run")
	}
	time.Sleep(230 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("advance calls=%d, want 1", got)
	}
}

func TestFarmAdvanceSchedulerDoesNotDuplicateUnchangedDeadline(t *testing.T) {
	scheduler := newFarmAdvanceScheduler(
		func() int64 { return time.Now().UnixMilli() },
		func(uint64) {},
	)
	defer scheduler.Close()
	due := time.Now().Add(time.Hour).UnixMilli()
	for i := 0; i < 1_000; i++ {
		scheduler.Schedule(42, due)
	}
	scheduler.mu.Lock()
	items := scheduler.items.Len()
	scheduler.mu.Unlock()
	if items != 1 {
		t.Fatalf("heap items=%d, want 1", items)
	}
}

func TestFarmAdvanceSchedulerCompactsChangedDeadlines(t *testing.T) {
	scheduler := newFarmAdvanceScheduler(
		func() int64 { return time.Now().UnixMilli() },
		func(uint64) {},
	)
	defer scheduler.Close()
	base := time.Now().Add(time.Hour).UnixMilli()
	for i := 0; i < 10_000; i++ {
		scheduler.Schedule(42, base+int64(i))
	}
	scheduler.mu.Lock()
	items := scheduler.items.Len()
	live := len(scheduler.deadline)
	scheduler.mu.Unlock()
	if live != 1 {
		t.Fatalf("live deadlines=%d, want 1", live)
	}
	if items > advanceHeapCompactSlack+1 {
		t.Fatalf("heap items=%d, want bounded stale nodes", items)
	}
}

type evictedAdvanceRuntime struct {
	actor *room.FarmActor
	calls atomic.Int32
}

func (r *evictedAdvanceRuntime) IsResident(uint64) bool { return false }

func (r *evictedAdvanceRuntime) Do(_ uint64, fn func(*room.FarmActor) error) error {
	r.calls.Add(1)
	return fn(r.actor)
}

type activeFarmPublisherStub struct{ active bool }

func (p activeFarmPublisherStub) Publish(context.Context, farm.FarmDelta, presence.ConnRef) error {
	return nil
}

func (p activeFarmPublisherStub) HasActiveFarm(context.Context, uint64) (bool, error) {
	return p.active, nil
}

func TestAdvanceScheduledReloadsEvictedActorOnlyForActiveFarm(t *testing.T) {
	for _, test := range []struct {
		name      string
		active    bool
		wantCalls int32
	}{
		{name: "active", active: true, wantCalls: 1},
		{name: "offline", active: false, wantCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &evictedAdvanceRuntime{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}}
			handler := NewHandler(
				runtime,
				nil,
				func(uint64) bool { return true },
				func() int64 { return 123 },
				WithDeltaPublisher(activeFarmPublisherStub{active: test.active}),
			)
			defer handler.Shutdown()
			handler.advanceScheduled(42)
			if got := runtime.calls.Load(); got != test.wantCalls {
				t.Fatalf("runtime calls=%d, want %d", got, test.wantCalls)
			}
		})
	}
}
