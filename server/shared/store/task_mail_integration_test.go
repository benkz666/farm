//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"farm/server/shared/gameconfig"
	"farm/server/shared/store"
)

func TestTaskStoreCreditsRewardsExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "it_task_mail_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before claim: %v", err)
	}

	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())
	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1+store.RandomDailyTaskCount {
		t.Fatalf("ListTasks returned %d tasks, want %d", len(tasks), 1+store.RandomDailyTaskCount)
	}
	daily := requireTask(t, tasks, store.TaskDailyLoginID)
	if daily.Progress != 1 || daily.Target != 1 {
		t.Fatalf("daily login task = %#v, want progress=target=1", daily)
	}
	gameplay := requireRandomTask(t, tasks)
	if _, err := s.ClaimTask(ctx, uid, dayKey, gameplay.ID); !errors.Is(err, store.ErrTaskNotComplete) {
		t.Fatalf("ClaimTask before gameplay error = %v, want ErrTaskNotComplete", err)
	}
	advanced, err := s.AdvanceTask(ctx, uid, dayKey, gameplay.ID, gameplay.Target)
	if err != nil {
		t.Fatalf("AdvanceTask: %v", err)
	}
	if !advanced.Changed || !advanced.JustCompleted || advanced.Task.ID != gameplay.ID ||
		advanced.Task.Progress != advanced.Task.Target || advanced.Task.Claimed {
		t.Fatalf("first task advancement = %#v", advanced)
	}
	repeated, err := s.AdvanceTask(ctx, uid, dayKey, gameplay.ID, 1)
	if err != nil {
		t.Fatalf("repeat AdvanceTask: %v", err)
	}
	if repeated.Changed || repeated.JustCompleted || repeated.Task.ID != 0 {
		t.Fatalf("repeated task advancement = %#v, want no changed task payload", repeated)
	}

	taskReward, err := s.ClaimTask(ctx, uid, dayKey, gameplay.ID)
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if taskReward.Coin == 0 {
		t.Fatalf("task reward = %#v", taskReward)
	}
	if _, err := s.ClaimTask(ctx, uid, dayKey, gameplay.ID); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("second ClaimTask error = %v, want ErrTaskAlreadyClaimed", err)
	}

	afterTask, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after task claim: %v", err)
	}
	if afterTask.Coin != before.Coin+taskReward.Coin {
		t.Fatalf("coin after task claim = %d, want %d", afterTask.Coin, before.Coin+taskReward.Coin)
	}

	dailyReward, err := s.ClaimDailyLogin(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ClaimDailyLogin: %v", err)
	}
	if dailyReward.Coin != 100 {
		t.Fatalf("daily reward = %#v, want 100 coins", dailyReward)
	}
	if _, err := s.ClaimTask(ctx, uid, dayKey, store.TaskDailyLoginID); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("TaskClaim daily after ClaimDailyLogin error = %v, want ErrTaskAlreadyClaimed", err)
	}

	mails, err := s.ListMails(ctx, uid)
	if err != nil {
		t.Fatalf("ListMails: %v", err)
	}
	if len(mails) != 0 {
		t.Fatalf("ListMails returned %d mails, want no reward mail", len(mails))
	}

	afterDaily, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after daily login claim: %v", err)
	}
	if before.Coin != gameconfig.InitialCoin {
		t.Fatalf("initial coin = %d, want %d", before.Coin, gameconfig.InitialCoin)
	}
	if afterDaily.Coin != before.Coin+taskReward.Coin+dailyReward.Coin {
		t.Fatalf("coin after daily login claim = %d, want %d", afterDaily.Coin, before.Coin+taskReward.Coin+dailyReward.Coin)
	}
}

