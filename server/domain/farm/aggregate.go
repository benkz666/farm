package farm

import (
	"strconv"

	"farm/server/shared/gameconfig"
)

// ItemKey 是背包或仓库中物品的稳定键。期 2a 先作为聚合内存状态；
// Task 4 会将同一语义映射到 item(uid, kind, item_id) 表。
type ItemKey string

const (
	seedItemPrefix       = "seed:"
	fertilizerItemPrefix = "fert:"
	dogFoodItemPrefix    = "dogfood:"
	fruitItemPrefix      = "fruit:"
)

// SeedItem 返回作物种子的背包键。
func SeedItem(cropID uint16) ItemKey {
	return ItemKey(seedItemPrefix + strconv.FormatUint(uint64(cropID), 10))
}

// FertilizerItem 返回化肥背包键。
func FertilizerItem(fertilizerID uint16) ItemKey {
	return ItemKey(fertilizerItemPrefix + strconv.FormatUint(uint64(fertilizerID), 10))
}

// DogFoodItem returns the single dog-food item key. Food is measured in grams.
func DogFoodItem() ItemKey {
	return ItemKey(dogFoodItemPrefix + "1")
}

// FruitItem 返回作物果实的仓库键。
func FruitItem(cropID uint16) ItemKey {
	return ItemKey(fruitItemPrefix + strconv.FormatUint(uint64(cropID), 10))
}

// Aggregate 是期 1 落地的农场聚合最小子集（架构 5.1 节 FarmAggregate 的裁剪）。
// Items 保存种子背包与果实仓库；持久化由 Task 4 接入。
type Aggregate struct {
	UID           uint64                    `json:"owner_uid"`
	Nickname      string                    `json:"nickname"`
	Level         uint16                    `json:"level"`
	Exp           uint32                    `json:"exp"`
	Coin          int64                     `json:"coin"`
	UnlockedPlots uint8                     `json:"unlocked_plots"`
	Plots         [gameconfig.MaxPlots]Plot `json:"plots"`
	Items         map[ItemKey]uint32        `json:"items"`
	Daily         DailyState                `json:"daily"`
	Pet           PetState                  `json:"pet"`
	CodexHarvests map[uint16]uint32         `json:"codex_harvests,omitempty"`
	FarmSeq       uint64                    `json:"farm_seq"`

	// CrossPending 是本玩家作为访客时尚未结算的跨农场预占，键为 req_id。
	// 它必须与聚合一起持久化，否则冻结的金币会随进程一起消失，见 cross_pending.go。
	CrossPending map[uint64]CrossReservation `json:"cross_pending,omitempty"`

	// CrossReceipts 是本玩家作为主人时已裁决的跨农场动作结果。它与地块变更一同
	// 持久化，使 Actor 卸载或消息重投后仍能返回原始裁决，而不是重复执行业务动作。
	CrossReceipts map[uint64]CrossReceipt `json:"cross_receipts,omitempty"`

	// HazardSalt 是草/虫确定性哈希盐（架构 5.4），由 Runtime 在加载时注入。
	// 非持久化、不下发协议：json:"-" 保证不进 Redis 缓存 / 存档 / 快照。
	HazardSalt uint64 `json:"-"`
}

// NewAggregate 按策划 4.2 节初始数值组装一份新农场聚合。
func NewAggregate(uid uint64, nickname string) *Aggregate {
	agg := &Aggregate{
		UID:           uid,
		Nickname:      nickname,
		Level:         0,
		Exp:           0,
		Coin:          gameconfig.InitialCoin,
		UnlockedPlots: gameconfig.InitialUnlockedPlots,
		Items:         make(map[ItemKey]uint32),
		CodexHarvests: make(map[uint16]uint32),
		FarmSeq:       0,
	}
	for i := range agg.Plots {
		agg.Plots[i] = NewWastelandPlot()
	}
	return agg
}

// AddItem 向背包累加物品，并在 Items 尚未初始化时补建。
// 从 Redis 缓存反序列化出的聚合可能没有 items 字段，直接写 map 会 panic。
func (a *Aggregate) AddItem(key ItemKey, count uint32) {
	if a == nil || count == 0 {
		return
	}
	if a.Items == nil {
		a.Items = make(map[ItemKey]uint32, 1)
	}
	a.Items[key] += count
}

// RecalcLevel 按与客户端一致的规则刷新等级：Level = Exp / ExpPerLevel。
func (a *Aggregate) RecalcLevel() {
	if a == nil {
		return
	}
	a.Level = uint16(a.Exp / gameconfig.ExpPerLevel)
}
