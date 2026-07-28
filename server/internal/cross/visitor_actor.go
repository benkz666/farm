package cross

import (
	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

// VisitorReservation describes resources that must be reserved on the Farm
// authoritative for the visitor before a CrossAction is published.
type VisitorReservation struct {
	Action CrossAction `json:"action"`
	DayID  uint32      `json:"day_id,omitempty"`
}

// VisitorReward is returned to the original client after authoritative
// settlement on the visitor's Farm.
type VisitorReward struct {
	ReqID        uint64       `json:"req_id"`
	ExpGained    uint32       `json:"exp_gained"`
	CoinGained   int64        `json:"coin_gained"`
	CropID       uint16       `json:"crop_id,omitempty"`
	Amount       uint16       `json:"amount,omitempty"`
	Compensation int64        `json:"compensation,omitempty"`
	DogType      farm.DogType `json:"dog_type,omitempty"`
}

// ReserveVisitor 在访客自己的聚合里扣减代价并登记预占。
//
// 预占登记在聚合内而非 Gateway 内存里，所以它随聚合落盘：Gateway 崩溃只会让
// 客户端收不到这一次的回执，不会让冻结的金币失去回滚的责任人。
func ReserveVisitor(aggregate *farm.Aggregate, reservation VisitorReservation, now int64) pkgerr.Code {
	if aggregate == nil || reservation.Action.VisitorUID != aggregate.UID {
		return pkgerr.BadRequest
	}

	entry := farm.CrossReservation{
		ReqID:    reservation.Action.ReqID,
		OwnerUID: reservation.Action.OwnerUID,
	}
	if reservation.Action.Kind == Steal {
		crop, ok := gameconf.CropByID(reservation.Action.CropID)
		if !ok {
			return pkgerr.BadRequest
		}
		// 赔付额按本地配置重算，不采信请求里带来的数字：这是访客自己要掏的钱，
		// 让上游声明金额等于把解冻额度的决定权交出去。
		entry.Steal = true
		entry.FrozenCoin = gameconf.StealCompensation(crop)
	} else {
		kind, ok := ownerPlotActionKind(reservation.Action.Kind)
		if !ok {
			return pkgerr.BadRequest
		}
		entry.MaintainKind = kind
		entry.DayID = reservation.DayID
	}

	_, code := aggregate.ReserveCross(entry, now)
	return code
}

// SettleVisitor 按主人侧回执结算访客预占，返回收益与个人状态增量。
//
// 预占从聚合里取走后即删除，因此重复投递的同一条回执会落到「取不到」分支，
// 天然幂等；返回 pkgerr.Timeout 表示该预占已被超时回滚，收益不再补发。
func SettleVisitor(
	aggregate *farm.Aggregate,
	result CrossResult,
	now int64,
) (VisitorReward, *farm.PlayerDelta, pkgerr.Code) {
	reward := VisitorReward{ReqID: result.ReqID}
	if aggregate == nil || result.VisitorUID != aggregate.UID {
		return reward, nil, pkgerr.BadRequest
	}

	aggregate.ExpireCrossPending(now)
	reservation, ok := aggregate.TakeCrossReservation(result.ReqID, result.OwnerUID)
	if !ok {
		return reward, nil, pkgerr.Timeout
	}

	if reservation.Steal {
		return settleVisitorSteal(aggregate, result, reservation)
	}
	return settleVisitorMaintenance(aggregate, result, reservation)
}

func settleVisitorSteal(
	aggregate *farm.Aggregate,
	result CrossResult,
	reservation farm.CrossReservation,
) (VisitorReward, *farm.PlayerDelta, pkgerr.Code) {
	reward := VisitorReward{ReqID: result.ReqID}
	code := result.Code

	switch result.Code {
	case pkgerr.OK:
		if result.CropID == 0 || result.Amount == 0 {
			// 主人侧报成功却没给果实：按内部错误解冻，宁可不赚也不错扣。
			aggregate.RollbackCross(reservation)
			code = pkgerr.Internal
			break
		}
		aggregate.RollbackCross(reservation)
		aggregate.AddItem(farm.FruitItem(result.CropID), uint32(result.Amount))
		reward.CropID = result.CropID
		reward.Amount = result.Amount
	case pkgerr.StealIntercepted:
		// 冻结额转为赔付，主人侧已同额入账，这里不解冻。金额取自本地预占记录，
		// 与主人侧收到的是同一个数（都由 gameconf.StealCompensation 算出）。
		reward.Compensation = reservation.FrozenCoin
		reward.DogType = result.DogType
	default:
		aggregate.RollbackCross(reservation)
	}

	delta := aggregate.PlayerDelta()
	return reward, &delta, code
}

func settleVisitorMaintenance(
	aggregate *farm.Aggregate,
	result CrossResult,
	reservation farm.CrossReservation,
) (VisitorReward, *farm.PlayerDelta, pkgerr.Code) {
	reward := VisitorReward{ReqID: result.ReqID}
	if result.Code != pkgerr.OK {
		aggregate.RollbackCross(reservation)
		return reward, nil, result.Code
	}

	exp, coin := aggregate.SettleMaintenance(reservation.Rewarded, reservation.MaintainKind)
	if !reservation.Rewarded {
		// 动作在主人侧已生效，但当日 150 次奖励额度已用尽，不产生个人状态变化。
		return reward, nil, pkgerr.OK
	}
	reward.ExpGained, reward.CoinGained = exp, coin

	delta := aggregate.PlayerDelta()
	return reward, &delta, pkgerr.OK
}
