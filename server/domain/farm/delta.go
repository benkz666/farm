package farm

// DeltaRingCapacity 对应 protocol.md 2.3 节的容量选择：每个农场 Actor 保留最近 200 条 delta。
const DeltaRingCapacity = 200

// PlotChange 是 FarmDelta 中单个地块的可见状态。
// 字段与 PlotSnapshot 保持一致，方便客户端复用地块投影逻辑。
type PlotChange struct {
	Index          uint8  `json:"index"`
	State          uint8  `json:"state"`
	CropID         uint16 `json:"crop_id"`
	SeasonIndex    uint8  `json:"season_index"`
	SeasonTotal    uint8  `json:"season_total"`
	SeasonStartAt  int64  `json:"season_start_at"`
	MatureAt       int64  `json:"mature_at"`
	SeasonDuration int64  `json:"season_duration"`
	FinalYield     uint16 `json:"final_yield"`
	LastSettleAt   int64  `json:"last_settle_at"`
	LastWaterAt    int64  `json:"last_water_at"`
	WeedSince      int64  `json:"weed_since"`
	PestSince      int64  `json:"pest_since"`
	Health         uint8  `json:"health"`
	StolenCount    uint16 `json:"stolen_count"`
	FertMask       uint8  `json:"fert_mask"`
}

// FarmDelta 是一次农场可见状态变化的增量同步单元。
type FarmDelta struct {
	OwnerUID uint64            `json:"owner_uid"`
	FarmSeq  uint64            `json:"farm_seq"`
	Plots    []PlotChange      `json:"plots"`
	GuardDog *GuardDogSnapshot `json:"guard_dog,omitempty"`
	ActorUID uint64            `json:"actor_uid"`
	Action   uint32            `json:"action"`
}

// DeltaRing 保留最近的农场增量，供断线恢复时补齐连续序列。
// 它由单个 FarmActor 串行访问，不承担并发同步职责。
type DeltaRing struct {
	deltas [DeltaRingCapacity]FarmDelta
	first  int
	length int
}

// Append 追加一条增量；满容量后淘汰最旧的一条。
func (r *DeltaRing) Append(delta FarmDelta) {
	if r == nil {
		return
	}

	index := (r.first + r.length) % DeltaRingCapacity
	if r.length == DeltaRingCapacity {
		index = r.first
		r.first = (r.first + 1) % DeltaRingCapacity
	} else {
		r.length++
	}
	r.deltas[index] = cloneDelta(delta)
}

// Since 返回 farm_seq 不小于 fromSeq 的所有保留增量。
// 若 fromSeq 早于可连续补齐的最早序列，ok 为 false，调用方应退回全量快照。
func (r *DeltaRing) Since(fromSeq uint64) ([]FarmDelta, bool) {
	if r == nil || r.length == 0 {
		return nil, true
	}

	oldest := r.deltas[r.first].FarmSeq
	if fromSeq < oldest {
		return nil, false
	}

	deltas := make([]FarmDelta, 0, r.length)
	for offset := range r.length {
		delta := r.deltas[(r.first+offset)%DeltaRingCapacity]
		if delta.FarmSeq >= fromSeq {
			deltas = append(deltas, cloneDelta(delta))
		}
	}
	return deltas, true
}

func cloneDelta(delta FarmDelta) FarmDelta {
	delta.Plots = append([]PlotChange(nil), delta.Plots...)
	if delta.GuardDog != nil {
		guardDog := *delta.GuardDog
		delta.GuardDog = &guardDog
	}
	return delta
}
