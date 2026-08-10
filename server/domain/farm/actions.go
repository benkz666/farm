package farm

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"math/bits"
	"time"

	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
)

// 默认时间档：保持旧调用与单测为 demo；线上动作由装配层显式传入权威档位。
const defaultTimeProfile = gameconfig.TimeProfileDemo

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

// ClearArgUproot is the Clear command's explicit argument for removing a
// growing crop. The zero value keeps the original residue/withered cleanup.
const ClearArgUproot uint16 = 1

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

// PlotAction 是一次地块意图。Plant 时 Arg 为 crop_id，Fertilize 时为
// fertilizer_id，ClearArgUproot 表示铲除生长中的作物。
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
	Plot      *Plot
	Coin      int64
	Exp       uint32
	// Items contains authoritative final counts only for keys changed by this
	// action. A zero count is an explicit deletion. It is not a full inventory
	// snapshot, which keeps hot write responses proportional to the mutation.
	Items map[ItemKey]uint32
	Codex *CodexProgress
}

// ActionResult 是 validate/commit 的统一返回。
type ActionResult struct {
	Err   errcode.Code
	Patch ActionPatch
}

// ApplyPlotAction 在副本上 advance 后校验；只有成功动作才会写回聚合。
func (a *Aggregate) ApplyPlotAction(act PlotAction, now int64) ActionResult {
	if a == nil {
		return ActionResult{Err: errcode.Internal}
	}
	if int(act.PlotIndex) >= int(a.UnlockedPlots) || int(act.PlotIndex) >= len(a.Plots) {
		return ActionResult{Err: errcode.PlotNotFound}
	}
	work := a.Plots[act.PlotIndex]
	timeProfile := actionTimeProfile(act.TimeProfile)
	a.advancePlot(&work, now, act.PlotIndex)
	if work.State == StateGrowing && gameconfig.ValidTimeProfile(act.TimeProfile) {
		reprofileGrowingPlot(&work, now, timeProfile)
	}
	allowed := AllowsPlotAction(work.State, act.Kind)
	if !allowed && act.Kind == Clear && act.Arg == ClearArgUproot && work.State == StateGrowing {
		allowed = true
	}
	if !allowed {
		return ActionResult{Err: plotActionStateError(work.State, act.Kind)}
	}

	switch act.Kind {
	case Till:
		return a.commitTill(act.PlotIndex, &work)
	case Clear:
		if act.Arg == ClearArgUproot && work.State == StateGrowing {
			return a.commitUproot(act.PlotIndex)
		}
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
		return ActionResult{Err: errcode.BadRequest}
	}
}

