package farm

import (
	"crypto/rand"
	"encoding/binary"
	"time"

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
	Codex     *CodexProgress
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
	// 与地块推进同批惰性回滚超时预占：任何一次动作都会把冻结的金币放回账面，
	// 返回的 Patch 已携带 Coin，客户端无需额外一轮同步。
	a.ExpireCrossPending(now)

	work := a.Plots[act.PlotIndex]
	a.advancePlot(&work, now, act.PlotIndex)

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

func (a *Aggregate) advancePlot(p *Plot, now int64, plotIndex uint8) {
	cfg := a.plotAdvanceConfig(p.CropID)
	cfg.PlotIndex = plotIndex
	Advance(p, now, cfg)
}

// plotAdvanceConfig 组装地块推进配置，并从聚合注入非持久化 HazardSalt。
func (a *Aggregate) plotAdvanceConfig(cropID uint16) AdvanceConfig {
	cfg := AdvanceConfig{}
	if cropID != 0 {
		if crop, ok := gameconf.CropByID(cropID); ok {
			cfg = NewAdvanceConfig(crop)
		}
	}
	if a != nil {
		cfg.OwnerUID = a.UID
		cfg.HazardSalt = a.HazardSalt
	}
	return cfg
}

// AdvanceAll 将所有已种植地块惰性推进到 now，并返回发生可见变化的地块。
func (a *Aggregate) AdvanceAll(now int64) []PlotChange {
	if a == nil {
		return nil
	}
	// EnterFarm / SyncFarm 都经这里，是玩家重新上线后回滚超时预占的主要时机。
	a.ExpireCrossPending(now)

	changes := make([]PlotChange, 0)
	for i := range a.Plots {
		p := &a.Plots[i]
		if p.State == StateGrowing || p.State == StateMature {
			before := PlotSnapshotOf(uint8(i), *p)
			a.advancePlot(p, now, uint8(i))
			after := PlotSnapshotOf(uint8(i), *p)
			if before != after {
				changes = append(changes, plotChangeFromSnapshot(after))
			}
		}
	}
	if len(changes) > 0 {
		a.FarmSeq++
	}
	return changes
}

func plotChangeFromSnapshot(s PlotSnapshot) PlotChange {
	return PlotChange{
		Index:          s.Index,
		State:          s.State,
		CropID:         s.CropID,
		SeasonIndex:    s.SeasonIndex,
		SeasonTotal:    s.SeasonTotal,
		MatureAt:       s.MatureAt,
		SeasonDuration: s.SeasonDuration,
		FinalYield:     s.FinalYield,
		LastWaterAt:    s.LastWaterAt,
		WeedSince:      s.WeedSince,
		PestSince:      s.PestSince,
		Health:         s.Health,
		StolenCount:    s.StolenCount,
		FertMask:       s.FertMask,
	}
}

// PlotChangeOf 将地块投影为 FarmDelta 用的 PlotChange（字段与 PlotSnapshot 对齐）。
func PlotChangeOf(index uint8, p Plot) PlotChange {
	return plotChangeFromSnapshot(PlotSnapshotOf(index, p))
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
		PlantNonce:     newPlantNonce(),
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
		cfg := a.plotAdvanceConfig(work.CropID)
		cfg.PlotIndex = idx
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
		cfg := a.plotAdvanceConfig(work.CropID)
		cfg.PlotIndex = idx
		settleTo(work, now, cfg)
		clearHazard(work, &work.WeedSince, &work.WeedNextWin, now, cfg)
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
		cfg := a.plotAdvanceConfig(work.CropID)
		cfg.PlotIndex = idx
		settleTo(work, now, cfg)
		clearHazard(work, &work.PestSince, &work.PestNextWin, now, cfg)
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
	codex := a.RecordCodexHarvest(cropID)

	if work.SeasonIndex+1 < work.SeasonTotal {
		enterNextSeason(work, crop, now)
		a.Plots[idx] = *work
	} else {
		a.Plots[idx] = Plot{
			State:        StateResidue,
			CropID:       cropID,
			HarvestRound: work.HarvestRound,
		}
	}
	a.FarmSeq++
	result := a.okPatch(idx)
	result.Patch.Codex = &codex
	return result
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
	return gameconf.SeasonDurationMs(crop, seasonIndex, defaultTimeProfile)
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
		Plot:      clonePlot(a.Plots[idx]),
		Coin:      a.Coin,
		Exp:       a.Exp,
		Items:     items,
	}
}

// clonePlot 深拷贝可变切片，避免 ActionPatch 逃出 Actor 串行区后与地块共享底层数组。
func clonePlot(p Plot) Plot {
	if len(p.Stealers) > 0 {
		p.Stealers = append([]uint64(nil), p.Stealers...)
	}
	return p
}

func newPlantNonce() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// CSPRNG 不可用时退化为非零时间混合，仍保证同农场轮次不完全撞车。
		return uint32(time.Now().UnixNano() | 1)
	}
	n := binary.LittleEndian.Uint32(b[:])
	if n == 0 {
		return 1
	}
	return n
}
