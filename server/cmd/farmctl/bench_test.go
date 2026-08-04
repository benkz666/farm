package main

import (
	"strings"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway"
	"farm/server/shared/errcode"
)

func durations(ms ...int) []time.Duration {
	out := make([]time.Duration, 0, len(ms))
	for _, v := range ms {
		out = append(out, time.Duration(v)*time.Millisecond)
	}
	return out
}

func TestPercentileDurationNearestRank(t *testing.T) {
	// 1..100ms，第 p 分位恰好是第 p 个样本，便于逐个核对最近秩法的取值。
	samples := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{50, 50 * time.Millisecond},
		{95, 95 * time.Millisecond},
		{99, 99 * time.Millisecond},
		{100, 100 * time.Millisecond},
	}
	for _, tt := range cases {
		if got := percentileDuration(samples, tt.p); got != tt.want {
			t.Fatalf("percentileDuration(p=%v) = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestPercentileDurationEdgeCases(t *testing.T) {
	if got := percentileDuration(nil, 50); got != 0 {
		t.Fatalf("空样本应返回 0，得到 %v", got)
	}
	single := durations(7)
	for _, p := range []float64{0, 50, 95, 99, 100} {
		if got := percentileDuration(single, p); got != 7*time.Millisecond {
			t.Fatalf("单样本 p=%v 应返回该样本，得到 %v", p, got)
		}
	}
	// 分位数必须落在真实样本上，绝不插值出一个没发生过的值。
	three := durations(10, 20, 90)
	if got := percentileDuration(three, 50); got != 20*time.Millisecond {
		t.Fatalf("p50 = %v, want 20ms", got)
	}
	if got := percentileDuration(three, 99); got != 90*time.Millisecond {
		t.Fatalf("p99 = %v, want 90ms", got)
	}
}

func TestBenchLatencyLineFormat(t *testing.T) {
	// 4 个样本时最近秩法的 p50 是第 ceil(0.5×4)=2 个，即 45ms。
	line := benchLatencyLine(durations(12, 45, 78, 102))
	want := "p50=  45.0ms   p95= 102.0ms   p99= 102.0ms   max= 102.0ms"
	if line != want {
		t.Fatalf("延迟行 = %q, want %q", line, want)
	}
	if got := benchLatencyLine(nil); !strings.Contains(got, "n/a") {
		t.Fatalf("无样本时应输出 n/a，得到 %q", got)
	}
}

func TestBenchStatsOnlyCountsSuccessLatency(t *testing.T) {
	stats := newBenchStats(2)
	stats.addAll([]benchOutcome{
		{latency: 10 * time.Millisecond, code: errcode.OK},
		{latency: 5000 * time.Millisecond, code: errcode.Timeout},
	})
	stats.addAll([]benchOutcome{
		{latency: 20 * time.Millisecond, code: errcode.OK},
		{latency: 30 * time.Millisecond, code: errcode.OK},
	})
	if len(stats.success) != 3 {
		t.Fatalf("成功样本数 = %d, want 3", len(stats.success))
	}
	for _, sample := range stats.success {
		if sample > time.Second {
			t.Fatalf("超时样本 %v 不应进入成功延迟统计", sample)
		}
	}
	lines := stats.render()
	if lines[0] != "访客数=2  样本=4  成功=3  失败=1" {
		t.Fatalf("汇总首行 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "max=  30.0ms") {
		t.Fatalf("延迟行应只覆盖成功样本，得到 %q", lines[1])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "1004(主人侧裁决超时)=1") {
		t.Fatalf("缺少错误码分布：\n%s", joined)
	}
}

func TestBenchStatsReportsConcurrentWindowThroughput(t *testing.T) {
	stats := newBenchStats(2)
	stats.addAll([]benchOutcome{
		{latency: 10 * time.Millisecond, code: errcode.OK},
		{latency: 20 * time.Millisecond, code: errcode.OK},
	})
	stats.addAll([]benchOutcome{
		{latency: 15 * time.Millisecond, code: errcode.OK},
		{latency: 20 * time.Millisecond, code: errcode.OK},
	})
	rendered := strings.Join(stats.render(), "\n")
	if !strings.Contains(rendered, "并发窗口吞吐=100.0 req/s") {
		t.Fatalf("并发窗口吞吐计算错误：\n%s", rendered)
	}
}

func TestBenchStatsWarnsAboveFailureThreshold(t *testing.T) {
	// 20 个样本里 1 个失败 = 5%，不触发；2 个失败 = 10%，必须显著警告。
	quiet := newBenchStats(1)
	for i := 0; i < 19; i++ {
		quiet.addAll([]benchOutcome{{latency: time.Millisecond, code: errcode.OK}})
	}
	quiet.addAll([]benchOutcome{{latency: time.Millisecond, code: errcode.AlreadyWatered}})
	if strings.Contains(strings.Join(quiet.render(), "\n"), "警告") {
		t.Fatalf("恰好 5%% 失败率不应告警：\n%s", strings.Join(quiet.render(), "\n"))
	}

	loud := newBenchStats(1)
	for i := 0; i < 18; i++ {
		loud.addAll([]benchOutcome{{latency: time.Millisecond, code: errcode.OK}})
	}
	loud.addAll([]benchOutcome{{latency: time.Millisecond, code: errcode.AlreadyWatered}})
	loud.addAll([]benchOutcome{{latency: time.Millisecond, code: errcode.AlreadyWatered}})
	rendered := strings.Join(loud.render(), "\n")
	if !strings.Contains(rendered, "警告") || !strings.Contains(rendered, "10.0%") {
		t.Fatalf("超过 5%% 失败率必须显著警告：\n%s", rendered)
	}
}

func TestBenchFailureBreakdownIsDeterministic(t *testing.T) {
	failures := map[string]int{
		"1211(水分充足)":    3,
		"1004(主人侧裁决超时)": 3,
		"1003(限流)":      9,
	}
	// 次数降序、同次数按标签升序，保证同一份数据每次输出一致。
	want := "1003(限流)=9  1004(主人侧裁决超时)=3  1211(水分充足)=3"
	for i := 0; i < 8; i++ {
		if got := benchFailureBreakdown(failures); got != want {
			t.Fatalf("失败分布 = %q, want %q", got, want)
		}
	}
}

func TestBenchOutcomeSuccessDefinition(t *testing.T) {
	if !(benchOutcome{code: errcode.OK}).ok() {
		t.Fatal("错误码 0 且无本地失败应算成功")
	}
	if (benchOutcome{code: errcode.AlreadyWatered}).ok() {
		t.Fatal("业务错误不算成功")
	}
	if (benchOutcome{code: errcode.OK, transport: "读超时"}).ok() {
		t.Fatal("本地失败不算成功")
	}
	if label := benchFailureLabel(benchOutcome{transport: "读超时"}); !strings.Contains(label, "读超时") {
		t.Fatalf("本地失败标签 = %q", label)
	}
}

func TestBenchPlotAssignment(t *testing.T) {
	// 访客不超过地块数时人各一块，保证每个请求都真正走完落盘。
	for i := 0; i < 6; i++ {
		if got := benchPlotForVisitor(i, 6); got != uint32(i) {
			t.Fatalf("访客[%d] 分到地块 %d, want %d", i, got, i)
		}
	}
	// 超出部分只能与人共用。
	if got := benchPlotForVisitor(7, 6); got != 1 {
		t.Fatalf("访客[7] 分到地块 %d, want 1", got)
	}

	if got := benchTargetPlots(3, 6); len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("benchTargetPlots(3,6) = %v", got)
	}
	if got := benchTargetPlots(10, 6); len(got) != 6 {
		t.Fatalf("目标地块数不应超过已解锁地块数，得到 %v", got)
	}
}

func TestBenchWaterSpanMatchesServerFormula(t *testing.T) {
	// 服务端 farm.waterFull 用的是整数算法 season × 35 / 100，必须逐位一致。
	cases := map[int64]int64{
		60_000: 21_000,
		78_000: 27_300,
		1:      0,
		0:      0,
		-5:     0,
	}
	for season, want := range cases {
		if got := benchWaterSpanMs(season); got != want {
			t.Fatalf("benchWaterSpanMs(%d) = %d, want %d", season, got, want)
		}
	}
}

func growingPlot(index uint8, seasonStart, seasonDuration, lastWater int64) farm.PlotSnapshot {
	return farm.PlotSnapshot{
		Index:          index,
		State:          farm.StateGrowing,
		CropID:         benchCropID,
		SeasonStartAt:  seasonStart,
		SeasonDuration: seasonDuration,
		MatureAt:       seasonStart + seasonDuration,
		LastWaterAt:    lastWater,
	}
}

func TestBenchPlanRoundAdvancesToDryWithoutReplant(t *testing.T) {
	const season = 60_000
	const serverTime = 1_000_000
	plots := []farm.PlotSnapshot{
		growingPlot(0, serverTime, season, serverTime),
		growingPlot(1, serverTime, season, serverTime),
	}
	plan, err := benchPlanRound(plots, []uint32{0, 1}, serverTime, 2_000)
	if err != nil {
		t.Fatalf("benchPlanRound: %v", err)
	}
	if plan.Replant {
		t.Fatal("刚种下的地块不需要补种")
	}
	// 水分持续 21s，再加 2s 余量。
	if plan.Advance != 23_000 {
		t.Fatalf("Advance = %d, want 23000", plan.Advance)
	}
}

func TestBenchPlanRoundTakesSlowestPlot(t *testing.T) {
	const season = 60_000
	const serverTime = 1_000_000
	plots := []farm.PlotSnapshot{
		growingPlot(0, serverTime-10_000, season, serverTime-10_000),
		growingPlot(1, serverTime-10_000, season, serverTime), // 刚被浇过，最晚变干
	}
	plan, err := benchPlanRound(plots, []uint32{0, 1}, serverTime, 2_000)
	if err != nil {
		t.Fatalf("benchPlanRound: %v", err)
	}
	if plan.Replant {
		t.Fatal("两块地都还在生长，不需要补种")
	}
	if plan.Advance != 23_000 {
		t.Fatalf("Advance = %d, want 23000（取最晚变干的那块）", plan.Advance)
	}
}

func TestBenchPlanRoundReplantsWhenAdvanceWouldMature(t *testing.T) {
	const season = 60_000
	const serverTime = 1_000_000
	// 已经过了两个水分窗口：再推进一个窗口就会越过成熟点，成熟后浇不动了。
	var seasonStart int64 = serverTime - 42_000
	plots := []farm.PlotSnapshot{growingPlot(0, seasonStart, season, serverTime)}
	plan, err := benchPlanRound(plots, []uint32{0}, serverTime, 2_000)
	if err != nil {
		t.Fatalf("benchPlanRound: %v", err)
	}
	if !plan.Replant {
		t.Fatal("推进后会成熟的地块必须先补种")
	}
	if plan.Advance != 0 {
		t.Fatalf("需要补种时不应先推进到缺水，Advance = %d", plan.Advance)
	}
	// 先推到成熟才能收获换钱：18000ms 后成熟，多推 1ms 越过判定边界。
	if plan.AdvanceToMature != 18_001 {
		t.Fatalf("AdvanceToMature = %d, want 18001", plan.AdvanceToMature)
	}
}

func TestBenchPlanRoundReplantsNonGrowingPlot(t *testing.T) {
	plots := []farm.PlotSnapshot{
		{Index: 0, State: farm.StateMature, MatureAt: 500},
		{Index: 1, State: farm.StateWasteland},
	}
	plan, err := benchPlanRound(plots, []uint32{0, 1}, 1_000, 2_000)
	if err != nil {
		t.Fatalf("benchPlanRound: %v", err)
	}
	if !plan.Replant {
		t.Fatal("非生长中的地块必须补种")
	}
	if plan.AdvanceToMature != 0 {
		t.Fatalf("没有仍在生长的作物时无需推进到成熟，得到 %d", plan.AdvanceToMature)
	}
}

func TestBenchPlanRoundRejectsMissingPlot(t *testing.T) {
	if _, err := benchPlanRound(nil, []uint32{0}, 1_000, 2_000); err == nil {
		t.Fatal("快照缺少目标地块时必须报错，而不是静默按零值规划")
	}
}

func TestBenchWaterMarksDetectStragglers(t *testing.T) {
	before := []farm.PlotSnapshot{
		growingPlot(0, 0, 60_000, 100),
		growingPlot(1, 0, 60_000, 200),
	}
	after := []farm.PlotSnapshot{
		growingPlot(0, 0, 60_000, 100),
		growingPlot(1, 0, 60_000, 900), // 上一轮的迟到裁决把这块地浇了
	}
	targets := []uint32{0, 1}

	marksBefore, err := benchWaterMarks(before, targets)
	if err != nil {
		t.Fatalf("benchWaterMarks: %v", err)
	}
	marksAfter, err := benchWaterMarks(after, targets)
	if err != nil {
		t.Fatalf("benchWaterMarks: %v", err)
	}
	if benchMarksEqual(marksBefore, marksAfter) {
		t.Fatal("last_water_at 变了就说明上一轮还没收敛，不能判为相等")
	}
	if !benchMarksEqual(marksBefore, marksBefore) {
		t.Fatal("同一组水印必须判为相等")
	}
	if benchMarksEqual(marksBefore, nil) {
		t.Fatal("长度不同必须判为不等")
	}
	if _, err := benchWaterMarks(before, []uint32{5}); err == nil {
		t.Fatal("快照缺少目标地块时必须报错")
	}
}

func TestBenchHasFailure(t *testing.T) {
	clean := []benchOutcome{{code: errcode.OK}, {code: errcode.OK}}
	if benchHasFailure(clean) {
		t.Fatal("全部成功不应判为有失败")
	}
	dirty := []benchOutcome{{code: errcode.OK}, {code: errcode.Timeout}}
	if !benchHasFailure(dirty) {
		t.Fatal("存在失败时必须触发轮间收敛等待")
	}
}

func TestBenchTillActionPerState(t *testing.T) {
	cases := []struct {
		state  uint8
		cmd    uint32
		arg    uint32
		needed bool
	}{
		{farm.StateWasteland, gateway.CommandTill, 0, true},
		{farm.StateTilled, 0, 0, false},
		{farm.StateGrowing, gateway.CommandClear, uint32(farm.ClearArgUproot), true},
		{farm.StateMature, gateway.CommandClear, 0, true},
		{farm.StateResidue, gateway.CommandClear, 0, true},
		{farm.StateWithered, gateway.CommandClear, 0, true},
	}
	for _, tt := range cases {
		cmd, arg, needed := benchTillAction(tt.state)
		if cmd != tt.cmd || arg != tt.arg || needed != tt.needed {
			t.Fatalf("benchTillAction(%d) = (%d, %d, %v), want (%d, %d, %v)",
				tt.state, cmd, arg, needed, tt.cmd, tt.arg, tt.needed)
		}
	}
}

func TestBenchRoundLineMarksWarmup(t *testing.T) {
	outcomes := []benchOutcome{
		{latency: 10 * time.Millisecond, code: errcode.OK},
		{latency: 30 * time.Millisecond, code: errcode.AlreadyWatered},
	}
	warm := benchRoundLine(2, 33, true, outcomes)
	if !strings.Contains(warm, "预热") || !strings.Contains(warm, "成功=1/2") {
		t.Fatalf("预热轮输出 = %q", warm)
	}
	counted := benchRoundLine(9, 33, false, outcomes)
	if !strings.Contains(counted, "计入") {
		t.Fatalf("计入轮输出 = %q", counted)
	}
}
