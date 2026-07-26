package farm

import "farm/server/internal/gameconf"

const healthPointsPerFullPenalty int64 = 100

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

	WitherSpanMultiplier int64
}

// NewAdvanceConfig 将 gameconf 的健康度参数转换为整数结算配置。
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
		WitherSpanMultiplier:  int64(gameconf.WitherSpanRatio),
	}
}

// Advance 将地块惰性推进到 now。调用方提供服务端时间，以保证测试和 smoke
// 不依赖真实等待；调用方应保证时间不早于 LastSettleAt。
func Advance(p *Plot, now int64, cfg AdvanceConfig) {
	if p == nil {
		return
	}

	for {
		switch p.State {
		case StateGrowing:
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

		case StateMature:
			witherAt := p.MatureAt + cfg.WitherSpanMultiplier*p.SeasonDuration
			if now < witherAt {
				return
			}

			p.State = StateWithered
			p.FinalYield = 0
			p.CropID = 0
			p.SeasonIndex = 0
			p.Stealers = nil
			return

		default:
			return
		}
	}
}

// settleTo 只结算 Growing 阶段的健康度。杂草和害虫的风险窗口配置保留在
// AdvanceConfig，后续动作接入确定性风险扫描时可复用该增量结算边界。
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
	if dryFrom < to {
		p.AccruedWeighted += cfg.DryWeight * (to - dryFrom)
	}
	p.LastSettleAt = to
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
