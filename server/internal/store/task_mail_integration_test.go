//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"farm/server/internal/gameconf"
	"farm/server/internal/store"
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

	dayKey := gameconf.LocalDayKey(time.Now().UnixMilli())
	tasks, err := s.ListTasks(ctx, uid, dayKey)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("ListTasks returned %d tasks, want 4", len(tasks))
	}
	if tasks[store.TaskDailyLoginID-1].Progress != 1 || tasks[store.TaskDailyLoginID-1].Target != 1 {
		t.Fatalf("daily login task = %#v, want progress=target=1", tasks[store.TaskDailyLoginID-1])
	}
	if _, err := s.ClaimTask(ctx, uid, dayKey, store.TaskPlantID); !errors.Is(err, store.ErrTaskNotComplete) {
		t.Fatalf("ClaimTask before gameplay error = %v, want ErrTaskNotComplete", err)
	}
	advanced, err := s.AdvanceTask(ctx, uid, dayKey, store.TaskPlantID, 1)
	if err != nil {
		t.Fatalf("AdvanceTask: %v", err)
	}
	if !advanced.Changed || !advanced.JustCompleted || advanced.Task.ID != store.TaskPlantID ||
		advanced.Task.Progress != advanced.Task.Target || advanced.Task.Claimed {
		t.Fatalf("first task advancement = %#v", advanced)
	}
	repeated, err := s.AdvanceTask(ctx, uid, dayKey, store.TaskPlantID, 1)
	if err != nil {
		t.Fatalf("repeat AdvanceTask: %v", err)
	}
	if repeated.Changed || repeated.JustCompleted || repeated.Task != advanced.Task {
		t.Fatalf("repeated task advancement = %#v, want unchanged %#v", repeated, advanced.Task)
	}

	taskReward, err := s.ClaimTask(ctx, uid, dayKey, store.TaskPlantID)
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if taskReward.Coin == 0 {
		t.Fatalf("task reward = %#v", taskReward)
	}
	if _, err := s.ClaimTask(ctx, uid, dayKey, store.TaskPlantID); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
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
	if before.Coin != gameconf.InitialCoin {
		t.Fatalf("initial coin = %d, want %d", before.Coin, gameconf.InitialCoin)
	}
	if afterDaily.Coin != before.Coin+taskReward.Coin+dailyReward.Coin {
		t.Fatalf("coin after daily login claim = %d, want %d", afterDaily.Coin, before.Coin+taskReward.Coin+dailyReward.Coin)
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
	dayKey := gameconf.LocalDayKey(now)
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
	daily := tasks[store.TaskDailyLoginID-1]
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
	originalLocal := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	t.Cleanup(func() { time.Local = originalLocal })

	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "day_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	firstDay := gameconf.LocalDayKey(time.Date(2026, time.July, 30, 23, 59, 59, 0, time.Local).UnixMilli())
	secondDay := gameconf.LocalDayKey(time.Date(2026, time.July, 31, 0, 0, 0, 0, time.Local).UnixMilli())
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
	secondDaily := secondTasks[store.TaskDailyLoginID-1]
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

	dayKey := gameconf.LocalDayKey(time.Now().UnixMilli())
	if _, err := s.AdvanceTask(ctx, uid, dayKey, store.TaskPlantID, 1); err != nil {
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
			_, err := s.ClaimTask(context.Background(), uid, dayKey, store.TaskPlantID)
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
	if after.Coin != before.Coin+20 {
		t.Fatalf("coin after concurrent claims = %d, want %d", after.Coin, before.Coin+20)
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

	dayKey := gameconf.LocalDayKey(time.Now().UnixMilli())
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
	daily := tasks[store.TaskDailyLoginID-1]
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
