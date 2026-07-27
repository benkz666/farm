package farm

// PlayerDelta is the current player-owned state sent outside a farm room.
// It deliberately excludes plots: room state continues to use FarmDelta.
type PlayerDelta struct {
	Coin      int64             `json:"coin"`
	Exp       uint32            `json:"exp"`
	Level     uint16            `json:"level"`
	Bag       map[string]uint32 `json:"bag"`
	Warehouse map[string]uint32 `json:"warehouse"`
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
