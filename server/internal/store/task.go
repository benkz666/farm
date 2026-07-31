package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"farm/server/internal/gameconf"
)

const (
	TaskPlantID      uint32 = 1
	TaskHarvestID    uint32 = 2
	TaskVisitID      uint32 = 3
	TaskDailyLoginID uint32 = 4
	TaskWaterID      uint32 = 5
	TaskFertilizeID  uint32 = 6
	TaskSellID       uint32 = 7
	TaskTillID       uint32 = 8
	TaskWeedID       uint32 = 9
	TaskPestID       uint32 = 10
	TaskFeedDogID    uint32 = 11

	RandomDailyTaskCount = 5
)

type dailyTaskDefinition struct {
	id              uint32
	title           string
	initialProgress uint32
	target          uint32
	rewardCoin      int64
	kind            string
}

// 固定任务每天必定出现；随机任务的选择结果按 uid 与自然日稳定计算后落库。
var fixedDailyTaskDefinitions = []dailyTaskDefinition{
	{id: TaskDailyLoginID, title: "每日登录", initialProgress: 1, target: 1, rewardCoin: 100, kind: TaskKindFixed},
}

// randomDailyTaskPool 是唯一的随机每日任务池。任务的进度与奖励仍由后端权威处理。
var randomDailyTaskPool = []dailyTaskDefinition{
	{id: TaskWaterID, title: "浇水 10 次", target: 10, rewardCoin: 200, kind: TaskKindRandom},
	{id: TaskHarvestID, title: "收获 5 次", target: 5, rewardCoin: 300, kind: TaskKindRandom},
	{id: TaskVisitID, title: "拜访好友农场 1 次", target: 1, rewardCoin: 250, kind: TaskKindRandom},
	{id: TaskPlantID, title: "播种 6 次", target: 6, rewardCoin: 200, kind: TaskKindRandom},
	{id: TaskSellID, title: "出售果实 1 次", target: 1, rewardCoin: 150, kind: TaskKindRandom},
	{id: TaskFertilizeID, title: "施肥 1 次", target: 1, rewardCoin: 100, kind: TaskKindRandom},
	{id: TaskTillID, title: "开垦土地 2 次", target: 2, rewardCoin: 180, kind: TaskKindRandom},
	{id: TaskWeedID, title: "除草 2 次", target: 2, rewardCoin: 160, kind: TaskKindRandom},
	{id: TaskPestID, title: "除虫 2 次", target: 2, rewardCoin: 160, kind: TaskKindRandom},
	{id: TaskFeedDogID, title: "喂食宠物 1 次", target: 1, rewardCoin: 120, kind: TaskKindRandom},
}

const (
	TaskKindFixed  = "fixed"
	TaskKindRandom = "random"
)

// ListTasks returns the fixed and randomly selected tasks for a server-local
// calendar day. The persisted logic_day column name is retained for schema
// compatibility.
func (s *Store) ListTasks(ctx context.Context, uid uint64, dayKey int64) ([]Task, error) {
	definitions, err := s.ensureDailyTasks(ctx, uid, dayKey)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, progress, target, reward_coin, claimed_at IS NOT NULL
		FROM player_task
		WHERE uid = ? AND logic_day = ?
		ORDER BY task_id`, uid, dayKey)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()

	byID := make(map[uint32]Task, len(definitions))
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Progress, &task.Target, &task.RewardCoin, &task.Claimed); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		if definition, ok := dailyTaskDefinitionByID(definitions, task.ID); ok {
			task.DayKey = dayKey
			task.Title = definition.title
			task.Kind = definition.kind
			byID[task.ID] = task
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tasks: %w", err)
	}
	tasks := make([]Task, 0, len(definitions))
	for _, definition := range definitions {
		if task, ok := byID[definition.id]; ok {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// AdvanceTask applies one authoritative gameplay event and returns the
// persisted task state. A completed task remains unchanged on repeated events.
func (s *Store) AdvanceTask(ctx context.Context, uid uint64, dayKey int64, taskID, amount uint32) (TaskAdvanceResult, error) {
	if uid == 0 || amount == 0 || !IsDailyTaskID(taskID) {
		return TaskAdvanceResult{}, errors.New("store: invalid task progress")
	}
	definitions, err := s.ensureDailyTasks(ctx, uid, dayKey)
	if err != nil {
		return TaskAdvanceResult{}, err
	}
	definition, ok := dailyTaskDefinitionByID(definitions, taskID)
	// 这个动作今天没有被抽中时无需写库，也不应被调用方当作异常记录。
	if !ok {
		return TaskAdvanceResult{}, nil
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE player_task
		SET progress = LEAST(target, progress + ?)
		WHERE uid = ? AND logic_day = ? AND task_id = ? AND progress < target`,
		amount, uid, dayKey, taskID,
	)
	if err != nil {
		return TaskAdvanceResult{}, fmt.Errorf("store: advance task %d: %w", taskID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return TaskAdvanceResult{}, fmt.Errorf("store: inspect task %d advancement: %w", taskID, err)
	}
	// 调用方只在 Changed=true 时需要完整任务对象发送 TaskNotify。任务已满时
	// 直接返回，避免每次同类玩法动作再追加一次无效 SELECT。
	if changed == 0 {
		return TaskAdvanceResult{}, nil
	}
	var task Task
	if err := s.db.QueryRowContext(ctx, `
		SELECT task_id, progress, target, reward_coin, claimed_at IS NOT NULL
		FROM player_task
		WHERE uid = ? AND logic_day = ? AND task_id = ?`,
		uid, dayKey, taskID,
	).Scan(&task.ID, &task.Progress, &task.Target, &task.RewardCoin, &task.Claimed); err != nil {
		return TaskAdvanceResult{}, fmt.Errorf("store: load advanced task %d: %w", taskID, err)
	}
	task.DayKey = dayKey
	task.Title = definition.title
	task.Kind = definition.kind
	return TaskAdvanceResult{
		Task:          task,
		Changed:       changed > 0,
		JustCompleted: changed > 0 && task.Progress == task.Target,
	}, nil
}

