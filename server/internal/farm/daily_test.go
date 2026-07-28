package farm

import "testing"

func TestMaintenanceReservationRewardsOnlyWithinDailyLimit(t *testing.T) {
	aggregate := NewAggregate(1, "visitor")
	const dayID uint32 = 12

	for range MaintenanceDailyLimit {
		if !aggregate.ReserveMaintenance(dayID) {
			t.Fatal("reservation below limit must earn a reward")
		}
	}
	if aggregate.ReserveMaintenance(dayID) {
		t.Fatal("reservation beyond limit must not earn a reward")
	}

	aggregate.SettleMaintenance(true, Weed)
	if aggregate.Exp != 2 || aggregate.Coin != 1_005 {
		t.Fatalf("reward = exp:%d coin:%d, want exp:2 coin:1005", aggregate.Exp, aggregate.Coin)
	}
	aggregate.SettleMaintenance(false, Pest)
	if aggregate.Exp != 2 || aggregate.Coin != 1_005 {
		t.Fatalf("over-limit action gave reward = exp:%d coin:%d", aggregate.Exp, aggregate.Coin)
	}
}

func TestMaintenanceRollbackReleasesReservedDailySlot(t *testing.T) {
	aggregate := NewAggregate(1, "visitor")
	const dayID uint32 = 12

	if !aggregate.ReserveMaintenance(dayID) {
		t.Fatal("first reservation must earn a reward")
	}
	aggregate.RollbackMaintenance(dayID, true)
	if aggregate.Daily.MaintainCnt != 0 {
		t.Fatalf("maintenance count after rollback = %d, want 0", aggregate.Daily.MaintainCnt)
	}
	if !aggregate.ReserveMaintenance(dayID) {
		t.Fatal("rolled-back reservation must make the reward slot available again")
	}
}

func TestMaintenanceDailyCounterResetsLazilyOnNewDay(t *testing.T) {
	aggregate := NewAggregate(1, "visitor")
	if !aggregate.ReserveMaintenance(12) {
		t.Fatal("first reservation must earn a reward")
	}
	if !aggregate.ReserveMaintenance(13) {
		t.Fatal("new logical day must earn a reward")
	}
	if aggregate.Daily.DayID != 13 || aggregate.Daily.MaintainCnt != 1 {
		t.Fatalf("daily state = %#v, want day 13 count 1", aggregate.Daily)
	}
}
