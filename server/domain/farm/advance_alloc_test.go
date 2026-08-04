package farm

import (
	"testing"
)

// sink 防止编译器把热路径结果整段消除；测试与 benchmark 共用。
var (
	sinkInt64  int64
	sinkUint16 uint16
	sinkBool   bool
	sinkBytes  []byte
)

// TestAllocsPerRunDetectsAllocation 证明本文件的 0-alloc 验收能抓住真实分配（可证伪）。
func TestAllocsPerRunDetectsAllocation(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		// 必须写入包级 sink，否则编译器可消除 make，验收本身会失效。
		sinkBytes = make([]byte, 64)
		sinkInt64 = int64(len(sinkBytes))
	})
	if allocs < 1 {
		t.Fatalf("AllocsPerRun did not detect deliberate allocation: got %.2f", allocs)
	}
}

func TestAdvanceZeroAllocs(t *testing.T) {
	p, cfg, now := benchAdvanceFixture()
	allocs := testing.AllocsPerRun(1000, func() {
		resetAdvanceFixture(&p)
		Advance(&p, now, cfg)
		sinkInt64 = p.AccruedWeighted
	})
	if allocs != 0 {
		t.Fatalf("Advance allocs/op = %.2f, want 0", allocs)
	}
}

func TestSettleToZeroAllocs(t *testing.T) {
	p, cfg, now := benchAdvanceFixture()
	allocs := testing.AllocsPerRun(1000, func() {
		resetAdvanceFixture(&p)
		settleTo(&p, now, cfg)
		sinkInt64 = p.AccruedWeighted
	})
	if allocs != 0 {
		t.Fatalf("settleTo allocs/op = %.2f, want 0", allocs)
	}
}

func TestScanHazardZeroAllocs(t *testing.T) {
	p, cfg, now := benchScanHazardWorstFixture()
	assertScanHazardWorstPath(t, &p, cfg, now)

	allocs := testing.AllocsPerRun(1000, func() {
		resetScanHazardWorstFixture(&p)
		sinkInt64 = scanHazard(&p, p.LastSettleAt, now, &p.WeedSince, &p.WeedNextWin, HazardWeed, cfg)
	})
	if allocs != 0 {
		t.Fatalf("scanHazard allocs/op = %.2f, want 0", allocs)
	}
}

// TestScanHazardWorstPathTenMisses 锁定文档「最多 10 次哈希」最坏路径：
// 正阈值走真实 hash，一次调用扫满 10 窗且确定性全部未命中。
func TestScanHazardWorstPathTenMisses(t *testing.T) {
	p, cfg, now := benchScanHazardWorstFixture()
	assertScanHazardWorstPath(t, &p, cfg, now)
}

func TestHazardHitZeroAllocs(t *testing.T) {
	p, cfg, _ := benchAdvanceFixture()
	allocs := testing.AllocsPerRun(1000, func() {
		sinkBool = hazardHit(cfg, &p, HazardWeed, 3)
	})
	if allocs != 0 {
		t.Fatalf("hazardHit allocs/op = %.2f, want 0", allocs)
	}
}

func TestComputeFinalYieldZeroAllocs(t *testing.T) {
	p, cfg, _ := benchAdvanceFixture()
	p.AccruedWeighted = 44 * 150
	allocs := testing.AllocsPerRun(1000, func() {
		sinkUint16 = computeFinalYield(&p, cfg)
	})
	if allocs != 0 {
		t.Fatalf("computeFinalYield allocs/op = %.2f, want 0", allocs)
	}
}

// benchAdvanceFixture 构造代表性生长中地块：一季内结算、开启风险窗口扫描。
func benchAdvanceFixture() (Plot, AdvanceConfig, int64) {
	p := growingPlot()
	p.PlantNonce = 99
	p.SeasonIndex = 1
	cfg := AdvanceConfig{
		BaseYield:             14,
		DryWeight:             44,
		WeedWeight:            33,
		PestWeight:            33,
		WaterSpanNumerator:    35,
		WaterSpanDenominator:  100,
		RiskWindowNumerator:   10,
		RiskWindowDenominator: 100,
		WeedThreshold:         1200,
		PestThreshold:         800,
		RiskWindows:           10,
		OwnerUID:              1001,
		PlotIndex:             2,
		HazardSalt:            DeriveHazardSalt("farm-bench-hazard-secret"),
	}
	now := testSeasonStart + testSeasonDuration/2
	return p, cfg, now
}

