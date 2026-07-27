package connreg

import (
	"context"
	"reflect"
	"testing"
)

func TestRegistryRegistersLooksUpAndUnregistersConnection(t *testing.T) {
	backend := newMemoryBackend()
	registry := NewWithBackend(backend)
	ctx := context.Background()

	if err := registry.Register(ctx, 42, 7, "gateway-0"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after Register: %v", err)
	}
	want := []ConnRef{{ConnID: 7, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup after Register = %#v, want %#v", got, want)
	}

	if err := registry.Unregister(ctx, 42, 7); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	got, err = registry.Lookup(ctx, 42)
	if err != nil {
		t.Fatalf("Lookup after Unregister: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Lookup after Unregister = %#v, want empty", got)
	}
}

type memoryBackend struct {
	hashes map[string]map[string]string
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{hashes: make(map[string]map[string]string)}
}

func (b *memoryBackend) Set(_ context.Context, key, field, value string) error {
	if b.hashes[key] == nil {
		b.hashes[key] = make(map[string]string)
	}
	b.hashes[key][field] = value
	return nil
}

func (b *memoryBackend) Delete(_ context.Context, key, field string) error {
	delete(b.hashes[key], field)
	return nil
}

func (b *memoryBackend) Values(_ context.Context, key string) (map[string]string, error) {
	values := make(map[string]string, len(b.hashes[key]))
	for field, value := range b.hashes[key] {
		values[field] = value
	}
	return values, nil
}
