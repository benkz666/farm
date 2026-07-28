package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

func TestWebSocketRequestRenewsConnregLease(t *testing.T) {
	t.Parallel()

	var nowMs atomic.Int64
	nowMs.Store(1_000_000)
	backend := newConnectionRegistryBackend()
	registry := connreg.NewWithBackend(backend,
		connreg.WithClock(func() time.Time { return time.UnixMilli(nowMs.Load()) }),
		connreg.WithLeaseTTL(time.Minute),
	)
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithConnectionRegistry(registry, "gateway-0"),
	)
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	refs, err := registry.Lookup(context.Background(), 42)
	if err != nil || len(refs) != 1 {
		t.Fatalf("Lookup after handshake = %#v err=%v", refs, err)
	}

	// Without renew, lease expires at t0+60s. Advance 50s then Ping to renew.
	nowMs.Add(50_000)
	writeEnvelope(t, connection, Envelope{Cmd: CommandPing, ClientSeq: 2, Payload: []byte(`{"client_time":1}`)})
	if got := readEnvelope(t, connection); got.Err != pkgerr.OK {
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
