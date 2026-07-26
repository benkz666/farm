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

// PlotActionKind 是地块动作种类（期 2a 主路径，不含 Fertilize）。
type PlotActionKind uint8

const (
	Till PlotActionKind = iota + 1
	Clear
	Plant
	Water
	Weed
	Pest
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
	case Harvest:
		return "Harvest"
	default:
		return "PlotActionKind(?)"
	}
}

// PlotAction 是一次地块意图。Plant 时 Arg 为 crop_id，其余为 0。
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

// ApplyPlotAction 先 advance 目标地块，再 validate/commit。
// 除「清理失败扣健康度」外，失败路径不改聚合。
func (a *Aggregate) ApplyPlotAction(act PlotAction, now int64) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if int(act.PlotIndex) >= int(a.UnlockedPlots) || int(act.PlotIndex) >= len(a.Plots) {
		return ActionResult{Err: pkgerr.PlotNotFound}
	}

	p := &a.Plots[act.PlotIndex]
	if p.CropID != 0 {
		if crop, ok := gameconf.CropByID(p.CropID); ok {
			Advance(p, now, NewAdvanceConfig(crop))
		}
	} else {
		Advance(p, now, AdvanceConfig{})
	}

	switch act.Kind {
	case Till:
		return a.commitTill(act.PlotIndex, now)
	case Clear:
		return a.commitClear(act.PlotIndex, now)
	case Plant:
		return a.commitPlant(act.PlotIndex, act.Arg, now)
	case Water:
		return a.commitWater(act.PlotIndex, now)
	case Weed:
		return a.commitWeed(act.PlotIndex, now)
	case Pest:
		return a.commitPest(act.PlotIndex, now)
	case Harvest:
		return a.commitHarvest(act.PlotIndex, now)
	default:
		return ActionResult{Err: pkgerr.BadRequest}
	}
}

func (a *Aggregate) commitTill(idx uint8, now int64) ActionResult {
	p := &a.Plots[idx]
	if p.State != StateWasteland {
		return ActionResult{Err: pkgerr.PlotNotWasteland}
	}
	*p = Plot{State: StateTilled}
	a.Exp += 3
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitClear(idx uint8, now int64) ActionResult {
	p := &a.Plots[idx]
	switch p.State {
	case StateResidue, StateWithered:
		*p = Plot{State: StateTilled}
		a.Exp += 3
		a.FarmSeq++
		return a.okPatch(idx)

	case StateGrowing:
		// 误铲生长中作物：返回不可清理，但按策划简规扣健康度（测试固定 10 百分点）。
		if p.SeasonDuration > 0 {
			p.AccruedWeighted += clearFailPenaltyPoints * p.SeasonDuration
		}
		a.FarmSeq++
		return ActionResult{
			Err:   pkgerr.PlotNotCleanable,
			Patch: a.patchOf(idx),
		}

	default:
		return ActionResult{Err: pkgerr.PlotNotCleanable}
	}
}

func (a *Aggregate) commitPlant(idx uint8, cropID uint16, now int64) ActionResult {
	p := &a.Plots[idx]
	if p.State != StateTilled {
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

	duration := int64(crop.CycleHours) * gameconf.HourMs(defaultTimeProfile)
	*p = Plot{
		State:          StateGrowing,
		SeasonIndex:    0,
		SeasonTotal:    crop.Seasons,
		StageCount:     3,
		CropID:         cropID,
		SeasonStartAt:  now,
		SeasonDuration: duration,
		MatureAt:       now + duration,
		LastSettleAt:   now,
		LastWaterAt:    now,
	}
	a.Exp += 2
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitWater(idx uint8, now int64) ActionResult {
	p := &a.Plots[idx]
	switch p.State {
	case StateMature:
		// 成熟地块照料为空操作（期 2 规格：允许 Water/Weed/Pest）。
		return ActionResult{Err: pkgerr.OK}
	case StateGrowing:
		if p.CropID == 0 {
			return ActionResult{Err: pkgerr.PlotEmpty}
		}
		cfg := NewAdvanceConfig(mustCrop(p.CropID))
		if waterFull(p, now, cfg) {
			return ActionResult{Err: pkgerr.AlreadyWatered}
		}
		settleTo(p, now, cfg)
		p.LastWaterAt = now
		a.Exp += 2
		a.FarmSeq++
		return a.okPatch(idx)
	default:
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
}

func (a *Aggregate) commitWeed(idx uint8, now int64) ActionResult {
	p := &a.Plots[idx]
	switch p.State {
	case StateMature:
		return ActionResult{Err: pkgerr.OK}
	case StateGrowing:
		if p.CropID == 0 {
			return ActionResult{Err: pkgerr.PlotEmpty}
		}
		if p.WeedSince == 0 {
			return ActionResult{Err: pkgerr.NoWeed}
		}
		cfg := NewAdvanceConfig(mustCrop(p.CropID))
		settleTo(p, now, cfg)
		p.WeedSince = 0
		a.Exp += 2
		a.FarmSeq++
		return a.okPatch(idx)
	default:
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
}

func (a *Aggregate) commitPest(idx uint8, now int64) ActionResult {
	p := &a.Plots[idx]
	switch p.State {
	case StateMature:
		return ActionResult{Err: pkgerr.OK}
	case StateGrowing:
		if p.CropID == 0 {
			return ActionResult{Err: pkgerr.PlotEmpty}
		}
		if p.PestSince == 0 {
			return ActionResult{Err: pkgerr.NoPest}
		}
		cfg := NewAdvanceConfig(mustCrop(p.CropID))
		settleTo(p, now, cfg)
		p.PestSince = 0
		a.Exp += 2
		a.FarmSeq++
		return a.okPatch(idx)
	default:
		return ActionResult{Err: pkgerr.PlotNotGrowing}
	}
}

func (a *Aggregate) commitHarvest(idx uint8, now int64) ActionResult {
	p := &a.Plots[idx]
	if p.State == StateWithered {
		return ActionResult{Err: pkgerr.PlotWithered}
	}
	if p.State != StateMature {
		return ActionResult{Err: pkgerr.PlotNotMature}
	}
	cropID := p.CropID
	crop, ok := gameconf.CropByID(cropID)
	if !ok {
		return ActionResult{Err: pkgerr.Internal}
	}

	yield := p.FinalYield
	if p.StolenCount > yield {
		yield = 0
	} else {
		yield -= p.StolenCount
	}
	if yield > 0 {
		fruit := FruitItem(cropID)
		a.Items[fruit] += uint32(yield)
	}
	a.Exp += crop.HarvestExp

	// 期 2a：单季作物收获后进入残茬；多季交 Task 10。
	*p = Plot{
		State:  StateResidue,
		CropID: cropID,
	}
	a.FarmSeq++
	return a.okPatch(idx)
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
