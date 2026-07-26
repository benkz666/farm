package farm

import (
	"strconv"

	"farm/server/internal/gameconf"
)

// ItemKey 是背包或仓库中物品的稳定键。期 2a 先作为聚合内存状态；
// Task 4 会将同一语义映射到 item(uid, kind, item_id) 表。
type ItemKey string

const (
	seedItemPrefix  = "seed:"
	fruitItemPrefix = "fruit:"
)

// SeedItem 返回作物种子的背包键。
func SeedItem(cropID uint16) ItemKey {
	return ItemKey(seedItemPrefix + strconv.FormatUint(uint64(cropID), 10))
}

// FruitItem 返回作物果实的仓库键。
func FruitItem(cropID uint16) ItemKey {
	return ItemKey(fruitItemPrefix + strconv.FormatUint(uint64(cropID), 10))
}

// Aggregate 是期 1 落地的农场聚合最小子集（架构 5.1 节 FarmAggregate 的裁剪）。
// Items 保存种子背包与果实仓库；持久化由 Task 4 接入。
type Aggregate struct {
	UID           uint64                  `json:"owner_uid"`
	Nickname      string                  `json:"nickname"`
	Level         uint16                  `json:"level"`
	Exp           uint32                  `json:"exp"`
	Coin          int64                   `json:"coin"`
	UnlockedPlots uint8                   `json:"unlocked_plots"`
	Plots         [gameconf.MaxPlots]Plot `json:"plots"`
	Items         map[ItemKey]uint32      `json:"items"`
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
		Items:         make(map[ItemKey]uint32),
		FarmSeq:       0,
	}
	for i := range agg.Plots {
		agg.Plots[i] = NewWastelandPlot()
	}
	return agg
}
