package farm

import (
	"encoding/binary"

	"farm/server/internal/gameconf"
	"github.com/cespare/xxhash/v2"
)

const healthPointsPerFullPenalty int64 = 100

// 架构 5.4：hazardHit 的 kind 字节，区分草/虫判定序列。
const (
	HazardWeed uint8 = 0
	HazardPest uint8 = 1
)

// AdvanceConfig 固化一季作物结算所需的配置。播种时应从当时的作物配置创建，
// 使配置热更不会改变在途作物的产量规则。
type AdvanceConfig struct {
	BaseYield uint16

	DryWeight  int64
	WeedWeight int64
	PestWeight int64

	WaterSpanNumerator    int64
	WaterSpanDenominator  int64
	RiskWindowNumerator   int64
	RiskWindowDenominator int64

	// 确定性草/虫：阈值与哈希上下文（架构 5.4）。
	WeedThreshold int64
	PestThreshold int64
	RiskWindows   uint8
	OwnerUID      uint64
	PlotIndex     uint8
	HazardSalt    uint64
}

// NewAdvanceConfig 将 gameconf 的健康度参数转换为整数结算配置。
// HazardSalt / OwnerUID / PlotIndex 必须由调用方注入，禁止在此回退到任何公开默认盐。
func NewAdvanceConfig(crop gameconf.CropConf) AdvanceConfig {
	return AdvanceConfig{
		BaseYield:             crop.Yield,
		DryWeight:             int64(gameconf.WeightDry * float64(healthPointsPerFullPenalty)),
		WeedWeight:            int64(gameconf.WeightWeed * float64(healthPointsPerFullPenalty)),
		PestWeight:            int64(gameconf.WeightPest * float64(healthPointsPerFullPenalty)),
		WaterSpanNumerator:    int64(gameconf.WaterSpanRatio * float64(healthPointsPerFullPenalty)),
		WaterSpanDenominator:  healthPointsPerFullPenalty,
		RiskWindowNumerator:   int64(gameconf.RiskWindowRatio * float64(healthPointsPerFullPenalty)),
		RiskWindowDenominator: healthPointsPerFullPenalty,
		WeedThreshold:         gameconf.WeedHazardThreshold,
		PestThreshold:         gameconf.PestHazardThreshold,
		RiskWindows:           gameconf.RiskWindowsPerSeason,
	}
}

// DeriveHazardSalt 把 FARM_HAZARD_SECRET 字符串稳定映射为 uint64 盐。
// 同一秘密跨进程/跨重启结果一致；不得把原始秘密写入存档或协议。
func DeriveHazardSalt(secret string) uint64 {
	return xxhash.Sum64String(secret)
}

// Advance 将地块惰性推进到 now。调用方提供服务端时间，以保证测试和 smoke
// 不依赖真实等待；调用方应保证时间不早于 LastSettleAt。
func Advance(p *Plot, now int64, cfg AdvanceConfig) {
	if p == nil {
		return
	}

	if p.State != StateGrowing {
		// 普通作物成熟后永久保持可收获状态；跨季只由收获动作触发。
		return
	}

	// 未播种完整的地块（缺 MatureAt/时长）不随时间演化，避免脏数据误成熟。
	if p.MatureAt <= 0 || p.SeasonDuration <= 0 {
		return
	}
	if now < p.MatureAt {
		settleTo(p, now, cfg)
		return
	}

	settleTo(p, p.MatureAt, cfg)
	p.FinalYield = computeFinalYield(p, cfg)
	p.State = StateMature
	p.HarvestRound++
	p.StolenCount = 0
	p.Stealers = p.Stealers[:0]
}

// settleTo 把健康度结算推进到 to。调用方保证 to <= MatureAt（成熟点由 Advance 截断）。
func settleTo(p *Plot, to int64, cfg AdvanceConfig) {
	from := p.LastSettleAt
	if to <= from {
		return
	}
	if p.SeasonDuration <= 0 || cfg.WaterSpanDenominator <= 0 {
		p.LastSettleAt = to
		return
	}

	waterSpan := p.SeasonDuration * cfg.WaterSpanNumerator / cfg.WaterSpanDenominator
	dryFrom := p.LastWaterAt + waterSpan
	if dryFrom < from {
		dryFrom = from
	}
	var dryMs int64
	if dryFrom < to {
		dryMs = to - dryFrom
	}

	weedMs := scanHazard(p, from, to, &p.WeedSince, &p.WeedNextWin, HazardWeed, cfg)
	pestMs := scanHazard(p, from, to, &p.PestSince, &p.PestNextWin, HazardPest, cfg)

	p.AccruedWeighted += cfg.DryWeight*dryMs + cfg.WeedWeight*weedMs + cfg.PestWeight*pestMs
	p.LastSettleAt = to
}

