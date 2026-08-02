package store

import (
	"strings"
	"testing"
)

func TestDailyTaskDefinitionsForIncludesFixedAndStableRandomTasks(t *testing.T) {
	const (
		uid    = uint64(42)
		dayKey = int64(20260731)
	)
	first := dailyTaskDefinitionsFor(uid, dayKey)
	second := dailyTaskDefinitionsFor(uid, dayKey)

	if len(first) != 1+RandomDailyTaskCount {
		t.Fatalf("daily task count = %d, want %d", len(first), 1+RandomDailyTaskCount)
	}
	if first[0].id != TaskDailyLoginID || first[0].kind != TaskKindFixed {
		t.Fatalf("first fixed task = %#v, want daily login", first[0])
	}

	seen := make(map[uint32]bool, len(first))
	for index, task := range first {
		if seen[task.id] {
			t.Fatalf("duplicate task id %d", task.id)
		}
		seen[task.id] = true
		if index > 0 && task.kind != TaskKindRandom {
			t.Fatalf("random task kind = %q, want %q", task.kind, TaskKindRandom)
		}
		if task != second[index] {
			t.Fatalf("selection is not stable: first[%d]=%#v second[%d]=%#v", index, task, index, second[index])
		}
	}
}

func TestRandomDailyTaskPoolCanSupplyFiveDistinctTasks(t *testing.T) {
	if len(randomDailyTaskPool) < RandomDailyTaskCount {
		t.Fatalf("random task pool size = %d, want at least %d", len(randomDailyTaskPool), RandomDailyTaskCount)
	}
	seen := make(map[uint32]bool, len(randomDailyTaskPool))
	for _, task := range randomDailyTaskPool {
		if task.kind != TaskKindRandom {
			t.Fatalf("task %d kind = %q, want %q", task.id, task.kind, TaskKindRandom)
		}
		if seen[task.id] {
			t.Fatalf("duplicate random task id %d", task.id)
		}
		seen[task.id] = true
	}
}

func TestIsDailyTaskIDOnlyAcceptsServerOwnedTaskPool(t *testing.T) {
	for _, taskID := range []uint32{
		TaskPlantID, TaskHarvestID, TaskVisitID, TaskDailyLoginID,
		TaskWaterID, TaskFertilizeID, TaskSellID, TaskTillID, TaskWeedID,
		TaskPestID, TaskFeedDogID,
	} {
		if !IsDailyTaskID(taskID) {
			t.Fatalf("IsDailyTaskID(%d) = false", taskID)
		}
	}
	if IsDailyTaskID(99) {
		t.Fatal("IsDailyTaskID(99) = true")
	}
}

func TestDailyTaskInsertQueryBatchesAllDefinitions(t *testing.T) {
	definitions := dailyTaskDefinitionsFor(42, 20260731)
	query, args := dailyTaskInsertQuery(42, 20260731, definitions)

	if got := strings.Count(query, "(?, ?, ?, ?, ?, ?)"); got != len(definitions) {
		t.Fatalf("value tuples = %d, want %d; query=%q", got, len(definitions), query)
	}
	if len(args) != len(definitions)*6 {
		t.Fatalf("args = %d, want %d", len(args), len(definitions)*6)
	}
	if !strings.Contains(query, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("query does not reconcile existing task rows: %q", query)
	}
}

func TestDailyTaskDeleteStaleQueryKeepsOnlySelectedDefinitions(t *testing.T) {
	definitions := dailyTaskDefinitionsFor(42, 20260731)
	query, args := dailyTaskDeleteStaleQuery(42, 20260731, definitions)
	if got := strings.Count(query, "?"); got != 2+len(definitions) {
		t.Fatalf("placeholders = %d, want %d; query=%q", got, 2+len(definitions), query)
	}
	if len(args) != 2+len(definitions) {
		t.Fatalf("args = %d, want %d", len(args), 2+len(definitions))
	}
}

func TestDailyTaskInitCacheCoalescesSamePlayerAndResetsAtNextDay(t *testing.T) {
	var cache dailyTaskInitCache
	first, leader := cache.acquire(42, 20260731)
	if !leader {
		t.Fatal("first acquire is not leader")
	}
	same, leader := cache.acquire(42, 20260731)
	if leader || same != first {
		t.Fatalf("same-day acquire = entry:%p leader:%v, want entry:%p leader:false", same, leader, first)
	}
	definitions := dailyTaskDefinitionsFor(42, 20260731)
	cache.complete(42, 20260731, first, definitions, nil)
	<-same.done
	if len(same.definitions) != len(definitions) {
		t.Fatalf("cached definitions = %d, want %d", len(same.definitions), len(definitions))
	}

	next, leader := cache.acquire(42, 20260801)
	if !leader || next == first {
		t.Fatalf("next-day acquire = entry:%p leader:%v, want fresh leader", next, leader)
	}
}
