package farm

// PlotSnapshot 是 EnterFarm / 动作补丁中的单地块视图（期 2 扩展成熟与照料字段）。
type PlotSnapshot struct {
	Index          uint8  `json:"index"`
	State          uint8  `json:"state"`
	CropID         uint16 `json:"crop_id"`
	SeasonIndex    uint8  `json:"season_index"`
	SeasonTotal    uint8  `json:"season_total"`
	SeasonStartAt  int64  `json:"season_start_at"`
	MatureAt       int64  `json:"mature_at"`
	SeasonDuration int64  `json:"season_duration"`
	FinalYield     uint16 `json:"final_yield"`
	LastSettleAt   int64  `json:"last_settle_at"`
	LastWaterAt    int64  `json:"last_water_at"`
	WeedSince      int64  `json:"weed_since"`
	PestSince      int64  `json:"pest_since"`
	Health         uint8  `json:"health"`
	StolenCount    uint16 `json:"stolen_count"`
	FertMask       uint8  `json:"fert_mask"`
}

// FarmSnapshotJSON 是 EnterFarm 返回的全量农场快照。
// relation 由 gateway 填入 EnterFarmRsp，不在此结构内。
type FarmSnapshotJSON struct {
	OwnerUID      uint64            `json:"owner_uid"`
	Nickname      string            `json:"nickname"`
	Level         uint16            `json:"level"`
	Exp           uint32            `json:"exp"`
	Coin          int64             `json:"coin"`
	UnlockedPlots uint8             `json:"unlocked_plots"`
	Plots         []PlotSnapshot    `json:"plots"`
	Bag           map[string]uint32 `json:"bag"`
	Warehouse     map[string]uint32 `json:"warehouse"`
}

// Snapshot 将聚合投影为协议/JSON 形状的 FarmSnapshotJSON。
// plots 固定长度为 MaxPlots（18）；state 使用数值枚举（0=wasteland）。
func (a *Aggregate) Snapshot() FarmSnapshotJSON {
	plots := make([]PlotSnapshot, len(a.Plots))
	for i := range a.Plots {
		plots[i] = PlotSnapshotOf(uint8(i), a.Plots[i])
	}
	bag, warehouse := SplitItems(a.Items)
	return FarmSnapshotJSON{
		OwnerUID:      a.UID,
		Nickname:      a.Nickname,
		Level:         a.Level,
		Exp:           a.Exp,
		Coin:          a.Coin,
		UnlockedPlots: a.UnlockedPlots,
		Plots:         plots,
		Bag:           bag,
		Warehouse:     warehouse,
	}
}

// VisitorSafeFarmSnapshot 去掉访客不应看到的个人经济字段（金币、经验、背包、仓库）。
// 保留昵称/等级/地块，供拜访 HUD 与偷菜判定使用。不修改入参。
func VisitorSafeFarmSnapshot(full FarmSnapshotJSON) FarmSnapshotJSON {
	plots := append([]PlotSnapshot(nil), full.Plots...)
	return FarmSnapshotJSON{
		OwnerUID:      full.OwnerUID,
		Nickname:      full.Nickname,
		Level:         full.Level,
		UnlockedPlots: full.UnlockedPlots,
		Plots:         plots,
	}
}

// PlotSnapshotOf 投影单地块。
func PlotSnapshotOf(index uint8, p Plot) PlotSnapshot {
	return PlotSnapshot{
		Index:          index,
		State:          p.State,
		CropID:         p.CropID,
		SeasonIndex:    p.SeasonIndex,
		SeasonTotal:    p.SeasonTotal,
		SeasonStartAt:  p.SeasonStartAt,
		MatureAt:       p.MatureAt,
		SeasonDuration: p.SeasonDuration,
		FinalYield:     p.FinalYield,
		LastSettleAt:   p.LastSettleAt,
		LastWaterAt:    p.LastWaterAt,
		WeedSince:      p.WeedSince,
		PestSince:      p.PestSince,
		Health:         PlotHealth(p),
		StolenCount:    p.StolenCount,
		FertMask:       p.FertMask,
	}
}

// SplitItems 将聚合 Items 拆成背包（种子）与仓库（果实）。
func SplitItems(items map[ItemKey]uint32) (bag, warehouse map[string]uint32) {
	bag = make(map[string]uint32)
	warehouse = make(map[string]uint32)
	for key, count := range items {
		if count == 0 {
			continue
		}
		s := string(key)
		switch {
		case len(s) >= 5 && s[:5] == "seed:":
			bag[s] = count
		case len(s) >= 6 && s[:6] == "fruit:":
			warehouse[s] = count
		default:
			bag[s] = count
		}
	}
	return bag, warehouse
}

// PatchJSON 是动作成功后的客户端补丁摘要。
type PatchJSON struct {
	PlotIndex uint8             `json:"plot_index"`
	Plot      *PlotSnapshot     `json:"plot,omitempty"`
	Coin      int64             `json:"coin"`
	Exp       uint32            `json:"exp"`
	Bag       map[string]uint32 `json:"bag"`
	Warehouse map[string]uint32 `json:"warehouse"`
	FarmSeq   uint64            `json:"farm_seq"`
	Codex     *CodexProgress    `json:"codex_progress,omitempty"`
}

// PatchFromAction 把 ActionPatch 转成可 JSON 序列化的补丁。
func (a *Aggregate) PatchFromAction(result ActionResult) PatchJSON {
	bag, warehouse := SplitItems(a.Items)
	patch := PatchJSON{
		PlotIndex: result.Patch.PlotIndex,
		Coin:      a.Coin,
		Exp:       a.Exp,
		Bag:       bag,
		Warehouse: warehouse,
		FarmSeq:   a.FarmSeq,
		Codex:     result.Patch.Codex,
	}
	// 商店动作用 plot_index=0 且未改地块时仍带当前 0 号地快照，便于客户端统一 apply。
	p := PlotSnapshotOf(result.Patch.PlotIndex, a.Plots[result.Patch.PlotIndex])
	patch.Plot = &p
	return patch
}
