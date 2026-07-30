package main

import (
	"context"
	"strings"
	"testing"

	"farm/server/internal/bus"
	"farm/server/internal/farm"
	"farm/server/internal/obs"
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
	t.Setenv("FARM_GATEWAY_URLS", "")
	t.Setenv("FARM_INSTANCE_ID", "gateway-0")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted gateway mode without internal RPC settings")
	}

	t.Setenv("FARM_INTERNAL_TOKEN", "internal-token")
	t.Setenv("FARM_FARM_URLS", `{"farm-0":"http://127.0.0.1:9100"}`)
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted gateway mode without TaskNotify Gateway topology")
	}

	t.Setenv("FARM_GATEWAY_URLS", `{"gateway-0":"not-a-url"}`)
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted malformed Gateway push endpoint")
	}

	t.Setenv("FARM_GATEWAY_URLS", `{"gateway-1":"http://127.0.0.1:9201"}`)
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig accepted Gateway topology without this instance")
	}

	t.Setenv("FARM_GATEWAY_URLS", `{"gateway-0":"http://127.0.0.1:9200"}`)
	if _, err := loadConfig(); err != nil {
		t.Fatalf("loadConfig with gateway RPC and TaskNotify settings: %v", err)
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

func TestCheckSecretsRejectsDevHazardSecretOutsideDev(t *testing.T) {
	strong := strings.Repeat("s", minSecretLength)
	for _, role := range []string{roleAll, roleFarm} {
		t.Run(role, func(t *testing.T) {
			cfg := config{
				env:           "prod",
				role:          role,
				tokenSecret:   strong,
				inviteSecret:  strong,
				internalToken: strong,
				hazardSecret:  devHazardSecret,
			}
			if err := cfg.checkSecrets(); err == nil {
				t.Fatal("checkSecrets accepted dev hazard secret outside FARM_ENV=dev")
			}
		})
	}

	t.Run("gateway_skips_hazard", func(t *testing.T) {
		cfg := config{
			env:           "prod",
			role:          roleGateway,
			tokenSecret:   strong,
			inviteSecret:  strong,
			internalToken: strong,
			hazardSecret:  "", // gateway 不推进农场，可不配
		}
		if err := cfg.checkSecrets(); err != nil {
			t.Fatalf("gateway checkSecrets = %v, want nil", err)
		}
	})
}

func TestLoadConfigDerivesHazardSaltFromSecret(t *testing.T) {
	t.Setenv("FARM_ENV", "dev")
	t.Setenv("FARM_ROLE", "all")
	t.Setenv("FARM_HAZARD_SECRET", "unit-test-hazard-secret")
	t.Setenv("FARM_TOKEN_SECRET", "dev-only-change-me")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := farm.DeriveHazardSalt("unit-test-hazard-secret")
	if cfg.hazardSalt != want {
		t.Fatalf("hazardSalt = %d, want %d", cfg.hazardSalt, want)
	}
}

func TestLoadConfigAdminAddr(t *testing.T) {
	t.Setenv("FARM_ENV", "dev")
	t.Setenv("FARM_ROLE", "all")
	t.Setenv("FARM_TOKEN_SECRET", "dev-only-change-me")

	t.Setenv("FARM_ADMIN_ADDR", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.adminEnabled || cfg.adminAddr != "127.0.0.1:9300" {
		t.Fatalf("default admin = enabled=%v addr=%q", cfg.adminEnabled, cfg.adminAddr)
	}

	t.Setenv("FARM_ADMIN_ADDR", "off")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig off: %v", err)
	}
	if cfg.adminEnabled {
		t.Fatal("expected admin disabled")
	}
}

func TestShutdownOrderBeginsWithDrain(t *testing.T) {
	// 关停契约：BeginDrain 必须先于业务停服，且 /readyz 立即 503，而 /healthz 仍 200。
	probe := obs.NewProbe()
	probe.MarkReady()
	if err := probe.Ready(context.Background()); err != nil {
		t.Fatalf("ready before drain: %v", err)
	}
	probe.BeginDrain()
	if err := probe.Ready(context.Background()); err == nil {
		t.Fatal("ready after BeginDrain should fail")
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
