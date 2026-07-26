package farm

import "farm/server/internal/gameconf"

// Aggregate 是期 1 落地的农场聚合最小子集（架构 5.1 节 FarmAggregate 的裁剪）。
// 只保留 Store/EnterFarm 快照所需字段；Items/Codex/Daily/Pet/Room 等留待后续期接入。
type Aggregate struct {
	UID           uint64                  `json:"owner_uid"`
	Nickname      string                  `json:"nickname"`
	Level         uint16                  `json:"level"`
	Exp           uint32                  `json:"exp"`
	Coin          int64                   `json:"coin"`
	UnlockedPlots uint8                   `json:"unlocked_plots"`
	Plots         [gameconf.MaxPlots]Plot `json:"plots"`
	FarmSeq       uint64                  `json:"farm_seq"`
}

// NewAggregate 按策划 4.2 节初始数值组装一份新农场聚合。
func NewAggregate(uid uint64, nickname string) *Aggregate {
	agg := &Aggregate{
		UID:           uid,
		Nickname:      nickname,
		Level:         0,
		Exp:           0,
		Coin:          gameconf.InitialCoin,
		UnlockedPlots: gameconf.InitialUnlockedPlots,
		FarmSeq:       0,
	}
	for i := range agg.Plots {
		agg.Plots[i] = NewWastelandPlot()
	}
	return agg
}
