//go:build integration

package connreg_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"farm/server/platform/connreg"
)

func TestRedisRegistryReplaceConnection(t *testing.T) {
	addr := os.Getenv("FARM_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis Ping: %v", err)
	}

	registry := connreg.New(client, connreg.WithLeaseTTL(time.Minute))
	uid := uint64(time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.Unregister(context.Background(), uid, 2, "gateway-1")
	})

	evicted, err := registry.ReplaceConnection(ctx, uid, 1, "gateway-0")
	if err != nil {
		t.Fatalf("first ReplaceConnection: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("first evicted = %#v, want empty", evicted)
	}

	evicted, err = registry.ReplaceConnection(ctx, uid, 2, "gateway-1")
	if err != nil {
		t.Fatalf("second ReplaceConnection: %v", err)
	}
	wantEvicted := []connreg.ConnRef{{ConnID: 1, GatewayID: "gateway-0"}}
	if !reflect.DeepEqual(evicted, wantEvicted) {
		t.Fatalf("second evicted = %#v, want %#v", evicted, wantEvicted)
	}

	got, err := registry.Lookup(ctx, uid)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := []connreg.ConnRef{{ConnID: 2, GatewayID: "gateway-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %#v, want %#v", got, want)
	}
}