func TestConcurrentColdTaskListIsReadOnly(t *testing.T) {
	const accountCount = 96

	s := newTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	uidBase := testUID(t)
	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())

	// Account creation is deliberately sequential and outside the concurrent
	// section. The assertion targets ListTasks' cold daily-task transaction, not
	// registration throughput.
	for index := 0; index < accountCount; index++ {
		uid := uidBase + uint64(index)
		username := "ti_" + strconv.FormatUint(uid, 10)
		if err := s.SaveAccount(ctx, uid, username, "hash"); err != nil {
			t.Fatalf("SaveAccount(%d): %v", uid, err)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, accountCount)
	var wait sync.WaitGroup
	for index := 0; index < accountCount; index++ {
		uid := uidBase + uint64(index)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			tasks, err := s.ListTasks(ctx, uid, dayKey)
			if err != nil {
				errs <- fmt.Errorf("ListTasks(%d): %w", uid, err)
				return
			}
			if len(tasks) != 1+store.RandomDailyTaskCount {
				errs <- fmt.Errorf("ListTasks(%d) returned %d tasks", uid, len(tasks))
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	db := openIntegrationMySQL(t)
	var persisted int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM player_task
		WHERE uid >= ? AND uid < ? AND logic_day = ?`,
		uidBase, uidBase+accountCount, dayKey,
	).Scan(&persisted); err != nil {
		t.Fatalf("count cold task rows: %v", err)
	}
	if persisted != 0 {
		t.Fatalf("cold ListTasks persisted %d rows, want zero writes", persisted)
	}
}

func TestLegacyDailyLoginBlocksTaskAndLegacyClaims(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "ld_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before claims: %v", err)
	}

	now := time.Now().UnixMilli()
	dayKey := gameconfig.LocalDayKey(now)
	db := openIntegrationMySQL(t)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO daily_login (uid, logic_day, created_at)
		VALUES (?, ?, ?)`, uid, 1, now); err != nil {
		t.Fatalf("insert legacy daily_login: %v", err)
	}

	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	daily := requireTask(t, tasks, store.TaskDailyLoginID)
	if !daily.Claimed || daily.Progress != daily.Target {
		t.Fatalf("legacy daily task = %#v, want completed and claimed", daily)
	}
	if _, err := s.ClaimTask(ctx, uid, dayKey, store.TaskDailyLoginID); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("ClaimTask daily after legacy row error = %v, want ErrTaskAlreadyClaimed", err)
	}
	if _, err := s.ClaimDailyLogin(ctx, uid, dayKey); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("ClaimDailyLogin after legacy row error = %v, want ErrTaskAlreadyClaimed", err)
	}

	after, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after blocked claims: %v", err)
	}
	if after.Coin != before.Coin {
		t.Fatalf("coin after blocked legacy claims = %d, want %d", after.Coin, before.Coin)
	}
}

func TestTask4ResetsOnNextLocalDayAndSharesClaimState(t *testing.T) {
	s := newTestStore(t)

	// Open the database before installing a synthetic fixed-zone clock. The
	// MySQL driver's loc=Local parser expects an IANA location name, while the
	// fixed zone is intentionally named only for this business-day assertion.
	originalLocal := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	t.Cleanup(func() { time.Local = originalLocal })

	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "day_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	firstDay := gameconfig.LocalDayKey(time.Date(2026, time.July, 30, 23, 59, 59, 0, time.Local).UnixMilli())
	secondDay := gameconfig.LocalDayKey(time.Date(2026, time.July, 31, 0, 0, 0, 0, time.Local).UnixMilli())
	if firstDay == secondDay {
		t.Fatalf("local day keys must differ across midnight: %d", firstDay)
	}

	if _, err := s.ClaimTask(ctx, uid, firstDay, store.TaskDailyLoginID); err != nil {
		t.Fatalf("first-day TaskClaim(4): %v", err)
	}
	if _, err := s.ClaimDailyLogin(ctx, uid, firstDay); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("first-day ClaimDailyLogin after TaskClaim(4) error = %v, want ErrTaskAlreadyClaimed", err)
	}

	secondTasks, err := s.ListTasks(ctx, uid, secondDay)
	if err != nil {
		t.Fatalf("second-day ListTasks: %v", err)
	}
	secondDaily := requireTask(t, secondTasks, store.TaskDailyLoginID)
	if secondDaily.Progress != secondDaily.Target || secondDaily.Claimed {
		t.Fatalf("second-day daily task = %#v, want completed but unclaimed", secondDaily)
	}
	if _, err := s.ClaimDailyLogin(ctx, uid, secondDay); err != nil {
		t.Fatalf("second-day ClaimDailyLogin: %v", err)
	}
	if _, err := s.ClaimTask(ctx, uid, secondDay, store.TaskDailyLoginID); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("second-day TaskClaim(4) after ClaimDailyLogin error = %v, want ErrTaskAlreadyClaimed", err)
	}
}

func TestTaskClaimConcurrentCreditsOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "tc_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before claim: %v", err)
	}

	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())
	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gameplay := requireRandomTask(t, tasks)
	if _, err := s.AdvanceTask(ctx, uid, dayKey, gameplay.ID, gameplay.Target); err != nil {
		t.Fatalf("AdvanceTask: %v", err)
	}

	const claimers = 8
	start := make(chan struct{})
	results := make(chan error, claimers)
	var group sync.WaitGroup
	group.Add(claimers)
	for range claimers {
		go func() {
			defer group.Done()
			<-start
			_, err := s.ClaimTask(context.Background(), uid, dayKey, gameplay.ID)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)

	successes := 0
	alreadyClaimed := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrTaskAlreadyClaimed):
			alreadyClaimed++
		default:
			t.Fatalf("concurrent ClaimTask error = %v", err)
		}
	}
	if successes != 1 || alreadyClaimed != claimers-1 {
		t.Fatalf("claim results: successes=%d already_claimed=%d, want 1 and %d", successes, alreadyClaimed, claimers-1)
	}

	after, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after concurrent claims: %v", err)
	}
	if after.Coin != before.Coin+gameplay.RewardCoin {
		t.Fatalf("coin after concurrent claims = %d, want %d", after.Coin, before.Coin+gameplay.RewardCoin)
	}
}

func TestMailClaimConcurrentCreditsOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "mc_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before claim: %v", err)
	}

	db := openIntegrationMySQL(t)
	const attachment = int64(321)
	result, err := db.ExecContext(ctx, `
		INSERT INTO mail (uid, title, attachment_coin, created_at)
		VALUES (?, 'concurrent reward', ?, ?)`, uid, attachment, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("insert reward mail: %v", err)
	}
	mailID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("mail ID: %v", err)
	}

	const claimers = 8
	start := make(chan struct{})
	errs := make(chan error, claimers)
	var group sync.WaitGroup
	group.Add(claimers)
	for range claimers {
		go func() {
			defer group.Done()
			<-start
			_, claimErr := s.ClaimMail(context.Background(), uid, uint64(mailID))
			errs <- claimErr
		}()
	}
	close(start)
	group.Wait()
	close(errs)

	successes, alreadyClaimed := 0, 0
	for claimErr := range errs {
		switch {
		case claimErr == nil:
			successes++
		case errors.Is(claimErr, store.ErrMailAlreadyClaimed):
			alreadyClaimed++
		default:
			t.Fatalf("concurrent ClaimMail error = %v", claimErr)
		}
	}
	if successes != 1 || alreadyClaimed != claimers-1 {
		t.Fatalf("mail claim results: successes=%d already_claimed=%d, want 1 and %d",
			successes, alreadyClaimed, claimers-1)
	}
	after, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after claim: %v", err)
	}
	if after.Coin != before.Coin+attachment {
		t.Fatalf("coin after concurrent mail claim = %d, want %d", after.Coin, before.Coin+attachment)
	}
}

func TestDirectClaimsPersistActorEconomyAndFenceOlderFarmProjection(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "fenced_claim_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before fenced claims: %v", err)
	}

	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())
	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	gameplay := requireRandomTask(t, tasks)
	if _, err := s.AdvanceTask(ctx, uid, dayKey, gameplay.ID, gameplay.Target); err != nil {
		t.Fatalf("AdvanceTask: %v", err)
	}
	taskState := store.DirectClaimState{Coin: before.Coin + 700, NextFarmSeq: before.FarmSeq + 7}
	taskReward, err := s.ClaimTaskAtState(ctx, uid, dayKey, gameplay.ID, taskState)
	if err != nil {
		t.Fatalf("ClaimTaskAtState: %v", err)
	}
	if err := s.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatalf("DeleteFarmCache after task: %v", err)
	}
	afterTask, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after task: %v", err)
	}
	if afterTask.Coin != taskState.Coin+taskReward.Coin || afterTask.FarmSeq != taskState.NextFarmSeq {
		t.Fatalf("task claim farm coin=%d seq=%d, want %d/%d",
			afterTask.Coin, afterTask.FarmSeq, taskState.Coin+taskReward.Coin, taskState.NextFarmSeq)
	}

	db := openIntegrationMySQL(t)
	const attachment = int64(321)
	result, err := db.ExecContext(ctx, `
		INSERT INTO mail (uid, title, attachment_coin, created_at)
		VALUES (?, 'fenced reward', ?, ?)`, uid, attachment, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("insert fenced reward mail: %v", err)
	}
	mailID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("mail ID: %v", err)
	}
	mailState := store.DirectClaimState{Coin: afterTask.Coin + 900, NextFarmSeq: afterTask.FarmSeq + 9}
	mail, err := s.ClaimMailAtState(ctx, uid, uint64(mailID), mailState)
	if err != nil {
		t.Fatalf("ClaimMailAtState: %v", err)
	}
	if err := s.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatalf("DeleteFarmCache after mail: %v", err)
	}
	afterMail, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after mail: %v", err)
	}
	if afterMail.Coin != mailState.Coin+mail.AttachmentCoin || afterMail.FarmSeq != mailState.NextFarmSeq {
		t.Fatalf("mail claim farm coin=%d seq=%d, want %d/%d",
			afterMail.Coin, afterMail.FarmSeq, mailState.Coin+mail.AttachmentCoin, mailState.NextFarmSeq)
	}
}

