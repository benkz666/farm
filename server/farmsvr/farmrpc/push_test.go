package farmrpc

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	"farm/server/shared/store"
)

func TestFanoutPublisherPushesEverySubscribedConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 8, "gateway-1"); err != nil {
		t.Fatalf("Subscribe first connection: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 9, "gateway-0"); err != nil {
		t.Fatalf("Subscribe second connection: %v", err)
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}

	if err := publisher.Publish(t.Context(), delta, presence.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := connIDsByGateway(pusher.batches())
	want := map[string][]uint64{
		"gateway-1": {8},
		"gateway-0": {9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches by gateway = %#v, want %#v", got, want)
	}
}

func TestFanoutPublisherSkipsOriginatingConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-1"); err != nil {
		t.Fatalf("Subscribe first connection: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 1, "gateway-0"); err != nil {
		t.Fatalf("Subscribe originator connection: %v", err)
	}
	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}

	if err := publisher.Publish(t.Context(), delta, presence.ConnRef{ConnID: 1, GatewayID: "gateway-0"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := connIDsByGateway(pusher.batches())
	want := map[string][]uint64{"gateway-1": {1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches by gateway = %#v, want %#v", got, want)
	}
}

func TestFanoutPublisherSkipsExpiredRoomSubscribers(t *testing.T) {
	var nowMs int64 = 1_000_000
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend,
		presence.WithClock(func() time.Time { return time.UnixMilli(nowMs) }),
		presence.WithLeaseTTL(time.Minute),
	)
	if err := registry.Subscribe(t.Context(), 42, 8, "gateway-1"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	nowMs += int64(time.Minute / time.Millisecond)

	pusher := &recordingBatchPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	if err := publisher.Publish(t.Context(), farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}, presence.ConnRef{}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := connIDsByGateway(pusher.batches()); len(got) != 0 {
		t.Fatalf("expired subscriber still fanned out: %#v", got)
	}
}

func TestTaskFanoutPublisherPushesActivePlayerConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	ref := presence.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingTaskNotifyPusher{published: make(chan struct{}, 1)}
	publisher := newTaskFanoutPublisher(registry, pusher, 5*time.Millisecond)
	task := store.Task{
		ID: 1, DayKey: 20260731, Title: "完成一次播种", Progress: 1, Target: 1, RewardCoin: 20,
	}

	if err := publisher.PublishTaskNotify(t.Context(), 42, task); err != nil {
		t.Fatalf("PublishTaskNotify: %v", err)
	}
	select {
	case <-pusher.published:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced TaskNotify")
	}

	if got := pusher.notifications(); !reflect.DeepEqual(got, []pushedTaskNotify{
		{ref: presence.ConnRef{ConnID: 8, GatewayID: "gateway-1"}, uid: 42, task: task},
	}) {
		t.Fatalf("task notifications = %#v", got)
	}
}

func TestTaskFanoutPublisherCoalescesLatestStateForSameTask(t *testing.T) {
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	ref := presence.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingTaskNotifyPusher{published: make(chan struct{}, 2)}
	publisher := newTaskFanoutPublisher(registry, pusher, 10*time.Millisecond)
	first := store.Task{ID: 5, DayKey: 20260731, Progress: 1, Target: 10}
	latest := store.Task{ID: 5, DayKey: 20260731, Progress: 3, Target: 10}

	if err := publisher.PublishTaskNotify(t.Context(), 42, first); err != nil {
		t.Fatalf("first PublishTaskNotify: %v", err)
	}
	if err := publisher.PublishTaskNotify(t.Context(), 42, latest); err != nil {
		t.Fatalf("latest PublishTaskNotify: %v", err)
	}
	select {
	case <-pusher.published:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for coalesced TaskNotify")
	}
	if got := pusher.notifications(); !reflect.DeepEqual(got, []pushedTaskNotify{
		{ref: ref, uid: 42, task: latest},
	}) {
		t.Fatalf("task notifications = %#v, want latest only", got)
	}
	select {
	case <-pusher.published:
		t.Fatal("received duplicate TaskNotify after coalescing")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestTaskFanoutPublisherDoesNotCoalesceAcrossLocalDays(t *testing.T) {
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	ref := presence.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingTaskNotifyPusher{published: make(chan struct{}, 2)}
	publisher := newTaskFanoutPublisher(registry, pusher, 5*time.Millisecond)
	oldDay := store.Task{ID: 5, DayKey: 20260731, Progress: 9, Target: 10}
	newDay := store.Task{ID: 5, DayKey: 20260801, Progress: 1, Target: 10}

	if err := publisher.PublishTaskNotify(t.Context(), 42, oldDay); err != nil {
		t.Fatalf("old-day PublishTaskNotify: %v", err)
	}
	if err := publisher.PublishTaskNotify(t.Context(), 42, newDay); err != nil {
		t.Fatalf("new-day PublishTaskNotify: %v", err)
	}
	for range 2 {
		select {
		case <-pusher.published:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for cross-day TaskNotify")
		}
	}
	got := pusher.notifications()
	sort.Slice(got, func(i, j int) bool { return got[i].task.DayKey < got[j].task.DayKey })
	if !reflect.DeepEqual(got, []pushedTaskNotify{
		{ref: ref, uid: 42, task: oldDay},
		{ref: ref, uid: 42, task: newDay},
	}) {
		t.Fatalf("cross-day notifications = %#v", got)
	}
}

func TestTaskFanoutPublisherDropsOverflowWithoutDirectDelivery(t *testing.T) {
	registry := presence.NewWithBackend(newRegistryBackend())
	pusher := &recordingTaskNotifyPusher{}
	publisher := newTaskFanoutPublisher(registry, pusher, time.Hour)
	for index := 0; index < maxPendingTaskNotifies; index++ {
		key := taskNotifyKey{uid: uint64(index + 1), dayKey: 20260731, taskID: store.TaskWaterID}
		publisher.pending[key] = pendingTaskNotify{
			uid:  key.uid,
			task: store.Task{ID: key.taskID, DayKey: key.dayKey},
		}
	}

	if err := publisher.PublishTaskNotify(t.Context(), 999999, store.Task{
		ID: store.TaskPlantID, DayKey: 20260731,
	}); err != nil {
		t.Fatalf("overflow PublishTaskNotify: %v", err)
	}
	if got := publisher.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	if got := pusher.notifications(); len(got) != 0 {
		t.Fatalf("overflow performed direct delivery: %#v", got)
	}
}

func TestMailFanoutPublisherPushesActivePlayerConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := presence.NewWithBackend(backend)
	ref := presence.ConnRef{ConnID: 8, GatewayID: "gateway-1"}
	if err := registry.Register(t.Context(), 42, ref.ConnID, ref.GatewayID); err != nil {
		t.Fatalf("Register %#v: %v", ref, err)
	}
	pusher := &recordingMailNotifyPusher{}
	publisher := NewMailFanoutPublisher(registry, pusher)

	if err := publisher.PublishMailNotify(t.Context(), 42, "friend_request"); err != nil {
		t.Fatalf("PublishMailNotify: %v", err)
	}

	if got := pusher.notifications(); !reflect.DeepEqual(got, []pushedMailNotify{
		{ref: presence.ConnRef{ConnID: 8, GatewayID: "gateway-1"}, uid: 42, kind: "friend_request"},
	}) {
		t.Fatalf("mail notifications = %#v", got)
	}
}
