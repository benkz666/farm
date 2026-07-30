//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("FARM_MYSQL_DSN")
	if dsn == "" {
		dsn = "farm:farm@tcp(127.0.0.1:3306)/farm?parseTime=true&loc=Local"
	}
	addr := os.Getenv("FARM_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, closeFn, err := store.Open(ctx, dsn, addr, time.Minute)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Logf("close store: %v", err)
		}
	})
	return s
}

var testUIDSeq atomic.Uint64

// testUID 用纳秒时间戳叠加自增序号拼一个测试专属 uid，避免同一进程内连续调用碰撞。
func testUID(t *testing.T) uint64 {
	t.Helper()
	return uint64(time.Now().UnixNano()) + testUIDSeq.Add(1)
}

// TestFarmSaveLoadAndRedisBackfill 覆盖规格 9.1 第 6 条：
// 注册路径写入新聚合 -> 显式 DEL 掉 Redis 缓存 -> LoadFarm 应从 MySQL 回填，
// 且回填后 Redis 再次命中（第二次 LoadFarm 不再触碰 MySQL 也能拿到一致数据）。
func TestFarmSaveLoadAndRedisBackfill(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	username := "it_user_" + time.Now().Format("150405.000000")

	if err := s.SaveAccount(ctx, uid, username, "bcrypt-hash-placeholder"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	loaded, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm (first, warms cache): %v", err)
	}
	assertFreshFarm(t, loaded, uid, username)

	// 模拟 Redis 缓存丢失：直接删掉 farm:{uid}，验证下一次 LoadFarm 能从 MySQL 回填。
	if err := s.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatalf("DeleteFarmCache: %v", err)
	}

	backfilled, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm (after redis DEL, backfill from MySQL): %v", err)
	}
	assertFreshFarm(t, backfilled, uid, username)

	// 回填后 Redis 应已再次命中：改一份内存态写回，SaveFarm 应更新 MySQL 与缓存两处，
	// 随后 LoadFarm 命中缓存即可看到更新（不再依赖 MySQL 才能读到新值本身不可直接验证，
	// 但至少确认端到端读写链路一致）。
	backfilled.Coin = 12345
	backfilled.UnlockedPlots = 7
	if err := s.SaveFarm(ctx, backfilled); err != nil {
		t.Fatalf("SaveFarm: %v", err)
	}

	afterSave, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm (after SaveFarm): %v", err)
	}
	if afterSave.Coin != 12345 || afterSave.UnlockedPlots != 7 {
		t.Fatalf("want coin=12345 unlocked_plots=7, got coin=%d unlocked_plots=%d", afterSave.Coin, afterSave.UnlockedPlots)
	}
}

// TestSessionStorePutGetDelete 覆盖 SessionStore 的基本 round-trip 与删除后 miss。
func TestSessionStorePutGetDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	token := "it-token-" + time.Now().Format("150405.000000")
	uid := testUID(t)

	if err := s.Put(ctx, token, uid, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, token)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != uid {
		t.Fatalf("want uid=%d got %d", uid, got)
	}

	if err := s.Delete(ctx, token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, token); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("want ErrSessionNotFound after delete, got %v", err)
	}
}

func TestSessionStoreLatestLoginReplacesPreviousToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	firstToken := "it-first-token-" + suffix
	secondToken := "it-second-token-" + suffix
	uid := testUID(t)

	if err := s.Put(ctx, firstToken, uid, time.Minute); err != nil {
		t.Fatalf("Put first token: %v", err)
	}
	if err := s.Put(ctx, secondToken, uid, time.Minute); err != nil {
		t.Fatalf("Put second token: %v", err)
	}
	if _, err := s.Get(ctx, firstToken); !errors.Is(err, store.ErrSessionReplaced) {
		t.Fatalf("first token Get = %v, want ErrSessionReplaced", err)
	}
	got, err := s.Get(ctx, secondToken)
	if err != nil || got != uid {
		t.Fatalf("second token Get = (%d, %v), want (%d, nil)", got, err, uid)
	}

	if err := s.Delete(ctx, firstToken); err != nil {
		t.Fatalf("Delete first token: %v", err)
	}
	got, err = s.Get(ctx, secondToken)
	if err != nil || got != uid {
		t.Fatalf("second token after old Delete = (%d, %v), want (%d, nil)", got, err, uid)
	}
}

