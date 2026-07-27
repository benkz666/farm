package farm

// MaintenanceDailyLimit is the shared daily reward cap for watering, weeding,
// and pest removal. Actions above the cap still mutate the owner farm but do
// not award the visitor experience or mutual-aid coin.
const MaintenanceDailyLimit uint16 = 150

const (
	maintenanceExpReward  uint32 = 2
	maintenanceCoinReward int64  = 5
)

// DailyState contains player state that is reset lazily when the logical day
// changes. It is persisted with Aggregate rather than maintained in a process
// local counter so a visitor cannot bypass the limit by reconnecting.
type DailyState struct {
	DayID       uint32 `json:"day_id"`
	MaintainCnt uint16 `json:"maintain_cnt"`
}

// ReserveMaintenance reserves one rewardable maintenance slot. A false result
// means the action may still proceed but must not grant rewards.
func (a *Aggregate) ReserveMaintenance(dayID uint32) bool {
	if a == nil {
		return false
	}
	a.syncDaily(dayID)
	if a.Daily.MaintainCnt >= MaintenanceDailyLimit {
		return false
	}
	a.Daily.MaintainCnt++
	return true
}

// RollbackMaintenance returns a previously reserved rewardable slot after the
// owner rejects or times out a cross-farm action.
func (a *Aggregate) RollbackMaintenance(dayID uint32, reserved bool) {
	if a == nil || !reserved {
		return
	}
	a.syncDaily(dayID)
	if a.Daily.MaintainCnt > 0 {
		a.Daily.MaintainCnt--
	}
}

// SettleMaintenance grants a reward after the owner has committed the action.
// Weed and Pest mutual aid also award five coins while sharing the same cap.
func (a *Aggregate) SettleMaintenance(dayID uint32, reserved bool, kind PlotActionKind) {
	if a == nil || !reserved {
		return
	}
	a.syncDaily(dayID)
	a.Exp += maintenanceExpReward
	if kind == Weed || kind == Pest {
		a.Coin += maintenanceCoinReward
	}
	a.RecalcLevel()
}

func (a *Aggregate) syncDaily(dayID uint32) {
	if a.Daily.DayID != dayID {
		a.Daily = DailyState{DayID: dayID}
	}
}