func resetAdvanceFixture(p *Plot) {
	p.State = StateGrowing
	p.LastSettleAt = testSeasonStart
	p.LastWaterAt = testSeasonStart
	p.WeedSince = 0
	p.PestSince = 0
	p.WeedNextWin = 0
	p.PestNextWin = 0
	p.AccruedWeighted = 0
	p.FinalYield = 0
	p.StolenCount = 0
	p.CropID = 1
	// 标量复位，不碰 Stealers 底层数组，避免把 slice 操作算进热路径验收。
}

// scanHazard 最坏路径固定输入：正阈值强制走 hazardRoll/xxhash，
// 且在该输入下 10 窗确定性全部未命中（min roll%10000 = 3400 >= 1200）。
const (
	scanHazardWorstOwnerUID    uint64 = 1
	scanHazardWorstPlantNonce  uint32 = 3
	scanHazardWorstPlotIndex   uint8  = 2
	scanHazardWorstSeasonIndex uint8  = 1
	scanHazardWorstWeedThresh  int64  = 1200
)

// benchScanHazardWorstFixture 专用于 scanHazard：to=成熟点 + 正阈值全 miss，
// 一次调用完整走完 10 次真实哈希分支。
func benchScanHazardWorstFixture() (Plot, AdvanceConfig, int64) {
	p, cfg, _ := benchAdvanceFixture()
	p.PlantNonce = scanHazardWorstPlantNonce
	p.SeasonIndex = scanHazardWorstSeasonIndex
	cfg.OwnerUID = scanHazardWorstOwnerUID
	cfg.PlotIndex = scanHazardWorstPlotIndex
	cfg.WeedThreshold = scanHazardWorstWeedThresh
	cfg.PestThreshold = 0 // 本 fixture 只测草路径
	cfg.RiskWindows = 10
	cfg.HazardSalt = DeriveHazardSalt("farm-bench-hazard-secret")
	now := p.MatureAt // 第 10 窗起点 = SeasonStart+0.9*Duration < MatureAt
	return p, cfg, now
}

func resetScanHazardWorstFixture(p *Plot) {
	p.WeedSince = 0
	p.WeedNextWin = 0
	p.LastSettleAt = testSeasonStart
	p.PlantNonce = scanHazardWorstPlantNonce
	p.SeasonIndex = scanHazardWorstSeasonIndex
}

func assertScanHazardWorstPath(t *testing.T, p *Plot, cfg AdvanceConfig, now int64) {
	t.Helper()
	if cfg.RiskWindows != 10 {
		t.Fatalf("RiskWindows = %d, want 10 for worst-path fixture", cfg.RiskWindows)
	}
	if cfg.WeedThreshold <= 0 {
		t.Fatalf("WeedThreshold = %d, want > 0 so hazardHit calls hazardRoll/xxhash", cfg.WeedThreshold)
	}
	resetScanHazardWorstFixture(p)
	ms := scanHazard(p, p.LastSettleAt, now, &p.WeedSince, &p.WeedNextWin, HazardWeed, cfg)
	if ms != 0 {
		t.Fatalf("scanHazard ms = %d, want 0 (no hazard duration on full miss)", ms)
	}
	if p.WeedSince != 0 {
		t.Fatalf("WeedSince = %d, want 0 (no hit)", p.WeedSince)
	}
	if p.WeedNextWin != 10 {
		t.Fatalf("WeedNextWin = %d, want 10 (all risk windows scanned)", p.WeedNextWin)
	}
	for k := uint8(0); k < 10; k++ {
		if hazardHit(cfg, p, HazardWeed, k) {
			t.Fatalf("hazardHit(window=%d) = true, worst path requires all misses with positive threshold", k)
		}
	}
	resetScanHazardWorstFixture(p)
}
