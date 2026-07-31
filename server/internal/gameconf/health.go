// 健康度与照料相关常量（期 2）。
//
// 数值对照 docs/design/game-design-full.md 18.3 节及
// client/src/game/config.js。所有参数按「本季生长时长的比例」定义，
// 保证短周期与长周期作物照料难度一致（见策划 18.3 节末段）。
//
// advance 在 Growing 阶段惰性结算水分/杂草/害虫时使用这些常量。
package gameconf

// 健康度三项权重，合计 1.0。
const (
	WeightDry  float64 = 0.44
	WeightWeed float64 = 0.26
	WeightPest float64 = 0.30
)

// YieldFloor 是健康度对应的产量系数下限，低于此值按此值计。
const YieldFloor float64 = 0.60

// WaterSpanRatio 水分持续时长 = 本季 × 35%。
const WaterSpanRatio float64 = 0.35

// RiskWindowRatio 风险窗口 = 本季 × 10%，杂草/害虫按窗口逐个判定。
const RiskWindowRatio float64 = 0.10

// RiskWindowsPerSeason 策划 7.2：每季恰好 10 个风险窗口。
const RiskWindowsPerSeason = 10

// HazardRollMod 与架构 5.4 hazardHit 一致：xxhash%10000 与阈值比较。
const HazardRollMod = 10000

// 单个风险窗口的生草/生虫概率。
const (
	WeedChance float64 = 0.12
	PestChance float64 = 0.10
)

// 架构 5.4：阈值 = 概率 × 10000。
const (
	WeedHazardThreshold = 1200
	PestHazardThreshold = 1000
)