// TestSaveAccountDuplicateUsername 覆盖规格 4.5 节：用户名已占用应可判定（ErrUsernameTaken）。
func TestSaveAccountDuplicateUsername(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	username := "it_dup_" + time.Now().Format("150405.000000")

	if err := s.SaveAccount(ctx, testUID(t), username, "hash-a"); err != nil {
		t.Fatalf("first SaveAccount: %v", err)
	}
	err := s.SaveAccount(ctx, testUID(t), username, "hash-b")
	if !errors.Is(err, store.ErrUsernameTaken) {
		t.Fatalf("want ErrUsernameTaken, got %v", err)
	}
}

// TestGetAccountByUsernameNotFound 覆盖登录时用户名不存在的场景。
func TestGetAccountByUsernameNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _, err := s.GetAccountByUsername(ctx, "no-such-user-"+time.Now().Format("150405.000000"))
	if !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("want ErrAccountNotFound, got %v", err)
	}
}

func assertFreshFarm(t *testing.T, agg *farm.Aggregate, uid uint64, username string) {
	t.Helper()
	if agg.UID != uid {
		t.Fatalf("want uid=%d got %d", uid, agg.UID)
	}
	if agg.Nickname != username {
		t.Fatalf("want nickname=%s got %s", username, agg.Nickname)
	}
	if agg.Coin != gameconf.InitialCoin {
		t.Fatalf("want coin=%d got %d", gameconf.InitialCoin, agg.Coin)
	}
	if agg.UnlockedPlots != gameconf.InitialUnlockedPlots {
		t.Fatalf("want unlocked_plots=%d got %d", gameconf.InitialUnlockedPlots, agg.UnlockedPlots)
	}
	if len(agg.Plots) != gameconf.MaxPlots {
		t.Fatalf("want %d plots got %d", gameconf.MaxPlots, len(agg.Plots))
	}
	if agg.Items == nil {
		t.Fatal("Items map is nil")
	}
	for i, p := range agg.Plots {
		if p.State != farm.StateWasteland || p.CropID != 0 {
			t.Fatalf("plot[%d] want wasteland/crop=0, got state=%d crop=%d", i, p.State, p.CropID)
		}
	}
}

// TestItemPersistenceAfterPlant 覆盖期 2 Task 4：种子数量变更经 SaveFarm 后可从 MySQL 回填。
func TestItemPersistenceAfterPlant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	username := "it_item_" + time.Now().Format("150405.000000")

	if err := s.SaveAccount(ctx, uid, username, "bcrypt-hash-placeholder"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	agg, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm: %v", err)
	}
	agg.Items[farm.SeedItem(1)] = 2
	agg.Plots[0] = farm.Plot{State: farm.StateTilled}
	result := agg.ApplyPlotAction(farm.PlotAction{Kind: farm.Plant, PlotIndex: 0, Arg: 1}, 10_000)
	if result.Err != 0 {
		t.Fatalf("Plant Err = %d, want 0", result.Err)
	}
	if got := agg.Items[farm.SeedItem(1)]; got != 1 {
		t.Fatalf("seed count after plant = %d, want 1", got)
	}
	if err := s.SaveFarm(ctx, agg); err != nil {
		t.Fatalf("SaveFarm: %v", err)
	}
	if err := s.DeleteFarmCache(ctx, uid); err != nil {
		t.Fatalf("DeleteFarmCache: %v", err)
	}

	reloaded, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after save: %v", err)
	}
	if got := reloaded.Items[farm.SeedItem(1)]; got != 1 {
		t.Fatalf("persisted seed count = %d, want 1", got)
	}
	if reloaded.Plots[0].State != farm.StateGrowing || reloaded.Plots[0].CropID != 1 {
		t.Fatalf("plot after reload = %#v, want growing white radish", reloaded.Plots[0])
	}
}
