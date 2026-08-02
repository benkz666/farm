package farm

// PlayerDelta is the current player-owned state sent outside a farm room.
// It deliberately excludes plots: room state continues to use FarmDelta.
type PlayerDelta struct {
	Coin      int64             `json:"coin"`
	Exp       uint32            `json:"exp"`
	Level     uint16            `json:"level"`
	Bag       map[string]uint32 `json:"bag"`
	Warehouse map[string]uint32 `json:"warehouse"`
	Pet       *PetStatus        `json:"pet,omitempty"`
}

// PlayerDelta snapshots personal resources after an authoritative mutation.
func (a *Aggregate) PlayerDelta() PlayerDelta {
	if a == nil {
		return PlayerDelta{}
	}
	bag, warehouse := SplitItems(a.Items)
	return PlayerDelta{
		Coin:      a.Coin,
		Exp:       a.Exp,
		Level:     a.Level,
		Bag:       bag,
		Warehouse: warehouse,
	}
}

// CreditReward mirrors a committed direct reward into the resident
// authoritative aggregate so a later Actor flush cannot overwrite durable
// state with an older snapshot.
func (a *Aggregate) CreditReward(coin int64, exp uint32) bool {
	if a == nil || coin < 0 || (coin == 0 && exp == 0) {
		return false
	}
	a.Coin += coin
	a.Exp += exp
	a.RecalcLevel()
	a.FarmSeq++
	return true
}

// CreditMailReward 保留邮件附件领取的语义封装。
func (a *Aggregate) CreditMailReward(amount int64) bool {
	return a.CreditReward(amount, 0)
}
