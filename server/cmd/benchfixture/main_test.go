package main

import "testing"

func TestValidFixtureProfile(t *testing.T) {
	for _, profile := range []string{"default", "water", "water-visitor", "harvest", "sell", "steal"} {
		if !validFixtureProfile(profile) {
			t.Fatalf("profile %q should be valid", profile)
		}
	}
	if validFixtureProfile("unknown") {
		t.Fatal("unknown profile should be rejected")
	}
}
