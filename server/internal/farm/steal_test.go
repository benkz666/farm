package farm

import (
	"testing"

	"farm/server/internal/pkgerr"
)

func TestApplyStealCapsAtFortyPercentAndTruncatesRandomAmount(t *testing.T) {
	owner := matureStealAggregate()

	result := owner.ApplySteal(StealAction{
		VisitorUID: 2,
		PlotIndex:  0,
		Roll:       8,
	}, actionNow)

	if result.Err != pkgerr.OK {
		t.Fatalf("Err = %d, want OK", result.Err)
	}
	if result.Amount != 6 {
		t.Fatalf("Amount = %d, want 6", result.Amount)
	}
	if got := owner.Plots[0].StolenCount; got != 6 {
		t.Fatalf("StolenCount = %d, want 6", got)
	}
	if got := owner.FarmSeq; got != 1 {
		t.Fatalf("FarmSeq = %d, want 1", got)
	}

	exhausted := owner.ApplySteal(StealAction{
		VisitorUID: 3,
		PlotIndex:  0,
		Roll:       1,
	}, actionNow)
	if exhausted.Err != pkgerr.StealQuotaExhausted {
		t.Fatalf("quota exhausted Err = %d, want %d", exhausted.Err, pkgerr.StealQuotaExhausted)
	}
}

func TestApplyStealAllowsVisitorOncePerHarvestRound(t *testing.T) {
	owner := matureStealAggregate()
	action := StealAction{VisitorUID: 2, PlotIndex: 0, Roll: 1}

	first := owner.ApplySteal(action, actionNow)
	if first.Err != pkgerr.OK {
		t.Fatalf("first Err = %d, want OK", first.Err)
	}
	repeated := owner.ApplySteal(action, actionNow)
	if repeated.Err != pkgerr.StealAlreadyDone {
		t.Fatalf("repeated Err = %d, want %d", repeated.Err, pkgerr.StealAlreadyDone)
	}

	owner.Plots[0].HarvestRound++
	owner.Plots[0].StolenCount = 0
	owner.Plots[0].Stealers = nil
	nextRound := owner.ApplySteal(action, actionNow)
	if nextRound.Err != pkgerr.OK {
		t.Fatalf("next round Err = %d, want OK", nextRound.Err)
	}
}

func TestApplyStealDoesNotCollideDistinctUint64Visitors(t *testing.T) {
	owner := matureStealAggregate()
	firstUID := uint64(1)<<40 | 7
	secondUID := uint64(2)<<40 | 7

	first := owner.ApplySteal(StealAction{VisitorUID: firstUID, PlotIndex: 0, Roll: 1}, actionNow)
	second := owner.ApplySteal(StealAction{VisitorUID: secondUID, PlotIndex: 0, Roll: 1}, actionNow)

	if first.Err != pkgerr.OK || second.Err != pkgerr.OK {
		t.Fatalf("distinct uint64 visitors = first:%d second:%d, want both OK", first.Err, second.Err)
	}
	if got := owner.Plots[0].Stealers; len(got) != 2 || got[0] != firstUID || got[1] != secondUID {
		t.Fatalf("Stealers = %#v, want [%d %d]", got, firstUID, secondUID)
	}
}

func TestApplyStealAfterOwnerHarvestReturnsHarvestedByOwner(t *testing.T) {
	owner := matureStealAggregate()

	harvest := owner.ApplyPlotAction(PlotAction{Kind: Harvest, PlotIndex: 0}, actionNow)
	if harvest.Err != pkgerr.OK {
		t.Fatalf("Harvest Err = %d, want OK", harvest.Err)
	}
	result := owner.ApplySteal(StealAction{
		VisitorUID: 2,
		PlotIndex:  0,
		Roll:       1,
	}, actionNow)

	if result.Err != pkgerr.HarvestedByOwner {
		t.Fatalf("Err = %d, want %d", result.Err, pkgerr.HarvestedByOwner)
	}
}

func matureStealAggregate() *Aggregate {
	owner := NewAggregate(1, "owner")
	owner.Plots[0] = Plot{
		State:          StateMature,
		CropID:         1,
		FinalYield:     16,
		HarvestRound:   1,
		SeasonDuration: 60_000,
		MatureAt:       actionNow,
	}
	return owner
}
