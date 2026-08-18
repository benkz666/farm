package store

import (
	"testing"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/outbox"
)

func TestValidateFarmMutationsAllowsZeroSequenceForSideEffectsOnly(t *testing.T) {
	hasFarmRows, err := validateFarmMutations([]*farmv1.FarmWriteMutation{{
		Uid:     33,
		FarmSeq: 0,
		TaskAdvances: []*farmv1.FarmWriteTaskAdvance{{
			DayKey: 1,
			TaskId: 2,
			Amount: 1,
		}},
	}})
	if err != nil {
		t.Fatalf("validate side-effect-only mutation: %v", err)
	}
	if hasFarmRows {
		t.Fatal("side-effect-only mutation unexpectedly reports farm rows")
	}
}

func TestValidateFarmMutationsAllowsLegacyUnsequencedCrossVisitor(t *testing.T) {
	hasFarmRows, err := validateFarmMutations([]*farmv1.FarmWriteMutation{{
		Uid:        33,
		FarmSeq:    0,
		PlayerMask: outbox.PlayerEconomy | outbox.PlayerDaily | outbox.PlayerCrossPending,
	}})
	if err != nil {
		t.Fatalf("validate legacy cross-visitor mutation: %v", err)
	}
	if !hasFarmRows {
		t.Fatal("legacy cross-visitor mutation did not report farm rows")
	}
}

func TestValidateFarmMutationsRejectsZeroSequenceWithFarmRows(t *testing.T) {
	if _, err := validateFarmMutations([]*farmv1.FarmWriteMutation{{
		Uid:        33,
		FarmSeq:    0,
		PlayerMask: 1,
	}}); err == nil {
		t.Fatal("farm-row mutation with zero sequence unexpectedly passed validation")
	}
}
