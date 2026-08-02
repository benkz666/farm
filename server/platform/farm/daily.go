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
//
// 逻辑日已经翻页时直接放弃归还：那个计数器连同它所属的一天已经不存在了。
// 这里绝不能调 syncDaily——用昨天的 dayID 去同步会把今天已经攒下的计数清零，
// 等于送出一整轮 150 次额度。
func (a *Aggregate) RollbackMaintenance(dayID uint32, reserved bool) {
	if a == nil || !reserved || a.Daily.DayID != dayID {
		return
	}
	if a.Daily.MaintainCnt > 0 {
		a.Daily.MaintainCnt--
	}
}

// SettleMaintenance grants a reward after the owner has committed the action and
// reports what was granted, so the protocol layer never restates the numbers.
//
// 不看 dayID：额度在预占时就已经扣掉，奖励是那次预占的既得对价，跨日结算也照发。
func (a *Aggregate) SettleMaintenance(reserved bool, kind PlotActionKind) (exp uint32, coin int64) {
	if a == nil || !reserved {
		return 0, 0
	}
	exp = maintenanceExpReward
	if kind == Weed || kind == Pest {
		coin = maintenanceCoinReward
	}
	a.Exp += exp
	a.Coin += coin
	a.RecalcLevel()
	return exp, coin
}

func (a *Aggregate) syncDaily(dayID uint32) {
	if a.Daily.DayID != dayID {
		a.Daily = DailyState{DayID: dayID}
	}
}
