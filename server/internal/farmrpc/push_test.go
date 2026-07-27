package farmrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
)

func TestFanoutPublisherPushesEverySubscribedConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 8, "gateway-1"); err != nil {
		t.Fatalf("Subscribe first connection: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 9, "gateway-0"); err != nil {
		t.Fatalf("Subscribe second connection: %v", err)
	}
	pusher := &recordingDeltaPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}

	if err := publisher.Publish(t.Context(), delta, 0); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := []pushedDelta{
		{ref: connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"}, delta: delta},
		{ref: connreg.ConnRef{ConnID: 9, GatewayID: "gateway-0"}, delta: delta},
	}
	if !reflect.DeepEqual(pusher.pushes, want) {
		t.Fatalf("pushes = %#v, want %#v", pusher.pushes, want)
	}
}

func TestFanoutPublisherSkipsOriginatingConnection(t *testing.T) {
	backend := newRegistryBackend()
	registry := connreg.NewWithBackend(backend)
	if err := registry.Subscribe(t.Context(), 42, 8, "gateway-1"); err != nil {
		t.Fatalf("Subscribe first connection: %v", err)
	}
	if err := registry.Subscribe(t.Context(), 42, 9, "gateway-0"); err != nil {
		t.Fatalf("Subscribe originator connection: %v", err)
	}
	pusher := &recordingDeltaPusher{}
	publisher := NewFanoutPublisher(registry, pusher)
	delta := farm.FarmDelta{OwnerUID: 42, FarmSeq: 3}

	if err := publisher.Publish(t.Context(), delta, 9); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	want := []pushedDelta{{
		ref:   connreg.ConnRef{ConnID: 8, GatewayID: "gateway-1"},
		delta: delta,
	}}
	if !reflect.DeepEqual(pusher.pushes, want) {
		t.Fatalf("pushes = %#v, want %#v", pusher.pushes, want)
	}
}

func TestHTTPDeltaPusherSendsAuthenticatedPushRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deltaPushPath {
			t.Fatalf("path = %q, want %q", r.URL.Path, deltaPushPath)
		}
		if r.Header.Get("Authorization") != "Bearer internal-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	pusher := NewHTTPDeltaPusher(map[string]string{"gateway-0": server.URL}, "internal-token")

	if err := pusher.Push(t.Context(), connreg.ConnRef{ConnID: 7, GatewayID: "gateway-0"}, farm.FarmDelta{OwnerUID: 42}); err != nil {
		t.Fatalf("Push: %v", err)
	}
}

type registryBackend struct {
	hashes map[string]map[string]string
}

func newRegistryBackend() *registryBackend {
	return &registryBackend{hashes: make(map[string]map[string]string)}
}

func (b *registryBackend) Set(_ context.Context, key, field, value string) error {
	if b.hashes[key] == nil {
		b.hashes[key] = make(map[string]string)
	}
	b.hashes[key][field] = value
	return nil
}

func (*registryBackend) Delete(context.Context, string, string) error { return nil }

func (b *registryBackend) Values(_ context.Context, key string) (map[string]string, error) {
	values := make(map[string]string, len(b.hashes[key]))
	for field, value := range b.hashes[key] {
		values[field] = value
	}
	return values, nil
}

type pushedDelta struct {
	ref   connreg.ConnRef
	delta farm.FarmDelta
}

type recordingDeltaPusher struct {
	pushes []pushedDelta
}

func (p *recordingDeltaPusher) Push(_ context.Context, ref connreg.ConnRef, delta farm.FarmDelta) error {
	p.pushes = append(p.pushes, pushedDelta{ref: ref, delta: delta})
	return nil
}
