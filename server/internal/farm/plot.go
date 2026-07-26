// Package farm 定义农场聚合的领域结构（期 1 只承载数据布局，不实现 advance()）。
//
// 字段布局与 docs/design/architecture.md 5.2 节的 Plot 结构体保持一致，
// 避免期 2 引入 advance() 时推翻编解码格式。
package farm

// 六态状态机（策划 game-design-full.md 5.1 节），数值即协议/存储中的枚举值。
const (
	StateWasteland uint8 = iota // 荒地：未开垦，不能播种
	StateTilled                 // 空闲已翻：已松土，可播种
	StateGrowing                // 生长中：作物正在生长，可照料
	StateMature                 // 成熟：可收获，也可被访客偷取
	StateResidue                // 待清理：末季收获完成，残留植株待清理
	StateWithered               // 枯萎：成熟后超时未收获，本次产量全部作废
)

// Plot 对应架构 5.2 节的地块结构体。期 1 只写入/读出 State、CropID，
// 其余字段按零值预留，保证期 2 接入 advance() 时字段序不变。
//
// 全部时间字段为毫秒级 UNIX 时间戳，全部时长字段为已按 TIME_SCALE 折算的绝对毫秒数。
type Plot struct {
	State       uint8 `json:"state"`         // 六态状态机
	SeasonIndex uint8 `json:"season_index"`  // 当前第几季，0-based
	SeasonTotal uint8 `json:"season_total"`  // 总季数，冗余自配置
	StageCount  uint8 `json:"stage_count"`   // 生长阶段数 3 或 4，冗余自配置
	FertMask    uint8 `json:"fert_mask"`     // 各阶段是否已施肥的位掩码
	WeedNextWin uint8 `json:"weed_next_win"` // 下一个待判定的杂草风险窗口序号
	PestNextWin uint8 `json:"pest_next_win"` // 下一个待判定的害虫风险窗口序号
	_           uint8 // 对齐占位

	CropID      uint16 `json:"crop_id"`
	FinalYield  uint16 `json:"final_yield"`  // 跨越成熟点时固化的实际产量
	StolenCount uint16 `json:"stolen_count"` // 本轮已被偷走的数量
	_           uint16 // 对齐占位

	PlantNonce   uint32 `json:"plant_nonce"`   // 每次播种由 CSPRNG 生成，参与随机种子构造
	HarvestRound uint32 `json:"harvest_round"` // 成熟轮次，偷菜去重的作用域

	SeasonStartAt   int64 `json:"season_start_at"`  // 本季开始时刻，风险窗口的时间原点
	SeasonDuration  int64 `json:"season_duration"`  // 本季「名义」生长时长，施肥不改变它
	MatureAt        int64 `json:"mature_at"`        // 实际成熟时刻，施肥时前移
	LastSettleAt    int64 `json:"last_settle_at"`   // 健康度上次结算时刻
	LastWaterAt     int64 `json:"last_water_at"`    // 上次浇水时刻，播种视为浇水
	WeedSince       int64 `json:"weed_since"`       // 杂草出现时刻，0 表示无草
	PestSince       int64 `json:"pest_since"`       // 害虫出现时刻，0 表示无虫
	AccruedWeighted int64 `json:"accrued_weighted"` // 累计加权扣减，单位「百分点·毫秒」

	Stealers []uint32 `json:"stealers,omitempty"` // 本轮已偷过的 uid，通常为 nil
}

// NewWastelandPlot 返回一块全字段置零的荒地，用于新农场初始化。
func NewWastelandPlot() Plot {
	return Plot{State: StateWasteland}
}
