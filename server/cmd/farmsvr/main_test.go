package main

import (
	"testing"
	"time"
)

func TestActorRuntimeSettings(t *testing.T) {
	t.Setenv("FARM_ACTOR_IDLE_TTL", "90s")
	t.Setenv("FARM_ACTOR_MAX_RESIDENT", "1234")
	ttl, err := durationSetting("FARM_ACTOR_IDLE_TTL", "2m")
	if err != nil || ttl != 90*time.Second {
		t.Fatalf("durationSetting = %v, %v", ttl, err)
	}
	limit, err := intSetting("FARM_ACTOR_MAX_RESIDENT", 20_000)
	if err != nil || limit != 1234 {
		t.Fatalf("intSetting = %d, %v", limit, err)
	}
}

func TestActorRuntimeSettingsRejectInvalidValues(t *testing.T) {
	t.Setenv("FARM_ACTOR_IDLE_TTL", "0s")
	t.Setenv("FARM_ACTOR_MAX_RESIDENT", "-1")
	if _, err := durationSetting("FARM_ACTOR_IDLE_TTL", "2m"); err == nil {
		t.Fatal("durationSetting accepted zero")
	}
	if _, err := intSetting("FARM_ACTOR_MAX_RESIDENT", 20_000); err == nil {
		t.Fatal("intSetting accepted negative value")
	}
}