func actionTimeProfile(profile string) string {
	if gameconfig.ValidTimeProfile(profile) {
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
		if crop, ok := gameconfig.CropByID(cropID); ok {
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
	if !gameconfig.ValidTimeProfile(profile) {
		return a.AdvanceAll(now)
	}
	return a.advanceAll(now, profile)
}

// NextAdvanceAt returns the next authoritative time boundary that can change a
// growing plot. Farm uses it to advance resident actors without periodic
// SyncFarm polling from every browser. A zero result means no timer is needed.
func (a *Aggregate) NextAdvanceAt(now int64) int64 {
	if a == nil {
		return 0
	}
	var next int64
	for i := range a.Plots {
		plot := &a.Plots[i]
		if plot.State != StateGrowing || plot.MatureAt <= 0 || plot.SeasonDuration <= 0 {
			continue
		}
		next = earlierPositive(next, plot.MatureAt)
		windows := int64(gameconfig.RiskWindowsPerSeason)
		windowLen := plot.SeasonDuration / windows
		if windowLen <= 0 {
			continue
		}
		if plot.WeedSince == 0 && plot.WeedNextWin < uint8(windows) {
			boundary := plot.SeasonStartAt + int64(plot.WeedNextWin+1)*windowLen
			next = earlierFutureBoundary(next, boundary, now)
		}
		if plot.PestSince == 0 && plot.PestNextWin < uint8(windows) {
			boundary := plot.SeasonStartAt + int64(plot.PestNextWin+1)*windowLen
			next = earlierFutureBoundary(next, boundary, now)
		}
	}
	return next
}

func earlierPositive(current, candidate int64) int64 {
	if candidate <= 0 || current > 0 && current <= candidate {
		return current
	}
	return candidate
}

func earlierFutureBoundary(current, candidate, now int64) int64 {
	// A stale boundary should run immediately. Advancing by one millisecond is
	// required because hazard windows are half-open and scan wStart < to.
	if candidate <= now {
		candidate = now + 1
	}
	return earlierPositive(current, candidate)
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
	crop, ok := gameconfig.CropByID(p.CropID)
	if !ok {
		return false
	}
	newDuration := gameconfig.SeasonDurationMs(crop, p.SeasonIndex, profile)
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
	return PlotChange(s)
}

// PlotChangeOf 将地块投影为 FarmDelta 用的 PlotChange（字段与 PlotSnapshot 对齐）。
func PlotChangeOf(index uint8, p Plot) PlotChange {
	return plotChangeFromSnapshot(PlotSnapshotOf(index, p))
}

func (a *Aggregate) commitTill(idx uint8, work *Plot) ActionResult {
	a.Plots[idx] = Plot{State: StateTilled}
	a.Exp += 3
	a.RecalcLevel()
	itemKey := a.grantHiddenSeed()
	a.FarmSeq++
	return a.withItemCounts(a.okPatch(idx), itemKey)
}

func (a *Aggregate) commitClear(idx uint8, work *Plot) ActionResult {
	a.Plots[idx] = Plot{State: StateTilled}
	a.Exp += 3
	a.RecalcLevel()
	itemKey := a.grantHiddenSeed()
	a.FarmSeq++
	return a.withItemCounts(a.okPatch(idx), itemKey)
}

// commitUproot removes a growing crop without refunding its seed or awarding
// till/clear experience. The plot remains tilled and can be planted again.
func (a *Aggregate) commitUproot(idx uint8) ActionResult {
	a.Plots[idx] = Plot{State: StateTilled}
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitPlant(idx uint8, work *Plot, cropID uint16, now int64, timeProfile string) ActionResult {
	crop, ok := gameconfig.CropByID(cropID)
	if !ok {
		return ActionResult{Err: errcode.BadRequest}
	}
	if a.Level < uint16(crop.UnlockLevel) {
		return ActionResult{Err: errcode.CropLocked}
	}
	seedKey := SeedItem(cropID)
	if a.Items[seedKey] == 0 {
		return ActionResult{Err: errcode.SeedNotOwned}
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
	return a.withItemCounts(a.okPatch(idx), seedKey)
}

func (a *Aggregate) commitWater(idx uint8, work *Plot, now int64) ActionResult {
	if work.CropID == 0 {
		return ActionResult{Err: errcode.PlotEmpty}
	}
	cfg := a.plotAdvanceConfig(work.CropID)
	cfg.PlotIndex = idx
	if waterFull(work, now, cfg) {
		return ActionResult{Err: errcode.AlreadyWatered}
	}
	if work.State == StateGrowing {
		settleTo(work, now, cfg)
	}
	work.LastWaterAt = now
	a.Plots[idx] = *work
	a.Exp += 2
	a.RecalcLevel()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitWeed(idx uint8, work *Plot, now int64) ActionResult {
	if work.CropID == 0 {
		return ActionResult{Err: errcode.PlotEmpty}
	}
	if work.WeedSince == 0 {
		return ActionResult{Err: errcode.NoWeed}
	}
	cfg := a.plotAdvanceConfig(work.CropID)
	cfg.PlotIndex = idx
	if work.State == StateGrowing {
		settleTo(work, now, cfg)
	}
	clearHazard(work, &work.WeedSince, &work.WeedNextWin, now, cfg)
	a.Plots[idx] = *work
	a.Exp += 2
	a.RecalcLevel()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitPest(idx uint8, work *Plot, now int64) ActionResult {
	if work.CropID == 0 {
		return ActionResult{Err: errcode.PlotEmpty}
	}
	if work.PestSince == 0 {
		return ActionResult{Err: errcode.NoPest}
	}
	cfg := a.plotAdvanceConfig(work.CropID)
	cfg.PlotIndex = idx
	if work.State == StateGrowing {
		settleTo(work, now, cfg)
	}
	clearHazard(work, &work.PestSince, &work.PestNextWin, now, cfg)
	a.Plots[idx] = *work
	a.Exp += 2
	a.RecalcLevel()
	a.FarmSeq++
	return a.okPatch(idx)
}

func (a *Aggregate) commitFertilize(idx uint8, work *Plot, fertilizerID uint16, now int64) ActionResult {
	if work.CropID == 0 {
		return ActionResult{Err: errcode.PlotEmpty}
	}
	fertilizer, ok := gameconfig.FertilizerByID(fertilizerID)
	if !ok {
		return ActionResult{Err: errcode.BadRequest}
	}
	key := FertilizerItem(fertilizerID)
	if a.Items[key] == 0 {
		return ActionResult{Err: errcode.FertilizerNotOwned}
	}
	if work.SeasonDuration <= 0 || work.StageCount == 0 {
		return ActionResult{Err: errcode.Internal}
	}
	crop, ok := gameconfig.CropByID(work.CropID)
	if !ok {
		return ActionResult{Err: errcode.Internal}
	}

	progress := work.SeasonDuration - (work.MatureAt - now)
	if progress < 0 {
		progress = 0
	}
	stage := progress * int64(work.StageCount) / work.SeasonDuration
	if stage >= int64(work.StageCount) {
		return ActionResult{Err: errcode.PlotNotGrowing}
	}
	stageBit := uint8(1 << stage)
	if work.FertMask&stageBit != 0 {
		return ActionResult{Err: errcode.StageAlreadyFertilized}
	}
	stageEnd := (stage + 1) * work.SeasonDuration / int64(work.StageCount)
	// 当前季时长已在播种/进入新季度时固化。由它反推该季的 hourMs，确保
	// 服务重启切换全局档位后，旧作物的化肥效果仍沿用原档位。
	seasonMinutes := gameconfig.SeasonMinutes(crop, work.SeasonIndex)
	if seasonMinutes == 0 || work.SeasonDuration%int64(seasonMinutes) != 0 {
		return ActionResult{Err: errcode.Internal}
	}
	hourMs := work.SeasonDuration / int64(seasonMinutes) * 60
	reduce := int64(fertilizer.ReduceHours * float64(hourMs))
	if remaining := stageEnd - progress; reduce > remaining {
		reduce = remaining
	}
	if reduce <= 0 {
		return ActionResult{Err: errcode.PlotNotGrowing}
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
	return a.withItemCounts(a.okPatch(idx), key)
}

func (a *Aggregate) commitHarvest(idx uint8, work *Plot, now int64, timeProfile string) ActionResult {
	cropID := work.CropID
	crop, ok := gameconfig.CropByID(cropID)
	if !ok {
		return ActionResult{Err: errcode.Internal}
	}

	yield := work.FinalYield
	if work.StolenCount > yield {
		yield = 0
	} else {
		yield -= work.StolenCount
	}
	var itemKey ItemKey
	if yield > 0 {
		itemKey = FruitItem(cropID)
		a.Items[itemKey] += uint32(yield)
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
	result := a.withItemCounts(a.okPatch(idx), itemKey)
	result.Patch.Codex = &codex
	return result
}

func enterNextSeason(p *Plot, crop gameconfig.CropConf, now int64, timeProfile string) {
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

func seasonDuration(crop gameconfig.CropConf, seasonIndex uint8, timeProfile string) int64 {
	return gameconfig.SeasonDurationMs(crop, seasonIndex, actionTimeProfile(timeProfile))
}

func stageCount(crop gameconfig.CropConf) uint8 {
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
	reference := now
	if p.State == StateMature {
		// 成熟后只保留成熟瞬间已有的干旱。补水一次即永久清除本季干旱，
		// 不再因墙钟继续前进而重新变干。
		reference = p.MatureAt
	}
	return reference < p.LastWaterAt+span
}

func mustCrop(id uint16) gameconfig.CropConf {
	c, ok := gameconfig.CropByID(id)
	if !ok {
		return gameconfig.CropConf{}
	}
	return c
}

func (a *Aggregate) okPatch(idx uint8) ActionResult {
	return ActionResult{Err: errcode.OK, Patch: a.patchOf(idx)}
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
func (a *Aggregate) grantHiddenSeed() ItemKey {
	roll, err := hiddenSeedRandom(hiddenSeedDropChanceDenominator)
	if err != nil || roll >= hiddenSeedDropThreshold {
		return ""
	}

	eligible := make([]uint16, 0, 3)
	for id := uint16(1); id <= gameconfig.CropCount; id++ {
		crop, ok := gameconfig.CropByID(id)
		if ok && crop.Hidden && a.Level >= uint16(crop.DropLevel) {
			eligible = append(eligible, id)
		}
	}
	if len(eligible) == 0 {
		return ""
	}

	choice, err := hiddenSeedRandom(uint32(len(eligible)))
	if err != nil {
		return ""
	}
	key := SeedItem(eligible[choice])
	a.AddItem(key, 1)
	return key
}

func (a *Aggregate) patchOf(idx uint8) ActionPatch {
	plot := clonePlot(a.Plots[idx])
	return ActionPatch{
		PlotIndex: idx,
		Plot:      &plot,
		Coin:      a.Coin,
		Exp:       a.Exp,
	}
}

func (a *Aggregate) resourcePatch() ActionPatch {
	return ActionPatch{Coin: a.Coin, Exp: a.Exp}
}

func (a *Aggregate) withItemCounts(result ActionResult, keys ...ItemKey) ActionResult {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if result.Patch.Items == nil {
			result.Patch.Items = make(map[ItemKey]uint32, len(keys))
		}
		result.Patch.Items[key] = a.Items[key]
	}
	return result
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
