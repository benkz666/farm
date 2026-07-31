package farm

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"math/bits"
	"time"

	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

// 默认时间档：保持旧调用与单测为 demo；线上动作由装配层显式传入权威档位。
const defaultTimeProfile = gameconf.TimeProfileDemo

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
	Steal
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
	case Steal:
		return "Steal"
	default:
		return "PlotActionKind(?)"
	}
}

// PlotAction 是一次地块意图。Plant 时 Arg 为 crop_id，Fertilize 时为 fertilizer_id。
type PlotAction struct {
	Kind      PlotActionKind
	PlotIndex uint8
	Arg       uint16
	// TimeProfile 由服务端装配层注入，禁止从浏览器请求直接透传。
	// 空值兼容旧调用并回落 demo。
	TimeProfile string
}

// ActionPatch 描述一次成功动作后，客户端可应用的变更摘要。
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

// ApplyPlotAction 在副本上 advance 后校验；只有成功动作才会写回聚合。
func (a *Aggregate) ApplyPlotAction(act PlotAction, now int64) ActionResult {
	if a == nil {
		return ActionResult{Err: pkgerr.Internal}
	}
	if int(act.PlotIndex) >= int(a.UnlockedPlots) || int(act.PlotIndex) >= len(a.Plots) {
		return ActionResult{Err: pkgerr.PlotNotFound}
	}
	work := a.Plots[act.PlotIndex]
	timeProfile := actionTimeProfile(act.TimeProfile)
	a.advancePlot(&work, now, act.PlotIndex)
	if work.State == StateGrowing && gameconf.ValidTimeProfile(act.TimeProfile) {
		reprofileGrowingPlot(&work, now, timeProfile)
	}
	if !AllowsPlotAction(work.State, act.Kind) {
		return ActionResult{Err: plotActionStateError(work.State, act.Kind)}
	}

	switch act.Kind {
	case Till:
		return a.commitTill(act.PlotIndex, &work)
	case Clear:
		return a.commitClear(act.PlotIndex, &work)
	case Plant:
		return a.commitPlant(act.PlotIndex, &work, act.Arg, now, timeProfile)
	case Water:
		return a.commitWater(act.PlotIndex, &work, now)
	case Weed:
		return a.commitWeed(act.PlotIndex, &work, now)
	case Pest:
		return a.commitPest(act.PlotIndex, &work, now)
	case Fertilize:
		return a.commitFertilize(act.PlotIndex, &work, act.Arg, now)
	case Harvest:
		return a.commitHarvest(act.PlotIndex, &work, now, timeProfile)
	default:
		return ActionResult{Err: pkgerr.BadRequest}
	}
}

func actionTimeProfile(profile string) string {
	if gameconf.ValidTimeProfile(profile) {
		return profile
	}
	return defaultTimeProfile
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
	return a.advanceAll(now, "")
}

// AdvanceAllWithProfile 在惰性推进后，把仍在生长的当前季按已完成进度切换到
// profile。它用于调试时间档热切换：成熟作物不回退，正在生长的作物不重置进度。
func (a *Aggregate) AdvanceAllWithProfile(now int64, profile string) []PlotChange {
	if !gameconf.ValidTimeProfile(profile) {
		return a.AdvanceAll(now)
	}
	return a.advanceAll(now, profile)
}

func (a *Aggregate) advanceAll(now int64, profile string) []PlotChange {
	if a == nil {
		return nil
	}
	// EnterFarm / SyncFarm 都经这里，是玩家重新上线后回滚超时预占的主要时机。
	a.ExpireCrossPending(now)

	changes := make([]PlotChange, 0)
	for i := range a.Plots {
		p := &a.Plots[i]
		if p.State == StateGrowing {
			before := PlotSnapshotOf(uint8(i), *p)
			a.advancePlot(p, now, uint8(i))
			if p.State == StateGrowing && profile != "" {
				reprofileGrowingPlot(p, now, profile)
			}
			after := PlotSnapshotOf(uint8(i), *p)
			// LastSettleAt 是客户端插值健康度的基准，但它自身推进不属于可见变化；
			// 只有状态、风险、健康度或产量等字段改变时才占用一个 FarmSeq。
			before.LastSettleAt = after.LastSettleAt
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

// reprofileGrowingPlot 保留当前季的完成比例、施肥推进和健康度，把后续时间轴
// 映射到新的档位。所有输入均为非负毫秒；最大作物季时长远小于 int64 上限，
// 比例乘法仍使用 128 位中间值，避免以后扩大配置时出现溢出。
func reprofileGrowingPlot(p *Plot, now int64, profile string) bool {
	if p == nil || p.State != StateGrowing || p.SeasonDuration <= 0 || p.MatureAt <= now {
		return false
	}
	crop, ok := gameconf.CropByID(p.CropID)
	if !ok {
		return false
	}
	newDuration := gameconf.SeasonDurationMs(crop, p.SeasonIndex, profile)
	oldDuration := p.SeasonDuration
	if newDuration <= 0 || newDuration == oldDuration {
		return false
	}

	oldRemaining := p.MatureAt - now
	if oldRemaining > oldDuration {
		oldRemaining = oldDuration
	}
	newRemaining := scaleDuration(oldRemaining, newDuration, oldDuration)
	oldElapsed := now - p.SeasonStartAt
	if oldElapsed < 0 {
		oldElapsed = 0
	}
	if oldElapsed > oldDuration {
		oldElapsed = oldDuration
	}
	newSeasonStart := now - scaleDuration(oldElapsed, newDuration, oldDuration)

	p.AccruedWeighted = scaleDuration(p.AccruedWeighted, newDuration, oldDuration)
	p.LastWaterAt = scalePastTimestamp(p.LastWaterAt, now, newDuration, oldDuration, newSeasonStart)
	p.WeedSince = scalePastTimestamp(p.WeedSince, now, newDuration, oldDuration, newSeasonStart)
	p.PestSince = scalePastTimestamp(p.PestSince, now, newDuration, oldDuration, newSeasonStart)
	p.SeasonStartAt = newSeasonStart
	p.SeasonDuration = newDuration
	p.MatureAt = now + newRemaining
	p.LastSettleAt = now
	return true
}

func scaleDuration(value, numerator, denominator int64) int64 {
	if value <= 0 || numerator <= 0 || denominator <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(value), uint64(numerator))
	if hi >= uint64(denominator) {
		return int64(^uint64(0) >> 1)
	}
	quotient, _ := bits.Div64(hi, lo, uint64(denominator))
	if quotient > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1)
	}
	return int64(quotient)
}