func TestDailyLoginClaimConcurrentFirstInitializationCreditsOnlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "dl_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before claim: %v", err)
	}

	dayKey := gameconfig.LocalDayKey(time.Now().UnixMilli())
	db := openIntegrationMySQL(t)
	if _, err := db.ExecContext(ctx, `DELETE FROM daily_login WHERE uid = ?`, uid); err != nil {
		t.Fatalf("clear legacy daily_login: %v", err)
	}

	const claimers = 8
	start := make(chan struct{})
	results := make(chan error, claimers)
	var group sync.WaitGroup
	group.Add(claimers)
	for range claimers {
		go func() {
			defer group.Done()
			<-start
			_, err := s.ClaimTask(context.Background(), uid, dayKey, store.TaskDailyLoginID)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)

	successes := 0
	alreadyClaimed := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrTaskAlreadyClaimed):
			alreadyClaimed++
		default:
			t.Fatalf("concurrent first daily ClaimTask error = %v", err)
		}
	}
	if successes != 1 || alreadyClaimed != claimers-1 {
		t.Fatalf("first daily claim results: successes=%d already_claimed=%d, want 1 and %d", successes, alreadyClaimed, claimers-1)
	}

	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks after concurrent daily claim: %v", err)
	}
	daily := requireTask(t, tasks, store.TaskDailyLoginID)
	if !daily.Claimed || daily.Progress != daily.Target {
		t.Fatalf("daily task after concurrent first claim = %#v, want completed and claimed", daily)
	}
	after, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after concurrent daily claims: %v", err)
	}
	if after.Coin != before.Coin+100 {
		t.Fatalf("coin after concurrent first daily claims = %d, want %d", after.Coin, before.Coin+100)
	}
}

func TestLegacyFourTaskBoardIsReconciledToLatestDefinition(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "legacy_tasks_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	now := time.Now()
	dayKey := gameconfig.LocalDayKey(now.UnixMilli())
	db := openIntegrationMySQL(t)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO player_task (uid, logic_day, task_id, progress, target, reward_coin)
		VALUES
			(?, ?, 1, 1, 1, 20),
			(?, ?, 2, 0, 1, 30),
			(?, ?, 3, 0, 1, 40),
			(?, ?, 4, 1, 1, 100)`,
		uid, dayKey, uid, dayKey, uid, dayKey, uid, dayKey,
	); err != nil {
		t.Fatalf("insert legacy task board: %v", err)
	}

	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks reconciled day: %v", err)
	}
	if len(tasks) != 1+store.RandomDailyTaskCount {
		t.Fatalf("reconciled task count = %d, want %d", len(tasks), 1+store.RandomDailyTaskCount)
	}
	for _, task := range tasks {
		if task.DayKey != dayKey {
			t.Fatalf("reconciled task has wrong day key: %#v", task)
		}
		switch task.ID {
		case store.TaskPlantID:
			if task.Title != "播种 6 次" || task.Target != 6 || task.RewardCoin != 200 {
				t.Fatalf("plant task kept legacy definition: %#v", task)
			}
		case store.TaskHarvestID:
			if task.Title != "收获 5 次" || task.Target != 5 || task.RewardCoin != 300 {
				t.Fatalf("harvest task kept legacy definition: %#v", task)
			}
		case store.TaskVisitID:
			if task.Title != "拜访好友农场 1 次" || task.Target != 1 || task.RewardCoin != 250 {
				t.Fatalf("visit task kept legacy definition: %#v", task)
			}
		}
	}
	var persistedCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM player_task WHERE uid = ? AND logic_day = ?`,
		uid, dayKey,
	).Scan(&persistedCount); err != nil {
		t.Fatalf("count reconciled task rows: %v", err)
	}
	// ListTasks 只返回按当前配置合成的规范视图，不在读取路径改写旧数据。
	// 下一次 Advance/Claim 会经 ensureDailyTasks 幂等完成物化与清理。
	if persistedCount != 4 {
		t.Fatalf("persisted task count after read = %d, want unchanged legacy rows", persistedCount)
	}
}

