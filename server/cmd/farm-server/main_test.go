package main

import "testing"

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
