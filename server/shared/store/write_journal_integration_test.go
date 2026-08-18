//go:build integration

package store

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	farmv1 "farm/server/gen/farm/v1"
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
	mailResult, err := base.db.ExecContext(ctx, `
		INSERT INTO mail (uid, title, attachment_coin, created_at) VALUES (?, ?, ?, ?)`,
		uid, "async attachment", int64(30), time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	mailID, err := mailResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Coin += 321 + 100 + 30
	aggregate.FarmSeq++

	config := DefaultFarmWriteJournalConfig("journal-it")
	config.Prefix = "farm:test:write:" + strconv.FormatUint(uid, 10)
	config.Shards = 2
	config.Projectors = 1
	config.BatchSize = 16
	config.Block = 10 * time.Millisecond
	journal, closeJournal, err := OpenFarmWriteJournal(ctx, base, cacheAddr, config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeJournal()
	if err := journal.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !journal.targetedReady.Load() {
		t.Fatal("targeted projection was not enabled for an empty test journal")
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = journal.Shutdown(shutdownCtx)
		for shard := 0; shard < config.Shards; shard++ {
			_ = journal.rdb.Del(
				context.Background(), journal.streamKey(shard), journal.latestKey(shard, uid), journal.pendingUIDKey(shard, uid),
			).Err()
		}
	}()
	// Hold the ordinary projector so LoadFarm must use the UID-targeted path
	// instead of succeeding only because the background shard drained first.
	if !journal.projectLimiter.Acquire(ctx) {
		t.Fatal("reserve background projector slot")
	}

	wrapper := journal.WrapFarmStore(base)
	batch, ok := wrapper.(interface {
		CommitFarms(context.Context, []outbox.FarmCommit) error
	})
	if !ok {
		t.Fatal("journal farm wrapper has no CommitFarms")
	}
	if err := batch.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: aggregate,
		TaskAdvances: []outbox.TaskAdvance{{
			DayKey: dayKey, TaskID: taskID, Amount: 1,
		}},
		TaskClaims: []outbox.TaskClaim{{
			DayKey: dayKey, TaskID: TaskDailyLoginID, ClaimedAt: time.Now().UnixMilli(),
		}},
		MailMutations: []outbox.MailMutation{{
			MailID: uint64(mailID), Kind: outbox.MailClaim, OccurredAt: time.Now().UnixMilli(),
		}},
		Plan: outbox.PersistPlan{Mode: outbox.PersistEconomy},
	}}); err != nil {
		t.Fatal(err)
	}
	if latest, err := wrapper.LoadFarm(ctx, uid); err != nil || latest.Coin != aggregate.Coin {
		t.Fatalf("latest journal farm coin=%v err=%v", latest, err)
	}
	journal.projectLimiter.Release()
	if pending, err := journal.lookupRDB.LLen(ctx, journal.pendingUIDKey(journal.shard(uid), uid)).Result(); err != nil || pending != 0 {
		t.Fatalf("targeted projection pending=%d err=%v, want empty", pending, err)
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
	dailyClaimed := false
	for _, task := range tasks {
		if task.ID == TaskDailyLoginID {
			dailyClaimed = task.Claimed
		}
	}
	if !dailyClaimed {
		t.Fatalf("bundled task claim was not materialized: %#v", tasks)
	}
	mails, err := base.ListMails(ctx, uid)
	if err != nil || len(mails) != 1 || !mails[0].Claimed || !mails[0].Read {
		t.Fatalf("bundled mail claim was not materialized: mails=%#v err=%v", mails, err)
	}

	// Read/delete records carry no farm row and keep the same FarmSeq. They must
	// still project, replay idempotently, and avoid resurrecting the attachment.
	if err := batch.CommitFarms(ctx, []outbox.FarmCommit{{
		Snapshot: aggregate,
		MailMutations: []outbox.MailMutation{{
			MailID: uint64(mailID), Kind: outbox.MailRead, OccurredAt: time.Now().UnixMilli(),
		}, {
			MailID: uint64(mailID), Kind: outbox.MailDelete, OccurredAt: time.Now().UnixMilli(),
		}},
		Plan: outbox.PersistPlan{Mode: outbox.PersistSideEffects},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	mails, err = base.ListMails(ctx, uid)
	if err != nil || len(mails) != 0 {
		t.Fatalf("bundled mail delete was not materialized: mails=%#v err=%v", mails, err)
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

	// A targeted projection may commit only the first event before the ordinary
	// shard worker replays an overlapping larger batch. The batch upsert must
	// skip the old stream position while still applying the newer one exactly
	// once.
	newer := projection
	newer.streamMS++
	if _, err := base.materializeTaskJournal(ctx, []journalTaskProjection{projection, newer}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.materializeTaskJournal(ctx, []journalTaskProjection{projection, newer}); err != nil {
		t.Fatal(err)
	}
	tasks, err = base.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == taskID && task.Progress != 4 {
			t.Fatalf("partially replayed task progress=%d, want 4", task.Progress)
		}
	}
}

func TestFarmMutationReplayDoesNotOverwriteDirectReward(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsn := os.Getenv("FARM_MYSQL_DSN")
	if dsn == "" {
		dsn = "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"
	}
	cacheAddr := os.Getenv("FARM_REDIS_ADDR")
	if cacheAddr == "" {
		cacheAddr = "127.0.0.1:6379"
	}
	base, closeBase, err := Open(ctx, dsn, cacheAddr, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBase()

	uid := uint64(time.Now().UnixNano())
	username := "jr_" + strconv.FormatUint(uid, 10)
	if err := base.SaveAccount(ctx, uid, username, "hash"); err != nil {
		t.Fatal(err)
	}
	aggregate, err := base.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Coin = 100
	aggregate.FarmSeq++
	mutation, err := outbox.NewFarmWriteMutation(
		aggregate, outbox.PersistPlan{Mode: outbox.PersistEconomy}, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeFarmMutations(ctx, []*farmv1.FarmWriteMutation{mutation}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.db.ExecContext(ctx, `UPDATE player SET coin = coin + 50 WHERE uid = ?`, uid); err != nil {
		t.Fatal(err)
	}
	// Simulate a shard projector replaying a batch that targeted projection had
	// already applied before the direct claim transaction.
	if err := base.MaterializeFarmMutations(ctx, []*farmv1.FarmWriteMutation{mutation}); err != nil {
		t.Fatal(err)
	}
	if err := base.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatal(err)
	}
	reloaded, err := base.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Coin != 150 || reloaded.FarmSeq != aggregate.FarmSeq {
		t.Fatalf("replayed farm coin=%d seq=%d, want coin=150 seq=%d", reloaded.Coin, reloaded.FarmSeq, aggregate.FarmSeq)
	}
}
