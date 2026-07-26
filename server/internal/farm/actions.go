package farm

import (
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

// 默认时间档：期 2 单测与 demo 一致；Gateway 后续可按配置覆盖。
const defaultTimeProfile = gameconf.TimeProfileDemo

// 清理失败（误铲生长中作物）扣减的健康度百分点。
// AccruedWeighted 单位为「百分点·毫秒」，故惩罚 = 点数 × SeasonDuration。
const clearFailPenaltyPoints int64 = 10

// PlotActionKind 是地块动作种类。
type PlotActionKind uint8

const (
	Till PlotActionKind = iota + 1
	Clear
	Plant
	Water
	Weed
	Pest
	Fertilize
	Harvest
)

func (k PlotActionKind) String() string {
	switch k {
	case Till:
		return "Till"
	case Clear:
		return "Clear"
	case Plant:
		return "Plant"
	case Water:
		return "Water"
	case Weed:
		return "Weed"
	case Pest:
		return "Pest"
	case Fertilize:
		return "Fertilize"
	case Harvest:
		return "Harvest"
	default:
		return "PlotActionKind(?)"
	}
}

// PlotAction 是一次地块意图。Plant 时 Arg 为 crop_id，Fertilize 时为 fertilizer_id。
type PlotAction struct {
	Kind      PlotActionKind
	PlotIndex uint8
	Arg       uint16
}

// ActionPatch 描述一次成功（或带副作用失败）动作后，客户端可应用的变更摘要。
// 期 2 Gateway 再决定序列化字段；领域层先填齐内存可见变更。
type ActionPatch struct {
	PlotIndex uint8
	Plot      Plot
	Coin      int64
	Exp       uint32
	Items     map[ItemKey]uint32
}

// ActionResult 是 validate/commit 的统一返回。
type ActionResult struct {
	Err   pkgerr.Code
	Patch ActionPatch
}

// ApplyPlotAction 在副本上 advance 后 validate；仅成功（或清理误铲扣血）写回聚合。
func (a *Aggregate) ApplyPlotAction(act PlotAction, now int64) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if int(act.PlotIndex) >= int(a.UnlockedPlots) || int(act.PlotIndex) >= len(a.Plots) {
		return ActionResult{Err: pkgerr.PlotNotFound}
	}

	work := a.Plots[act.PlotIndex]
	advancePlot(&work, now)

	switch act.Kind {
	case Till:
		return a.commitTill(act.PlotIndex, &work)
	case Clear:
		return a.commitClear(act.PlotIndex, &work)
	case Plant:
		return a.commitPlant(act.PlotIndex, &work, act.Arg, now)
	case Water:
		return a.commitWater(act.PlotIndex, &work, now)
	case Weed:
		return a.commitWeed(act.PlotIndex, &work, now)
	case Pest:
		return a.commitPest(act.PlotIndex, &work, now)
	case Fertilize:
		return a.commitFertilize(act.PlotIndex, &work, act.Arg, now)
	case Harvest:
		return a.commitHarvest(act.PlotIndex, &work, now)
	default:
		return ActionResult{Err: pkgerr.BadRequest}
	}
}

func advancePlot(p *Plot, now int64) {
	if p.CropID != 0 {
		if crop, ok := gameconf.CropByID(p.CropID); ok {
			Advance(p, now, NewAdvanceConfig(crop))
			return
		}
	}
	Advance(p, now, AdvanceConfig{})
}

// AdvanceAll 将所有已种植地块惰性推进到 now，供 EnterFarm 等读路径生成最新快照。
func (a *Aggregate) AdvanceAll(now int64) {
	if a == nil {
		return
	}
	for i := range a.Plots {
		p := &a.Plots[i]
		if p.State == StateGrowing || p.State == StateMature {
			advancePlot(p, now)
		}
	}
}

