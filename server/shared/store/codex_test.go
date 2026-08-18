package store

import (
	"testing"

	"farm/server/shared/gameconfig"
)

func TestHasEligibleCodexRewardSkipsOrdinaryHarvests(t *testing.T) {
	if len(gameconfig.CodexTiers) == 0 {
		t.Fatal("codex tiers are empty")
	}
	first := gameconfig.CodexTiers[0].HarvestCount
	if first > 1 && hasEligibleCodexReward(first-1) {
		t.Fatalf("harvest count %d unexpectedly needs reward persistence", first-1)
	}
	if !hasEligibleCodexReward(first) {
		t.Fatalf("first milestone %d was skipped", first)
	}
}