func scalePastTimestamp(timestamp, now, numerator, denominator, lowerBound int64) int64 {
	if timestamp <= 0 {
		return 0
	}
	if timestamp >= now {
		return now
	}
	scaled := now - scaleDuration(now-timestamp, numerator, denominator)
	if scaled < lowerBound {
		return lowerBound
	}
	return scaled
}

func plotChangeFromSnapshot(s PlotSnapshot) PlotChange {
	return PlotChange{
		Index:          s.Index,
		State:          s.State,
		CropID:         s.CropID,
		SeasonIndex:    s.SeasonIndex,
		SeasonTotal:    s.SeasonTotal,
		SeasonStartAt:  s.SeasonStartAt,
		MatureAt:       s.MatureAt,
		SeasonDuration: s.SeasonDuration,
		FinalYield:     s.FinalYield,
		LastSettleAt:   s.LastSettleAt,
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
	a.Plots[idx] = Plot{State: StateTilled}
	a.Exp += 3
	a.RecalcLevel()
	a.grantHiddenSeed()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitClear(idx uint8, work *Plot) ActionResult {
	a.Plots[idx] = Plot{State: StateTilled}
	a.Exp += 3
	a.RecalcLevel()
	a.grantHiddenSeed()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitPlant(idx uint8, work *Plot, cropID uint16, now int64, timeProfile string) ActionResult {
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

	duration := seasonDuration(crop, 0, timeProfile)
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
}

func (a *Aggregate) commitWeed(idx uint8, work *Plot, now int64) ActionResult {
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
}

func (a *Aggregate) commitPest(idx uint8, work *Plot, now int64) ActionResult {
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
}

func (a *Aggregate) commitFertilize(idx uint8, work *Plot, fertilizerID uint16, now int64) ActionResult {
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
	crop, ok := gameconf.CropByID(work.CropID)
	if !ok {
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
	// 当前季时长已在播种/进入新季度时固化。由它反推该季的 hourMs，确保
	// 服务重启切换全局档位后，旧作物的化肥效果仍沿用原档位。
	seasonMinutes := gameconf.SeasonMinutes(crop, work.SeasonIndex)
	if seasonMinutes == 0 || work.SeasonDuration%int64(seasonMinutes) != 0 {
		return ActionResult{Err: pkgerr.Internal}
	}
	hourMs := work.SeasonDuration / int64(seasonMinutes) * 60
	reduce := int64(fertilizer.ReduceHours * float64(hourMs))
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
	// 最后一个生长阶段可能被化肥恰好压缩到当前时刻。此时应在同一次
	// 施肥响应里完成成熟结算，避免客户端停在 Growing + 00:00，直到
	// 下一次 SyncFarm 才看到成熟。
	a.advancePlot(work, now, idx)
	a.Plots[idx] = *work
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitHarvest(idx uint8, work *Plot, now int64, timeProfile string) ActionResult {
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
		enterNextSeason(work, crop, now, timeProfile)
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

func enterNextSeason(p *Plot, crop gameconf.CropConf, now int64, timeProfile string) {
	p.SeasonIndex++
	p.SeasonStartAt = now
	p.SeasonDuration = seasonDuration(crop, p.SeasonIndex, timeProfile)
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

func seasonDuration(crop gameconf.CropConf, seasonIndex uint8, timeProfile string) int64 {
	return gameconf.SeasonDurationMs(crop, seasonIndex, actionTimeProfile(timeProfile))
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

const hiddenSeedDropChanceDenominator uint32 = 100
const hiddenSeedDropThreshold uint32 = 3

// hiddenSeedRandom is replaceable only by same-package tests. Production uses
// crypto/rand so a client cannot predict or reroll a hidden-seed drop.
var hiddenSeedRandom = secureRandomBelow

func secureRandomBelow(limit uint32) (uint32, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return uint32(n.Int64()), nil
}

// grantHiddenSeed performs the documented 3% roll after a successful till or
// clear. Failure to obtain entropy degrades to no drop; the completed farm
// action remains valid and, importantly, never awards an unverified item.
func (a *Aggregate) grantHiddenSeed() {
	roll, err := hiddenSeedRandom(hiddenSeedDropChanceDenominator)
	if err != nil || roll >= hiddenSeedDropThreshold {
		return
	}

	eligible := make([]uint16, 0, 3)
	for id := uint16(1); id <= gameconf.CropCount; id++ {
		crop, ok := gameconf.CropByID(id)
		if ok && crop.Hidden && a.Level >= uint16(crop.DropLevel) {
			eligible = append(eligible, id)
		}
	}
	if len(eligible) == 0 {
		return
	}

	choice, err := hiddenSeedRandom(uint32(len(eligible)))
	if err != nil {
		return
	}
	a.AddItem(SeedItem(eligible[choice]), 1)
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