func TestMailReadAndDeleteAllPersistAndStayWithinUID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	firstUID := testUID(t)
	secondUID := testUID(t)
	if err := s.SaveAccount(ctx, firstUID, "mail_first_"+strconv.FormatUint(firstUID, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount first: %v", err)
	}
	if err := s.SaveAccount(ctx, secondUID, "mail_second_"+strconv.FormatUint(secondUID, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount second: %v", err)
	}

	db := openIntegrationMySQL(t)
	now := time.Now().UnixMilli()
	for _, uid := range []uint64{firstUID, firstUID, secondUID} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO mail (uid, title, attachment_coin, created_at)
			VALUES (?, ?, 0, ?)`, uid, "unread notice", now); err != nil {
			t.Fatalf("insert mail for uid %d: %v", uid, err)
		}
	}

	before, err := s.ListMails(ctx, firstUID)
	if err != nil {
		t.Fatalf("ListMails before read: %v", err)
	}
	if len(before) != 2 || before[0].Read || before[1].Read {
		t.Fatalf("first mailbox before read = %#v, want two unread mails", before)
	}

	affected, err := s.MarkMailsRead(ctx, firstUID, 0)
	if err != nil {
		t.Fatalf("MarkMailsRead all: %v", err)
	}
	if affected != 2 {
		t.Fatalf("MarkMailsRead affected = %d, want 2", affected)
	}
	afterRead, err := s.ListMails(ctx, firstUID)
	if err != nil {
		t.Fatalf("ListMails after read: %v", err)
	}
	if len(afterRead) != 2 || !afterRead[0].Read || !afterRead[1].Read {
		t.Fatalf("first mailbox after read = %#v, want two read mails", afterRead)
	}
	secondMails, err := s.ListMails(ctx, secondUID)
	if err != nil {
		t.Fatalf("ListMails second user: %v", err)
	}
	if len(secondMails) != 1 || secondMails[0].Read {
		t.Fatalf("second mailbox after first user read = %#v, want one unread mail", secondMails)
	}

	deleted, err := s.DeleteMails(ctx, firstUID, 0)
	if err != nil {
		t.Fatalf("DeleteMails all: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteMails affected = %d, want 2", deleted)
	}
	afterDelete, err := s.ListMails(ctx, firstUID)
	if err != nil {
		t.Fatalf("ListMails after delete: %v", err)
	}
	if len(afterDelete) != 0 {
		t.Fatalf("first mailbox after delete = %#v, want empty", afterDelete)
	}
	secondMails, err = s.ListMails(ctx, secondUID)
	if err != nil {
		t.Fatalf("ListMails second user after delete: %v", err)
	}
	if len(secondMails) != 1 {
		t.Fatalf("second mailbox after first user delete = %#v, want preserved", secondMails)
	}
}

func requireTask(t *testing.T, tasks []store.Task, taskID uint32) store.Task {
	t.Helper()
	for _, task := range tasks {
		if task.ID == taskID {
			return task
		}
	}
	t.Fatalf("task %d not found in %#v", taskID, tasks)
	return store.Task{}
}

func requireRandomTask(t *testing.T, tasks []store.Task) store.Task {
	t.Helper()
	for _, task := range tasks {
		if task.Kind == "random" {
			return task
		}
	}
	t.Fatalf("random task not found in %#v", tasks)
	return store.Task{}
}

func openIntegrationMySQL(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("FARM_MYSQL_DSN")
	if dsn == "" {
		dsn = "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		_ = db.Close()
		t.Fatalf("ping MySQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