// ClaimTask atomically marks and credits one calendar-day task.
func (s *Store) ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (TaskReward, error) {
	definitions, err := s.ensureDailyTasks(ctx, uid, dayKey)
	if err != nil {
		return TaskReward{}, err
	}
	if _, ok := dailyTaskDefinitionByID(definitions, taskID); !ok {
		return TaskReward{}, ErrTaskNotComplete
	}
	// Initialize outside the reward transaction. Concurrent task-board
	// reconciliation while another claimant holds this row FOR UPDATE can
	// otherwise form an InnoDB deadlock; initialization is idempotent and has no
	// reward side effect.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskReward{}, fmt.Errorf("store: begin claim task tx: %w", err)
	}
	defer tx.Rollback()

	var progress, target uint32
	var rewardCoin int64
	var claimedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT progress, target, reward_coin, claimed_at
		FROM player_task
		WHERE uid = ? AND logic_day = ? AND task_id = ?
		FOR UPDATE`, uid, dayKey, taskID).Scan(&progress, &target, &rewardCoin, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskReward{}, ErrTaskNotComplete
	}
	if err != nil {
		return TaskReward{}, fmt.Errorf("store: load task for claim: %w", err)
	}
	if claimedAt.Valid {
		return TaskReward{}, ErrTaskAlreadyClaimed
	}
	if progress < target {
		return TaskReward{}, ErrTaskNotComplete
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		UPDATE player SET coin = coin + ?, updated_at = ? WHERE uid = ?`,
		rewardCoin, now, uid); err != nil {
		return TaskReward{}, fmt.Errorf("store: credit task reward: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE player_task SET claimed_at = ?
		WHERE uid = ? AND logic_day = ? AND task_id = ?`,
		now, uid, dayKey, taskID); err != nil {
		return TaskReward{}, fmt.Errorf("store: mark task claimed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TaskReward{}, fmt.Errorf("store: commit claim task: %w", err)
	}
	_ = s.DeleteFarmCache(ctx, uid)
	return TaskReward{Coin: rewardCoin}, nil
}

type dailyTaskInitEntry struct {
	done        chan struct{}
	err         error
	definitions []dailyTaskDefinition
}

// dailyTaskInitCache 只缓存当前服务进程见过的一个自然日。数据库主键仍是
// 跨实例幂等边界；缓存仅用于跳过同一进程内重复的任务板校准和兼容迁移。
type dailyTaskInitCache struct {
	mu      sync.Mutex
	dayKey  int64
	entries map[uint64]*dailyTaskInitEntry
}

func (c *dailyTaskInitCache) acquire(uid uint64, dayKey int64) (*dailyTaskInitEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dayKey != dayKey || c.entries == nil {
		c.dayKey = dayKey
		c.entries = make(map[uint64]*dailyTaskInitEntry)
	}
	if entry, ok := c.entries[uid]; ok {
		return entry, false
	}
	entry := &dailyTaskInitEntry{done: make(chan struct{})}
	c.entries[uid] = entry
	return entry, true
}

func (c *dailyTaskInitCache) complete(uid uint64, dayKey int64, entry *dailyTaskInitEntry, definitions []dailyTaskDefinition, err error) {
	c.mu.Lock()
	entry.err = err
	entry.definitions = definitions
	if err != nil && c.dayKey == dayKey && c.entries[uid] == entry {
		delete(c.entries, uid)
	}
	close(entry.done)
	c.mu.Unlock()
}

func (s *Store) ensureDailyTasks(ctx context.Context, uid uint64, dayKey int64) ([]dailyTaskDefinition, error) {
	entry, leader := s.taskInit.acquire(uid, dayKey)
	if !leader {
		select {
		case <-entry.done:
			return entry.definitions, entry.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	definitions, err := s.initializeDailyTasks(ctx, uid, dayKey)
	s.taskInit.complete(uid, dayKey, entry, definitions, err)
	return definitions, err
}

func (s *Store) initializeDailyTasks(ctx context.Context, uid uint64, dayKey int64) ([]dailyTaskDefinition, error) {
	definitions := dailyTaskDefinitionsFor(uid, dayKey)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin daily task initialization: %w", err)
	}
	defer tx.Rollback()

	deleteQuery, deleteArgs := dailyTaskDeleteStaleQuery(uid, dayKey, definitions)
	if _, err := tx.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
		return nil, fmt.Errorf("store: remove stale daily tasks: %w", err)
	}
	query, args := dailyTaskInsertQuery(uid, dayKey, definitions)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("store: initialize daily tasks: %w", err)
	}
	// 旧每日登录映射只在本进程首次初始化该玩家当天任务时执行。
	if err := markLegacyDailyLoginClaimed(ctx, tx.ExecContext, uid, dayKey); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit daily task initialization: %w", err)
	}
	return definitions, nil
}

func dailyTaskInsertQuery(uid uint64, dayKey int64, definitions []dailyTaskDefinition) (string, []any) {
	var query strings.Builder
	query.WriteString(`
		INSERT INTO player_task (uid, logic_day, task_id, progress, target, reward_coin)
		VALUES `)
	args := make([]any, 0, len(definitions)*6)
	for index, task := range definitions {
		if index > 0 {
			query.WriteByte(',')
		}
		query.WriteString("(?, ?, ?, ?, ?, ?)")
		args = append(args, uid, dayKey, task.id, task.initialProgress, task.target, task.rewardCoin)
	}
	query.WriteString(`
		ON DUPLICATE KEY UPDATE
			progress = IF(claimed_at IS NULL, LEAST(progress, VALUES(target)), VALUES(target)),
			target = VALUES(target),
			reward_coin = VALUES(reward_coin)`)
	return query.String(), args
}

func dailyTaskDeleteStaleQuery(uid uint64, dayKey int64, definitions []dailyTaskDefinition) (string, []any) {
	var query strings.Builder
	query.WriteString(`
		DELETE FROM player_task
		WHERE uid = ? AND logic_day = ? AND task_id NOT IN (`)
	args := make([]any, 0, 2+len(definitions))
	args = append(args, uid, dayKey)
	for index, definition := range definitions {
		if index > 0 {
			query.WriteByte(',')
		}
		query.WriteByte('?')
		args = append(args, definition.id)
	}
	query.WriteByte(')')
	return query.String(), args
}

// markLegacyDailyLoginClaimed maps an old daily_login row into task 4. Legacy
// rows used accelerated logic-day keys, so created_at is the only reliable
// signal for whether the reward was already claimed during this local day.
func markLegacyDailyLoginClaimed(
	ctx context.Context,
	exec func(context.Context, string, ...any) (sql.Result, error),
	uid uint64,
	dayKey int64,
) error {
	startMs, nextStartMs, ok := gameconf.LocalDayBounds(dayKey)
	if !ok {
		return nil
	}
	if _, err := exec(ctx, `
		UPDATE player_task
		SET claimed_at = (
			SELECT MIN(created_at)
			FROM daily_login
			WHERE uid = ? AND created_at >= ? AND created_at < ?
		)
		WHERE uid = ? AND logic_day = ? AND task_id = ? AND claimed_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM daily_login
			WHERE uid = ? AND created_at >= ? AND created_at < ?
		)`,
		uid, startMs, nextStartMs,
		uid, dayKey, TaskDailyLoginID,
		uid, startMs, nextStartMs,
	); err != nil {
		return fmt.Errorf("store: map legacy daily login: %w", err)
	}
	return nil
}

func dailyTaskDefinitionsFor(uid uint64, dayKey int64) []dailyTaskDefinition {
	definitions := make([]dailyTaskDefinition, 0, len(fixedDailyTaskDefinitions)+RandomDailyTaskCount)
	definitions = append(definitions, fixedDailyTaskDefinitions...)

	pool := append([]dailyTaskDefinition(nil), randomDailyTaskPool...)
	state := uint64(dayKey) ^ (uid * 0x9e3779b97f4a7c15)
	for len(pool) > 0 && len(definitions) < len(fixedDailyTaskDefinitions)+RandomDailyTaskCount {
		// xorshift64：仅用于稳定抽取，不承担安全随机职责。
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		index := int(state % uint64(len(pool)))
		definitions = append(definitions, pool[index])
		pool = append(pool[:index], pool[index+1:]...)
	}
	return definitions
}

func dailyTaskDefinitionByID(definitions []dailyTaskDefinition, taskID uint32) (dailyTaskDefinition, bool) {
	for _, definition := range definitions {
		if definition.id == taskID {
			return definition, true
		}
	}
	return dailyTaskDefinition{}, false
}

// IsDailyTaskID reports whether an ID belongs to the current server-owned pool.
func IsDailyTaskID(taskID uint32) bool {
	for _, definition := range fixedDailyTaskDefinitions {
		if definition.id == taskID {
			return true
		}
	}
	for _, definition := range randomDailyTaskPool {
		if definition.id == taskID {
			return true
		}
	}
	return false
}
