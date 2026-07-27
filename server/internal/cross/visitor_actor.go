package cross

import (
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

// VisitorReservation describes resources that must be reserved on the Farm
// authoritative for the visitor before a CrossAction is published.
type VisitorReservation struct {
	Action CrossAction `json:"action"`
	DayID  uint32      `json:"day_id,omitempty"`
}

// VisitorSettlement carries the reservation metadata needed to commit or roll
// back the visitor Actor after the owner's CrossResult arrives.
type VisitorSettlement struct {
	Result          CrossResult         `json:"result"`
	DayID           uint32              `json:"day_id,omitempty"`
	Rewarded        bool                `json:"rewarded,omitempty"`
	MaintenanceKind farm.PlotActionKind `json:"maintenance_kind,omitempty"`
	Steal           bool                `json:"steal,omitempty"`
	FrozenCoin      int64               `json:"frozen_coin,omitempty"`
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

// ReserveVisitor mutates only the visitor aggregate. The Gateway retains the
// transport pending state, while this function works in both all-in-one and
// sharded Farm processes.
func ReserveVisitor(aggregate *farm.Aggregate, reservation VisitorReservation) (bool, pkgerr.Code) {
	if aggregate == nil || reservation.Action.VisitorUID != aggregate.UID {
		return false, pkgerr.BadRequest
	}
	if reservation.Action.Kind == Steal {
		return false, aggregate.FreezeStealCompensation(reservation.Action.Compensation)
	}
	if _, ok := ownerPlotActionKind(reservation.Action.Kind); !ok {
		return false, pkgerr.BadRequest
	}
	return aggregate.ReserveMaintenance(reservation.DayID), pkgerr.OK
}

// SettleVisitor commits rewards or rolls back reservations on the visitor
// aggregate and returns the authoritative personal-state delta when changed.
func SettleVisitor(
	aggregate *farm.Aggregate,
	settlement VisitorSettlement,
) (VisitorReward, *farm.PlayerDelta, pkgerr.Code) {
	reward := VisitorReward{ReqID: settlement.Result.ReqID}
	if aggregate == nil || settlement.Result.VisitorUID != aggregate.UID {
		return reward, nil, pkgerr.BadRequest
	}
	code := settlement.Result.Code
	if settlement.Steal {
		switch settlement.Result.Code {
		case pkgerr.OK:
			if settlement.Result.CropID == 0 || settlement.Result.Amount == 0 {
				aggregate.ReleaseStealCompensation(settlement.FrozenCoin)
				code = pkgerr.Internal
				break
			}
			aggregate.ReleaseStealCompensation(settlement.FrozenCoin)
			aggregate.Items[farm.FruitItem(settlement.Result.CropID)] += uint32(settlement.Result.Amount)
			reward.CropID = settlement.Result.CropID
			reward.Amount = settlement.Result.Amount
		case pkgerr.StealIntercepted:
			reward.Compensation = settlement.Result.Compensation
			reward.DogType = settlement.Result.DogType
		default:
			aggregate.ReleaseStealCompensation(settlement.FrozenCoin)
		}
		delta := aggregate.PlayerDelta()
		return reward, &delta, code
	}

	if settlement.Result.Code == pkgerr.OK {
		aggregate.SettleMaintenance(settlement.DayID, settlement.Rewarded, settlement.MaintenanceKind)
		if settlement.Rewarded {
			reward.ExpGained = 2
			if settlement.MaintenanceKind == farm.Weed || settlement.MaintenanceKind == farm.Pest {
				reward.CoinGained = 5
			}
			delta := aggregate.PlayerDelta()
			return reward, &delta, code
		}
		return reward, nil, code
	}
	aggregate.RollbackMaintenance(settlement.DayID, settlement.Rewarded)
	return reward, nil, code
}