// scanHazard 返回 [from, to) 内该类不良状态持续毫秒，并惰性判定尚未扫描的风险窗口。
// 见架构 5.4：区间内至多一次「无→有」，永不「有→无」。
func scanHazard(p *Plot, from, to int64, since *int64, nextWin *uint8, kind uint8, cfg AdvanceConfig) int64 {
	windows := cfg.RiskWindows
	if windows == 0 {
		windows = gameconf.RiskWindowsPerSeason
	}
	if *since == 0 {
		windowLen := p.SeasonDuration / int64(windows)
		if windowLen <= 0 {
			return 0
		}
		for k := *nextWin; k < windows; k++ {
			wStart := p.SeasonStartAt + int64(k)*windowLen
			if wStart >= to {
				break
			}
			*nextWin = k + 1
			if hazardHit(cfg, p, kind, k) {
				*since = wStart
				break
			}
		}
	}
	if *since == 0 {
		return 0
	}
	start := *since
	if start < from {
		start = from
	}
	if start >= to {
		return 0
	}
	return to - start
}

// clearHazard 由除草/除虫调用，恢复「已判定窗口」不变式，避免刚清除又在同窗口重生。
func clearHazard(p *Plot, since *int64, nextWin *uint8, t int64, cfg AdvanceConfig) {
	*since = 0
	windows := cfg.RiskWindows
	if windows == 0 {
		windows = gameconf.RiskWindowsPerSeason
	}
	windowLen := p.SeasonDuration / int64(windows)
	if windowLen <= 0 {
		*nextWin = windows
		return
	}
	k := (t-p.SeasonStartAt)/windowLen + 1
	if k > int64(windows) {
		k = int64(windows)
	}
	if k < 0 {
		k = 0
	}
	if uint8(k) > *nextWin {
		*nextWin = uint8(k)
	}
}

func hazardRoll(cfg AdvanceConfig, p *Plot, kind, window uint8) uint64 {
	var buf [24]byte
	binary.LittleEndian.PutUint64(buf[0:], cfg.OwnerUID)
	binary.LittleEndian.PutUint32(buf[8:], p.PlantNonce)
	buf[12] = cfg.PlotIndex
	buf[13] = p.SeasonIndex
	buf[14] = kind
	buf[15] = window
	binary.LittleEndian.PutUint64(buf[16:], cfg.HazardSalt)
	return xxhash.Sum64(buf[:])
}

func hazardHit(cfg AdvanceConfig, p *Plot, kind, window uint8) bool {
	threshold := cfg.WeedThreshold
	if kind == HazardPest {
		threshold = cfg.PestThreshold
	}
	if threshold <= 0 {
		return false
	}
	return hazardRoll(cfg, p, kind, window)%gameconf.HazardRollMod < uint64(threshold)
}

func computeFinalYield(p *Plot, cfg AdvanceConfig) uint16 {
	if p.SeasonDuration <= 0 {
		return 0
	}

	accrued := p.AccruedWeighted
	maxAccrued := healthPointsPerFullPenalty * p.SeasonDuration
	if accrued < 0 {
		accrued = 0
	}
	if accrued > maxAccrued {
		accrued = maxAccrued
	}

	const yieldScale int64 = 250
	denominator := yieldScale * p.SeasonDuration
	return uint16(int64(cfg.BaseYield) * (denominator - accrued) / denominator)
}

// PlotHealth 按策划 7.3 / 架构 5.5：健康度 = 100 - AccruedWeighted / SeasonDuration。
func PlotHealth(p Plot) uint8 {
	if p.SeasonDuration <= 0 {
		return 100
	}
	deducted := p.AccruedWeighted / p.SeasonDuration
	if deducted <= 0 {
		return 100
	}
	if deducted >= healthPointsPerFullPenalty {
		return 0
	}
	return uint8(healthPointsPerFullPenalty - deducted)
}