func (a *Aggregate) commitTill(idx uint8, work *Plot) ActionResult {
	if work.State != StateWasteland {
		return ActionResult{Err: pkgerr.PlotNotWasteland}
	}
	a.Plots[idx] = Plot{State: StateTilled}
	a.Exp += 3
	a.RecalcLevel()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitClear(idx uint8, work *Plot) ActionResult {
	switch work.State {
	case StateResidue, StateWithered:
		a.Plots[idx] = Plot{State: StateTilled}
		a.Exp += 3
		a.RecalcLevel()
		a.FarmSeq++
		return a.okPatch(idx)

	case StateGrowing:
		// 误铲：写回已 advance 的地块并扣健康度，返回不可清理。
		if work.SeasonDuration > 0 {
			work.AccruedWeighted += clearFailPenaltyPoints * work.SeasonDuration
		}
		a.Plots[idx] = *work
		a.FarmSeq++
		return ActionResult{
			Err:   pkgerr.PlotNotCleanable,
			Patch: a.patchOf(idx),
		}

	default:
		return ActionResult{Err: pkgerr.PlotNotCleanable}
	}
}

func (a *Aggregate) commitPlant(idx uint8, work *Plot, cropID uint16, now int64) ActionResult {
	if work.State != StateTilled {
		return ActionResult{Err: pkgerr.PlotNotTilled}
	}
	crop, ok := gameconf.CropByID(cropID)
	if !ok {
		return ActionResult{Err: pkgerr.BadRequest}
	}
	if a.Level < uint16(crop.UnlockLevel) {
		return ActionResult{Err: pkgerr.CropLocked}
	}
	seedKey := SeedItem(cropID)
	if a.Items[seedKey] == 0 {
		return ActionResult{Err: pkgerr.SeedNotOwned}
	}

	a.Items[seedKey]--
	if a.Items[seedKey] == 0 {
		delete(a.Items, seedKey)
	}

	duration := seasonDuration(crop, 0)
	a.Plots[idx] = Plot{
		State:          StateGrowing,
		SeasonIndex:    0,
		SeasonTotal:    crop.Seasons,
		StageCount:     stageCount(crop),
		CropID:         cropID,
		SeasonStartAt:  now,
		SeasonDuration: duration,
		MatureAt:       now + duration,
		LastSettleAt:   now,
		LastWaterAt:    now,
	}
	a.Exp += 2
	a.RecalcLevel()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitWater(idx uint8, work *Plot, now int64) ActionResult {
	switch work.State {
	case StateMature:
		// 成熟照料空操作：不写回，避免无意义 FarmSeq 抖动。
		return ActionResult{Err: pkgerr.OK}
	case StateGrowing:
		if work.CropID == 0 {
			return ActionResult{Err: pkgerr.PlotEmpty}
		}
		cfg := NewAdvanceConfig(mustCrop(work.CropID))
		if waterFull(work, now, cfg) {
			return ActionResult{Err: pkgerr.AlreadyWatered}
		}
		settleTo(work, now, cfg)
		work.LastWaterAt = now
		a.Plots[idx] = *work
		a.Exp += 2
		a.RecalcLevel()
		a.FarmSeq++
		return a.okPatch(idx)
	default:
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
}

func (a *Aggregate) commitWeed(idx uint8, work *Plot, now int64) ActionResult {
	switch work.State {
	case StateMature:
		return ActionResult{Err: pkgerr.OK}
	case StateGrowing:
		if work.CropID == 0 {
			return ActionResult{Err: pkgerr.PlotEmpty}
		}
		if work.WeedSince == 0 {
			return ActionResult{Err: pkgerr.NoWeed}
		}
		cfg := NewAdvanceConfig(mustCrop(work.CropID))
		settleTo(work, now, cfg)
		work.WeedSince = 0
		a.Plots[idx] = *work
		a.Exp += 2
		a.RecalcLevel()
		a.FarmSeq++
		return a.okPatch(idx)
	default:
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
}

func (a *Aggregate) commitPest(idx uint8, work *Plot, now int64) ActionResult {
	switch work.State {
	case StateMature:
		return ActionResult{Err: pkgerr.OK}
	case StateGrowing:
		if work.CropID == 0 {
			return ActionResult{Err: pkgerr.PlotEmpty}
		}
		if work.PestSince == 0 {
			return ActionResult{Err: pkgerr.NoPest}
		}
		cfg := NewAdvanceConfig(mustCrop(work.CropID))
		settleTo(work, now, cfg)
		work.PestSince = 0
		a.Plots[idx] = *work
		a.Exp += 2
		a.RecalcLevel()
		a.FarmSeq++
		return a.okPatch(idx)
	default:
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
}

func (a *Aggregate) commitFertilize(idx uint8, work *Plot, fertilizerID uint16, now int64) ActionResult {
	if work.State != StateGrowing {
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
	if work.CropID == 0 {
		return ActionResult{Err: pkgerr.PlotEmpty}
	}
	fertilizer, ok := gameconf.FertilizerByID(fertilizerID)
	if !ok {
		return ActionResult{Err: pkgerr.BadRequest}
	}
	key := FertilizerItem(fertilizerID)
	if a.Items[key] == 0 {
		return ActionResult{Err: pkgerr.FertilizerNotOwned}
	}
	if work.SeasonDuration <= 0 || work.StageCount == 0 {
		return ActionResult{Err: pkgerr.Internal}
	}

	progress := work.SeasonDuration - (work.MatureAt - now)
	if progress < 0 {
		progress = 0
	}
	stage := progress * int64(work.StageCount) / work.SeasonDuration
	if stage >= int64(work.StageCount) {
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
	stageBit := uint8(1 << stage)
	if work.FertMask&stageBit != 0 {
		return ActionResult{Err: pkgerr.StageAlreadyFertilized}
	}
	stageEnd := (stage + 1) * work.SeasonDuration / int64(work.StageCount)
	reduce := fertilizer.ReduceDuration(defaultTimeProfile)
	if remaining := stageEnd - progress; reduce > remaining {
		reduce = remaining
	}
	if reduce <= 0 {
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}

	a.Items[key]--
	if a.Items[key] == 0 {
		delete(a.Items, key)
	}
	work.MatureAt -= reduce
	work.FertMask |= stageBit
	a.Plots[idx] = *work
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitHarvest(idx uint8, work *Plot, now int64) ActionResult {
	if work.State == StateWithered {
		return ActionResult{Err: pkgerr.PlotWithered}
	}
	if work.State != StateMature {
		return ActionResult{Err: pkgerr.PlotNotMature}
	}
	cropID := work.CropID
	crop, ok := gameconf.CropByID(cropID)
	if !ok {
		return ActionResult{Err: pkgerr.Internal}
	}

	yield := work.FinalYield
	if work.StolenCount > yield {
		yield = 0
	} else {
		yield -= work.StolenCount
	}
	if yield > 0 {
		a.Items[FruitItem(cropID)] += uint32(yield)
	}
	a.Exp += crop.HarvestExp
	a.RecalcLevel()

	if work.SeasonIndex+1 < work.SeasonTotal {
		enterNextSeason(work, crop, now)
		a.Plots[idx] = *work
	} else {
		a.Plots[idx] = Plot{
			State:  StateResidue,
			CropID: cropID,
		}
	}
	a.FarmSeq++
	return a.okPatch(idx)
}

func enterNextSeason(p *Plot, crop gameconf.CropConf, now int64) {
	p.SeasonIndex++
	p.SeasonStartAt = now
	p.SeasonDuration = seasonDuration(crop, p.SeasonIndex)
	p.MatureAt = now + p.SeasonDuration
	p.LastSettleAt = now
	p.LastWaterAt = now
	p.AccruedWeighted = 0
	p.WeedSince, p.PestSince = 0, 0
	p.WeedNextWin, p.PestNextWin = 0, 0
	p.FertMask = 0
	p.FinalYield = 0
	p.StolenCount = 0
	p.Stealers = nil
	p.State = StateGrowing
}

func seasonDuration(crop gameconf.CropConf, seasonIndex uint8) int64 {
	return int64(gameconf.SeasonHours(crop, seasonIndex)) * gameconf.HourMs(defaultTimeProfile)
}

func stageCount(crop gameconf.CropConf) uint8 {
	if crop.UnlockLevel >= 3 {
		return 4
	}
	return 3
}

func waterFull(p *Plot, now int64, cfg AdvanceConfig) bool {
	if p.SeasonDuration <= 0 || cfg.WaterSpanDenominator <= 0 {
		return false
	}
	span := p.SeasonDuration * cfg.WaterSpanNumerator / cfg.WaterSpanDenominator
	return now < p.LastWaterAt+span
}

func mustCrop(id uint16) gameconf.CropConf {
	c, ok := gameconf.CropByID(id)
	if !ok {
		return gameconf.CropConf{}
	}
	return c
}

func (a *Aggregate) okPatch(idx uint8) ActionResult {
	return ActionResult{Err: pkgerr.OK, Patch: a.patchOf(idx)}
}

func (a *Aggregate) patchOf(idx uint8) ActionPatch {
	items := make(map[ItemKey]uint32, len(a.Items))
	for k, v := range a.Items {
		items[k] = v
	}
	return ActionPatch{
		PlotIndex: idx,
		Plot:      a.Plots[idx],
		Coin:      a.Coin,
		Exp:       a.Exp,
		Items:     items,
	}
}
