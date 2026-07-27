package bus

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryBusPublishDeliversToSubscriber(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()

	got := make(chan struct {
		key     string
		payload []byte
	}, 1)
	if err := bus.Subscribe(context.Background(), TopicCrossAction, func(key string, payload []byte) error {
		got <- struct {
			key     string
			payload []byte
		}{key, payload}
		return nil
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	wantKey := "uid:42"
	wantPayload := []byte(`{"req_id":1}`)
	if err := bus.Publish(context.Background(), TopicCrossAction, wantKey, wantPayload); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case g := <-got:
		if g.key != wantKey {
			t.Fatalf("key: want %q got %q", wantKey, g.key)
		}
		if !bytes.Equal(g.payload, wantPayload) {
			t.Fatalf("payload: want %q got %q", wantPayload, g.payload)
		}
	default:
		t.Fatal("handler 未收到消息")
	}
}

func TestMemoryBusMultipleSubscribersAllReceive(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()

	var mu sync.Mutex
	var keys []string
	mkHandler := func() func(string, []byte) error {
		return func(key string, payload []byte) error {
			mu.Lock()
			keys = append(keys, key)
			mu.Unlock()
			return nil
		}
	}
	if err := bus.Subscribe(context.Background(), TopicCrossResult, mkHandler()); err != nil {
		t.Fatalf("subscribe1: %v", err)
	}
	if err := bus.Subscribe(context.Background(), TopicCrossResult, mkHandler()); err != nil {
		t.Fatalf("subscribe2: %v", err)
	}

	if err := bus.Publish(context.Background(), TopicCrossResult, "k", []byte("p")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("want 2 次投递 got %d", len(keys))
	}
}

func TestMemoryBusPublishStopsOnFirstHandlerError(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()

	var secondCalled bool
	errSentinel := errors.New("boom")
	if err := bus.Subscribe(context.Background(), TopicCrossAction, func(string, []byte) error {
		return errSentinel
	}); err != nil {
		t.Fatalf("subscribe1: %v", err)
	}
	if err := bus.Subscribe(context.Background(), TopicCrossAction, func(string, []byte) error {
		secondCalled = true
		return nil
	}); err != nil {
		t.Fatalf("subscribe2: %v", err)
	}

	if err := bus.Publish(context.Background(), TopicCrossAction, "k", nil); !errors.Is(err, errSentinel) {
		t.Fatalf("want sentinel got %v", err)
	}
	if secondCalled {
		t.Fatal("首个 handler 出错后不应继续投递")
	}
}

func TestMemoryBusClosedRejectsPublishAndSubscribe(t *testing.T) {
	bus := NewMemoryBus()
	if err := bus.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := bus.Publish(context.Background(), TopicCrossAction, "k", nil); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("publish after close: want ErrBusClosed got %v", err)
	}
	if err := bus.Subscribe(context.Background(), TopicCrossAction, func(string, []byte) error { return nil }); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("subscribe after close: want ErrBusClosed got %v", err)
	}
}

func TestMemoryBusPublishUnknownTopicIsNoop(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()

	if err := bus.Publish(context.Background(), "no.such.topic", "k", []byte("p")); err != nil {
		t.Fatalf("publish 无人订阅应 no-op, got %v", err)
	}
}

func TestMemoryBusConcurrentPublishSafe(t *testing.T) {
	bus := NewMemoryBus()
	defer bus.Close()

	var mu sync.Mutex
	var count int
	_ = bus.Subscribe(context.Background(), TopicCrossAction, func(string, []byte) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Publish(context.Background(), TopicCrossAction, "k", []byte("p"))
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != 50 {
		t.Fatalf("want 50 次投递 got %d", count)
	}
}
