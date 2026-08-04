package farm

import "testing"

// 第一层单函数基准（docs/design/capacity-and-benchmark.md §4.2）。
// 命名匹配文档命令：BenchmarkAdvance|BenchmarkSettle|BenchmarkHazard|BenchmarkYield

func BenchmarkAdvance(b *testing.B) {
	p, cfg, now := benchAdvanceFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetAdvanceFixture(&p)
		Advance(&p, now, cfg)
		sinkInt64 = p.AccruedWeighted
	}
}

func BenchmarkSettle(b *testing.B) {
	p, cfg, now := benchAdvanceFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetAdvanceFixture(&p)
		settleTo(&p, now, cfg)
		sinkInt64 = p.AccruedWeighted
	}
}

func BenchmarkHazard(b *testing.B) {
	p, cfg, now := benchScanHazardWorstFixture()
	// 计时外锁定最坏路径：正阈值（真实 hash）+ 10 窗全 miss。
	if cfg.RiskWindows != 10 || cfg.WeedThreshold <= 0 {
		b.Fatalf("worst-path fixture degraded: RiskWindows=%d WeedThreshold=%d", cfg.RiskWindows, cfg.WeedThreshold)
	}
	resetScanHazardWorstFixture(&p)
	ms := scanHazard(&p, p.LastSettleAt, now, &p.WeedSince, &p.WeedNextWin, HazardWeed, cfg)
	if ms != 0 || p.WeedSince != 0 || p.WeedNextWin != 10 {
		b.Fatalf("worst-path probe failed: ms=%d WeedSince=%d WeedNextWin=%d", ms, p.WeedSince, p.WeedNextWin)
	}
	resetScanHazardWorstFixture(&p)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resetScanHazardWorstFixture(&p)
		sinkInt64 = scanHazard(&p, p.LastSettleAt, now, &p.WeedSince, &p.WeedNextWin, HazardWeed, cfg)
	}
}

func BenchmarkHazardHit(b *testing.B) {
	p, cfg, _ := benchAdvanceFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = hazardHit(cfg, &p, HazardWeed, uint8(i%10))
	}
}

func BenchmarkYield(b *testing.B) {
	p, cfg, _ := benchAdvanceFixture()
	p.AccruedWeighted = 44 * 150
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkUint16 = computeFinalYield(&p, cfg)
	}
}
