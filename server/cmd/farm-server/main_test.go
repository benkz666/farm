package main

import (
	"testing"

	"farm/server/internal/bus"
)

func TestLoadConfigRejectsInvalidRole(t *testing.T) {
	t.Setenv("FARM_ROLE", "worker")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted an unsupported FARM_ROLE")
	}
}

func TestLoadConfigRequiresGatewayRPCSettings(t *testing.T) {
	t.Setenv("FARM_ROLE", "gateway")
	t.Setenv("FARM_INTERNAL_TOKEN", "")
	t.Setenv("FARM_FARM_URLS", "")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted gateway mode without internal RPC settings")
	}

	t.Setenv("FARM_INTERNAL_TOKEN", "internal-token")
	t.Setenv("FARM_FARM_URLS", `{"farm-0":"http://127.0.0.1:9100"}`)
	if _, err := loadConfig(); err != nil {
		t.Fatalf("loadConfig with gateway RPC settings: %v", err)
	}
}

func TestLoadConfigRequiresFarmGatewayPushSettings(t *testing.T) {
	t.Setenv("FARM_ROLE", "farm")
	t.Setenv("FARM_INTERNAL_TOKEN", "internal-token")
	t.Setenv("FARM_INSTANCE_ID", "farm-0")
	t.Setenv("FARM_GATEWAY_URLS", "")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted farm mode without Gateway push settings")
	}

	t.Setenv("FARM_GATEWAY_URLS", `{"gateway-0":"http://127.0.0.1:9200"}`)
	if _, err := loadConfig(); err != nil {
		t.Fatalf("loadConfig with Gateway push settings: %v", err)
	}
}

func TestNewCrossBusUsesMemoryForAllRole(t *testing.T) {
	eventBus, err := newCrossBus(config{role: roleAll, busKind: "kafka"})
	if err != nil {
		t.Fatalf("newCrossBus: %v", err)
	}
	t.Cleanup(func() { _ = eventBus.Close() })
	if _, ok := eventBus.(*bus.MemoryBus); !ok {
		t.Fatalf("event bus = %T, want *bus.MemoryBus", eventBus)
	}
}
