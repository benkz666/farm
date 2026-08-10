//go:build integration

package store

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"
)

func TestFarmWriteJournalProjectsFarmAndTaskToMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := os.Getenv("FARM_MYSQL_DSN")
	if dsn == "" {
		dsn = "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"
	}
	cacheAddr := os.Getenv("FARM_REDIS_ADDR")
	if cacheAddr == "" {
		cacheAddr = "127.0.0.1:6379"
	}
	eventAddr := os.Getenv("FARM_EVENT_REDIS_ADDR")
	if eventAddr == "" {
		eventAddr = "127.0.0.1:6380"
	}

	base, closeBase, err := Open(ctx, dsn, cacheAddr, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBase()

	uid := uint64(time.Now().UnixNano())
	username := "journal_it_" + strconv.FormatUint(uid, 10)
	if err := base.SaveAccount(ctx, uid, username, "hash"); err != nil {
		t.Fatal(err)
	}
	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())
	definitions := dailyTaskDefinitionsFor(uid, dayKey)
	var taskID uint32
	for _, definition := range definitions {
		if definition.id != TaskDailyLoginID && definition.target >= 3 {
			taskID = definition.id
			break
		}
	}
	if taskID == 0 {
		t.Fatal("no gameplay task selected")
	}
	aggregate, err := base.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Coin += 321
	aggregate.FarmSeq++

	config := DefaultFarmWriteJournalConfig("journal-it")
	config.Prefix = "farm:test:write:" + strconv.FormatUint(uid, 10)
	config.Shards = 2
	config.BatchSize = 16
	config.Block = 10 * time.Millisecond
	journal, closeJournal, err := OpenFarmWriteJournal(ctx, base, eventAddr, config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeJournal()
	if err := journal.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = journal.Shutdown(shutdownCtx)
		for shard := 0; shard < config.Shards; shard++ {
			_ = journal.rdb.Del(context.Background(), journal.streamKey(shard), journal.latestKey(shard, uid)).Err()
		}
	}()

	wrapper := journal.WrapFarmStore(base)
	if batch, ok := wrapper.(interface {
		CommitFarms(context.Context, []outbox.FarmCommit) error
	}); !ok {
		t.Fatal("journal farm wrapper has no CommitFarms")
	} else if err := batch.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: aggregate,
		TaskAdvances: []outbox.TaskAdvance{{
			DayKey: dayKey, TaskID: taskID, Amount: 1,
		}},
		Plan: outbox.PersistPlan{Mode: outbox.PersistEconomy},
	}}); err != nil {
		t.Fatal(err)
	}
	if latest, err := wrapper.LoadFarm(ctx, uid); err != nil || latest.Coin != aggregate.Coin {
		t.Fatalf("latest journal farm coin=%v err=%v", latest, err)
	}
	if err := journal.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := base.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatal(err)
	}
	materialized, err := base.LoadFarm(ctx, uid)
	if err != nil || materialized.Coin != aggregate.Coin || materialized.FarmSeq != aggregate.FarmSeq {
		t.Fatalf("materialized farm=%#v err=%v", materialized, err)
	}

	tasks, err := base.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range tasks {
		if task.ID == taskID {
			found = task.Progress == 1
		}
	}
	if !found {
		t.Fatalf("bundled task %d was not materialized: %#v", taskID, tasks)
	}

	if _, err := journal.AdvanceTask(ctx, uid, dayKey, taskID, 1); err != nil {
		t.Fatal(err)
	}
	if err := journal.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	tasks, err = base.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, task := range tasks {
		if task.ID == taskID {
			found = task.Progress == 2
		}
	}
	if !found {
		t.Fatalf("task %d was not materialized: %#v", taskID, tasks)
	}

	// Simulate the exact crash window between MySQL COMMIT and Redis XACK.
	// Replaying the same stream position must not increment task progress twice.
	projection := journalTaskProjection{
		uid: uid, dayKey: dayKey, taskID: taskID, amount: 1,
		streamMS: uint64(time.Now().UnixMilli() + 1000), streamSeq: 0,
	}
	if _, err := base.materializeTaskJournal(ctx, []journalTaskProjection{projection}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.materializeTaskJournal(ctx, []journalTaskProjection{projection}); err != nil {
		t.Fatal(err)
	}
	tasks, err = base.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == taskID && task.Progress != 3 {
			t.Fatalf("replayed task progress=%d, want 3", task.Progress)
		}
	}
}
