package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"farm/server/domain/farm"
	"farm/server/shared/gameconfig"
)

func TestValidFixtureProfile(t *testing.T) {
	for _, profile := range []string{"default", "water", "water-visitor", "harvest", "sell", "hot-economy", "steal"} {
		if !validFixtureProfile(profile) {
			t.Fatalf("profile %q should be valid", profile)
		}
	}
	if validFixtureProfile("unknown") {
		t.Fatal("unknown profile should be rejected")
	}
}

func TestFixtureTimeProfileDefault(t *testing.T) {
	t.Setenv("FARM_TIME_PROFILE", gameconfig.TimeProfileAuthentic)
	if got := fixtureTimeProfileDefault(); got != gameconfig.TimeProfileAuthentic {
		t.Fatalf("time profile = %q", got)
	}
	t.Setenv("FARM_TIME_PROFILE", "invalid")
	if got := fixtureTimeProfileDefault(); got != gameconfig.TimeProfileDemo {
		t.Fatalf("invalid time profile fallback = %q", got)
	}
}

func TestReadFixturesReusesExistingCredentials(t *testing.T) {
	want := []fixture{
		{UID: "9007199254740993", Token: "token-a", PeerUID: "9007199254740994"},
		{UID: "9007199254740994", Token: "token-b", PeerUID: "9007199254740993"},
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	encoded, err := json.Marshal(map[string]any{"accounts": want})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readFixtures(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UID != want[0].UID || got[0].Token != want[0].Token {
		t.Fatalf("fixtures = %#v", got)
	}
}

func TestStatefulPlotProfilesExposeEveryLegalPlot(t *testing.T) {
	for _, profile := range []string{"water", "water-visitor", "harvest", "steal"} {
		indexes := fixturePlotIndexes(profile)
		if len(indexes) != gameconfig.MaxPlots {
			t.Fatalf("%s plot indexes = %d, want %d", profile, len(indexes), gameconfig.MaxPlots)
		}
		for index, value := range indexes {
			if value != index {
				t.Fatalf("%s plot index[%d] = %d", profile, index, value)
			}
		}
	}
	if indexes := fixturePlotIndexes("hot-economy"); len(indexes) != 0 {
		t.Fatalf("hot-economy plot indexes = %#v, want none", indexes)
	}
}

func TestPrepareAggregateProfileUnlocksAndInitializesAllPlots(t *testing.T) {
	const now = int64(1_786_000_000_000)
	water := farm.NewAggregate(41, "water")
	if err := prepareAggregateProfile(water, "water", gameconfig.TimeProfileAuthentic, now); err != nil {
		t.Fatal(err)
	}
	if water.UnlockedPlots != gameconfig.MaxPlots {
		t.Fatalf("water unlocked plots = %d", water.UnlockedPlots)
	}
	for index, plot := range water.Plots {
		if plot.State != farm.StateGrowing || plot.CropID == 0 || plot.LastWaterAt != 0 || plot.PlantNonce != uint32(index+1) {
			t.Fatalf("water plot[%d] = %#v", index, plot)
		}
		crop, _ := gameconfig.CropByID(plot.CropID)
		if want := gameconfig.SeasonDurationMs(crop, 0, gameconfig.TimeProfileAuthentic); plot.SeasonDuration != want {
			t.Fatalf("water plot[%d] duration = %d, want %d", index, plot.SeasonDuration, want)
		}
	}

	harvest := farm.NewAggregate(42, "harvest")
	if err := prepareAggregateProfile(harvest, "harvest", gameconfig.TimeProfileAuthentic, now); err != nil {
		t.Fatal(err)
	}
	if harvest.UnlockedPlots != gameconfig.MaxPlots {
		t.Fatalf("harvest unlocked plots = %d", harvest.UnlockedPlots)
	}
	for index, plot := range harvest.Plots {
		if plot.State != farm.StateMature || plot.CropID == 0 || plot.FinalYield == 0 || plot.PlantNonce != uint32(index+1) {
			t.Fatalf("harvest plot[%d] = %#v", index, plot)
		}
	}
}
