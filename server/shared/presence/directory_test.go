package presence

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestGatewayDirectoryRegisterResolveListAndUnregister(t *testing.T) {
	backend := newGatewayDirectoryMemoryBackend()
	directory := newGatewayDirectoryWithBackend(backend)
	ctx := t.Context()

	if err := directory.Register(ctx, "gateway-pod-1", "10.42.0.7:9202", time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}
	target, err := directory.ResolveGateway(ctx, "gateway-pod-1")
	if err != nil || target != "10.42.0.7:9202" {
		t.Fatalf("ResolveGateway = %q, %v", target, err)
	}
	gateways, err := directory.ListGateways(ctx)
	if err != nil {
		t.Fatalf("ListGateways: %v", err)
	}
	want := map[string]string{"gateway-pod-1": "10.42.0.7:9202"}
	if !reflect.DeepEqual(gateways, want) {
		t.Fatalf("ListGateways = %#v, want %#v", gateways, want)
	}
	if err := directory.Unregister(ctx, "gateway-pod-1", "10.42.0.7:9202"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := directory.ResolveGateway(ctx, "gateway-pod-1"); !errors.Is(err, ErrGatewayInstanceNotFound) {
		t.Fatalf("ResolveGateway after unregister = %v", err)
	}
}

func TestGatewayDirectoryStaleUnregisterKeepsNewEndpoint(t *testing.T) {
	backend := newGatewayDirectoryMemoryBackend()
	directory := newGatewayDirectoryWithBackend(backend)
	ctx := t.Context()

	if err := directory.Register(ctx, "gateway-1", "10.42.0.7:9202", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := directory.Register(ctx, "gateway-1", "10.42.0.8:9202", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := directory.Unregister(ctx, "gateway-1", "10.42.0.7:9202"); err != nil {
		t.Fatal(err)
	}
	target, err := directory.ResolveGateway(ctx, "gateway-1")
	if err != nil || target != "10.42.0.8:9202" {
		t.Fatalf("ResolveGateway = %q, %v", target, err)
	}
}

func TestGatewayDirectoryRejectsInvalidRegistration(t *testing.T) {
	directory := newGatewayDirectoryWithBackend(newGatewayDirectoryMemoryBackend())
	for _, test := range []struct {
		id     string
		target string
		ttl    time.Duration
	}{
		{id: "", target: "10.42.0.7:9202", ttl: time.Minute},
		{id: "gateway:1", target: "10.42.0.7:9202", ttl: time.Minute},
		{id: "gateway-1", target: "not-an-endpoint", ttl: time.Minute},
		{id: "gateway-1", target: "10.42.0.7:9202", ttl: 0},
	} {
		if err := directory.Register(t.Context(), test.id, test.target, test.ttl); err == nil {
			t.Fatalf("Register(%q, %q, %s) unexpectedly succeeded", test.id, test.target, test.ttl)
		}
	}
}

type gatewayDirectoryMemoryBackend struct {
	mu     sync.Mutex
	values map[string]string
}

func newGatewayDirectoryMemoryBackend() *gatewayDirectoryMemoryBackend {
	return &gatewayDirectoryMemoryBackend{values: make(map[string]string)}
}

func (backend *gatewayDirectoryMemoryBackend) Put(
	_ context.Context,
	key, value string,
	_ time.Duration,
) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.values[key] = value
	return nil
}

func (backend *gatewayDirectoryMemoryBackend) Get(_ context.Context, key string) (string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	value, ok := backend.values[key]
	if !ok {
		return "", redis.Nil
	}
	return value, nil
}

func (backend *gatewayDirectoryMemoryBackend) DeleteIfValue(
	_ context.Context,
	key, value string,
) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.values[key] == value {
		delete(backend.values, key)
	}
	return nil
}

func (backend *gatewayDirectoryMemoryBackend) List(
	_ context.Context,
	prefix string,
) (map[string]string, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	result := make(map[string]string)
	for key, value := range backend.values {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			result[key] = value
		}
	}
	return result, nil
}
