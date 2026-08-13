package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMarkOutstandingFailed(t *testing.T) {
	recorded := &recorder{}
	recorded.succeeded.Store(7)
	recorded.failed.Store(2)

	markOutstandingFailed(recorded, 12)
	markOutstandingFailed(recorded, 12)

	if got := recorded.failed.Load(); got != 5 {
		t.Fatalf("failed = %d, want 5", got)
	}
}

func TestSummarizeTimedOut(t *testing.T) {
	recorded := &recorder{}
	recorded.add(2*time.Millisecond, true, 0)
	measured := summarize("test", 10, 1, time.Second, recorded, true)

	if !measured.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
	if measured.ActualQPS != 1 {
		t.Fatalf("ActualQPS = %v, want 1", measured.ActualQPS)
	}
	if measured.EndedMS-measured.StartedMS != 1000 {
		t.Fatalf("measurement window = %dms, want 1000ms", measured.EndedMS-measured.StartedMS)
	}
}

func TestSummarizeRecordsP90(t *testing.T) {
	recorded := &recorder{}
	for milliseconds := 1; milliseconds <= 100; milliseconds++ {
		recorded.add(time.Duration(milliseconds)*time.Millisecond, true, 0)
	}

	measured := summarize("test", 100, 100, time.Second, recorded, false)
	if measured.P90MS != 90 {
		t.Fatalf("P90MS = %v, want 90", measured.P90MS)
	}

	step := summarizeStep(recorded)
	if step.P90MS != 90 {
		t.Fatalf("step P90MS = %v, want 90", step.P90MS)
	}
}

func TestGatewayOperationModes(t *testing.T) {
	if !validGatewayWarmupMode(gatewayWarmupFull) || !validGatewayWarmupMode(gatewayWarmupSessionOnly) {
		t.Fatal("documented gateway warmup mode is invalid")
	}
	if validGatewayWarmupMode("none") {
		t.Fatal("unknown gateway warmup mode is valid")
	}
	for _, operation := range []string{"water", "harvest", "steal", "water-visitor"} {
		if !gatewayOperationSupported(operation) {
			t.Fatalf("%s is not supported", operation)
		}
		if !gatewayOperationOneShot(operation) {
			t.Fatalf("%s is not one-shot", operation)
		}
		if !gatewayOperationNeedsOwnActor(operation) {
			t.Fatalf("%s does not prewarm the visitor actor", operation)
		}
		if gatewayOperationWarmRequest(operation) {
			t.Fatalf("%s mutates state during warmup", operation)
		}
	}
	for _, operation := range []string{"ping", "enter", "sync", "friend-list", "search-user", "task-list", "mail-list"} {
		if !gatewayOperationWarmRequest(operation) {
			t.Fatalf("%s does not warm its read path", operation)
		}
	}
	for _, operation := range []string{"task-list", "mail-list"} {
		if !gatewayOperationNeedsFinalReadWarmup(operation) {
			t.Fatalf("%s does not refresh its cache immediately before measurement", operation)
		}
	}
	for _, operation := range []string{"ping", "enter", "sync", "friend-list", "search-user"} {
		if gatewayOperationNeedsFinalReadWarmup(operation) {
			t.Fatalf("%s unexpectedly performs a second read warmup", operation)
		}
	}
	for _, operation := range []string{"buy", "sell"} {
		if !gatewayOperationSupported(operation) || gatewayOperationOneShot(operation) {
			t.Fatalf("%s must be a sustained hot operation", operation)
		}
	}
}

func TestOneShotOperationSchedule(t *testing.T) {
	if got := oneShotOperationCount(3_000, 2*time.Second); got != 6_000 {
		t.Fatalf("oneShotOperationCount = %d, want 6000", got)
	}
	if got := oneShotOperationCount(10, time.Millisecond); got != 1 {
		t.Fatalf("short oneShotOperationCount = %d, want 1", got)
	}
	if got := oneShotOperationCount(int(^uint(0)>>1), time.Duration(1<<63-1)); got != int(^uint(0)>>1) {
		t.Fatalf("overflow-safe oneShotOperationCount = %d, want platform max int", got)
	}
	if got := oneShotStartOffset(3_000, 3_000); got != time.Second {
		t.Fatalf("oneShotStartOffset = %s, want 1s", got)
	}
	if got := oneShotStartOffset(0, 3_000); got != 0 {
		t.Fatalf("first oneShotStartOffset = %s, want 0", got)
	}
}

func TestGatewayMultiPlotFixtureCapacityAndRotation(t *testing.T) {
	indexes := make([]int, 18)
	for index := range indexes {
		indexes[index] = index
	}
	accounts := []gatewayAccount{
		{PlotIndex: 0, PlotIndexes: append([]int(nil), indexes...)},
		{PlotIndex: 0, PlotIndexes: append([]int(nil), indexes...)},
	}
	if err := validateGatewayFixturePlots(accounts, "water"); err != nil {
		t.Fatal(err)
	}
	if got := minimumGatewayActionCount(accounts, "water"); got != 18 {
		t.Fatalf("minimum action count = %d, want 18", got)
	}
	if got := minimumGatewayActionCount([]gatewayAccount{{PlotIndex: 7}}, "harvest"); got != 1 {
		t.Fatalf("legacy action count = %d, want 1", got)
	}

	client := gatewayBenchConnection{account: accounts[0]}
	for _, actionIndex := range []int{0, 9, 17} {
		request := client.requestAt("water", actionIndex)
		var payload struct {
			PlotIndex int `json:"plot_index"`
		}
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.PlotIndex != actionIndex {
			t.Fatalf("action %d used plot %d", actionIndex, payload.PlotIndex)
		}
	}
	if client.nextSeq != 3 {
		t.Fatalf("next sequence = %d, want 3", client.nextSeq)
	}

	bad := []gatewayAccount{{PlotIndexes: []int{0, 0}}}
	if err := validateGatewayFixturePlots(bad, "harvest"); err == nil {
		t.Fatal("duplicate plot indexes should be rejected")
	}
	bad = []gatewayAccount{{PlotIndexes: []int{0, 18}}}
	if err := validateGatewayFixturePlots(bad, "steal"); err == nil {
		t.Fatal("out-of-range plot index should be rejected")
	}
}
