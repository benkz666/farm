package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	"farm/server/shared/errcode"
)

func TestDueWebSocketRequestRenewsConnregLease(t *testing.T) {
	t.Parallel()

	var nowMs atomic.Int64
	nowMs.Store(1_000_000)
	backend := newConnectionRegistryBackend()
	registry := presence.NewWithBackend(backend,
		presence.WithClock(func() time.Time { return time.UnixMilli(nowMs.Load()) }),
		presence.WithLeaseTTL(time.Minute),
	)
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithConnectionRegistry(registry, "gateway-0"),
	)
	var serverConnection *wsConnection
	gateway.afterConnectionRegistered = func(connection *wsConnection) {
		serverConnection = connection
	}
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	refs, err := registry.Lookup(context.Background(), 42)
	if err != nil || len(refs) != 1 {
		t.Fatalf("Lookup after handshake = %#v err=%v", refs, err)
	}

	// Without renew, lease expires at t0+60s. Advance 50s then Ping to renew.
	nowMs.Add(50_000)
	serverConnection.nextAuthValidationAt.Store(0)
	writeEnvelope(t, connection, Envelope{Cmd: CommandPing, ClientSeq: 2, Payload: []byte(`{"client_time":1}`)})
	if got := readEnvelope(t, connection); got.Err != errcode.OK {
		t.Fatalf("Ping = %#v", got)
	}

	nowMs.Add(50_000)
	refs, err = registry.Lookup(context.Background(), 42)
	if err != nil {
		t.Fatalf("Lookup after renew window: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("Lookup after renew = %#v, want still leased", refs)
	}
}

func TestHotWebSocketRequestSkipsConnregRenewBeforeInterval(t *testing.T) {
	t.Parallel()

	var nowMs atomic.Int64
	nowMs.Store(2_000_000)
	backend := newConnectionRegistryBackend()
	registry := presence.NewWithBackend(backend,
		presence.WithClock(func() time.Time { return time.UnixMilli(nowMs.Load()) }),
		presence.WithLeaseTTL(time.Minute),
	)
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithConnectionRegistry(registry, "gateway-0"),
	)
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	// A hot-path command inside the validation interval must not extend the
	// Redis lease. In production the periodic Pong or a later due request does.
	nowMs.Add(50_000)
	writeEnvelope(t, connection, Envelope{Cmd: CommandPing, ClientSeq: 2, Payload: []byte(`{"client_time":1}`)})
	if got := readEnvelope(t, connection); got.Err != errcode.OK {
		t.Fatalf("Ping = %#v", got)
	}
	nowMs.Add(11_000)
	refs, err := registry.Lookup(context.Background(), 42)
	if err != nil {
		t.Fatalf("Lookup after original lease expiry: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("hot request unexpectedly renewed Redis lease: %#v", refs)
	}
}
