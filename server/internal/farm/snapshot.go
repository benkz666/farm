package farm

// PlotSnapshot 是 EnterFarm 快照中单块地的期 1 最小视图（规格 6.3）。
type PlotSnapshot struct {
	Index  uint8  `json:"index"`
	State  uint8  `json:"state"`
	CropID uint16 `json:"crop_id"`
}

// FarmSnapshotJSON 是 EnterFarm 返回的全量农场快照（规格 6.3）。
// relation 由 gateway 填入 EnterFarmRsp，不在此结构内。
type FarmSnapshotJSON struct {
	OwnerUID      uint64         `json:"owner_uid"`
	Nickname      string         `json:"nickname"`
	Level         uint16         `json:"level"`
	Exp           uint32         `json:"exp"`
	Coin          int64          `json:"coin"`
	UnlockedPlots uint8          `json:"unlocked_plots"`
	Plots         []PlotSnapshot `json:"plots"`
}

// Snapshot 将聚合投影为协议/JSON 形状的 FarmSnapshotJSON。
// plots 固定长度为 MaxPlots（18）；state 使用数值枚举（0=wasteland）。
func (a *Aggregate) Snapshot() FarmSnapshotJSON {
	plots := make([]PlotSnapshot, len(a.Plots))
	for i := range a.Plots {
		plots[i] = PlotSnapshot{
			Index:  uint8(i),
			State:  a.Plots[i].State,
			CropID: a.Plots[i].CropID,
		}
	}
	return FarmSnapshotJSON{
		OwnerUID:      a.UID,
		Nickname:      a.Nickname,
		Level:         a.Level,
		Exp:           a.Exp,
		Coin:          a.Coin,
		UnlockedPlots: a.UnlockedPlots,
		Plots:         plots,
	}
}
