package gateway

import (
	"errors"
	"sync"
	"testing"
	"time"

	"farm/server/internal/farm"
	"farm/server/internal/store"
)

func TestTaskNotifyMailboxDefersUntilConnectionReady(t *testing.T) {
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: farm.NewAggregate(42, "alice")})
	delivered := make(chan store.Task, 1)
	gateway.taskNotifyDelivery = func(_ *wsConnection, task store.Task) error {
		delivered <- task
		return nil
	}
	connection := &wsConnection{id: 1, uid: 42, authed: true}
	gateway.connections.Store(connection.id, connection)

	if !connection.enqueueTaskNotify(store.Task{ID: store.TaskPlantID, Progress: 1, Target: 1}) {
		t.Fatal("enqueueTaskNotify rejected a valid task")
	}
	select {
	case task := <-delivered:
		t.Fatalf("TaskNotify delivered before handshake response: %#v", task)
	case <-time.After(50 * time.Millisecond):
	}

	connection.enableTaskNotify(gateway)
	select {
	case task := <-delivered:
		if task.ID != store.TaskPlantID {
			t.Fatalf("TaskNotify = %#v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("TaskNotify did not deliver after handshake response")
	}
	connection.closeTaskNotify()
	select {
	case <-connection.taskNotifyDone:
	case <-time.After(time.Second):
		t.Fatal("TaskNotify dispatcher did not exit after connection close")
	}
}

func TestTaskNotifyMailboxIsolatesSlowConnectionAndCoalescesLatestTask(t *testing.T) {
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: farm.NewAggregate(42, "alice")})
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	slowDelivered := make(chan store.Task, 4)
	fastDelivered := make(chan store.Task, 4)
	var once sync.Once
	gateway.taskNotifyDelivery = func(connection *wsConnection, task store.Task) error {
		if connection.id == 1 {
			first := false
			once.Do(func() {
				first = true
				close(slowStarted)
			})
			if first {
				<-releaseSlow
			}
			slowDelivered <- task
			return nil
		}
		fastDelivered <- task
		return nil
	}
	slow := &wsConnection{id: 1, uid: 42, authed: true}
	fast := &wsConnection{id: 2, uid: 42, authed: true}
	slow.enableTaskNotify(gateway)
	fast.enableTaskNotify(gateway)
	defer slow.closeTaskNotify()
	defer fast.closeTaskNotify()
	gateway.connections.Store(slow.id, slow)
	gateway.connections.Store(fast.id, fast)

	gateway.PublishTaskNotify(t.Context(), 42, store.Task{ID: store.TaskPlantID, Progress: 1, Target: 1})
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow TaskNotify write did not start")
	}
	gateway.PublishTaskNotify(t.Context(), 42, store.Task{ID: store.TaskPlantID, Progress: 2, Target: 2})
	gateway.PublishTaskNotify(t.Context(), 42, store.Task{ID: store.TaskHarvestID, Progress: 1, Target: 1})

	select {
	case task := <-fastDelivered:
		if task.ID == 0 {
			t.Fatal("fast connection received an empty TaskNotify")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("slow connection delayed fast TaskNotify")
	}

	close(releaseSlow)
	deadline := time.After(time.Second)
	seenLatestPlant := false
	for !seenLatestPlant {
		select {
		case task := <-slowDelivered:
			if task.ID == store.TaskPlantID && task.Progress == 2 {
				seenLatestPlant = true
			}
		case <-deadline:
			t.Fatal("slow mailbox did not retain the latest plant task")
		}
	}
}

func TestTaskNotifyMailboxStopsBlockedDeliveryWhenConnectionCloses(t *testing.T) {
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: farm.NewAggregate(42, "alice")})
	started := make(chan struct{})
	gateway.taskNotifyDelivery = func(connection *wsConnection, _ store.Task) error {
		close(started)
		<-connection.taskNotifyStop
		return errors.New("connection closed")
	}
	connection := &wsConnection{id: 1, uid: 42, authed: true}
	connection.enableTaskNotify(gateway)
	connection.enqueueTaskNotify(store.Task{ID: store.TaskPlantID, Progress: 1, Target: 1})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("TaskNotify delivery did not start")
	}

	connection.closeTaskNotify()
	select {
	case <-connection.taskNotifyDone:
	case <-time.After(time.Second):
		t.Fatal("TaskNotify dispatcher did not exit after blocked delivery closed")
	}
}
