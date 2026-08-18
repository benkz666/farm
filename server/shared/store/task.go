package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"farm/server/shared/gameconfig"
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
	cacheKey := taskReadKey{uid: uid, dayKey: dayKey}
	if tasks, ok := s.taskRead.get(cacheKey, time.Now()); ok {
		return cloneTasks(tasks), nil
	}
	generation := s.taskCacheGeneration(cacheKey)
	// 每日任务的选择只取决于 uid + dayKey，因此列表冷读无需先执行
	// INSERT/DELETE/兼容迁移事务。进度与领取操作仍会通过 ensureDailyTasks
	// 幂等物化任务板；这里只把尚未落库的定义合成为零写入的只读视图。
	definitions := dailyTaskDefinitionsFor(uid, dayKey)
	startMs, nextStartMs, validDay := gameconfig.LocalDayBounds(dayKey)
	if !validDay {
		startMs, nextStartMs = 0, 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, progress, target, reward_coin,
		       claimed_at IS NOT NULL, FALSE AS legacy_daily_claimed
		FROM player_task
		WHERE uid = ? AND logic_day = ?
		UNION ALL
		SELECT 0, 0, 0, 0, FALSE,
		       EXISTS(
		           SELECT 1 FROM daily_login
		           WHERE uid = ? AND created_at >= ? AND created_at < ?
		       )`, uid, dayKey, uid, startMs, nextStartMs)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()

	type persistedTask struct {
		progress uint32
		claimed  bool
	}
	byID := make(map[uint32]persistedTask, len(definitions))
	legacyDailyClaimed := false
	for rows.Next() {
		var id, progress, ignoredTarget uint32
		var ignoredReward int64
		var claimed, legacyClaimed bool
		if err := rows.Scan(&id, &progress, &ignoredTarget, &ignoredReward, &claimed, &legacyClaimed); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		legacyDailyClaimed = legacyDailyClaimed || legacyClaimed
		if _, ok := dailyTaskDefinitionByID(definitions, id); ok {
			byID[id] = persistedTask{progress: progress, claimed: claimed}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tasks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close task rows: %w", err)
	}

	tasks := make([]Task, 0, len(definitions))
	for _, definition := range definitions {
		task := Task{
			ID:         definition.id,
			DayKey:     dayKey,
			Kind:       definition.kind,
			Title:      definition.title,
			Progress:   definition.initialProgress,
			Target:     definition.target,
			RewardCoin: definition.rewardCoin,
		}
		if persisted, ok := byID[definition.id]; ok {
			task.Progress = min(persisted.progress, definition.target)
			task.Claimed = persisted.claimed
			if task.Claimed {
				task.Progress = task.Target
			}
		}
		if definition.id == TaskDailyLoginID && legacyDailyClaimed {
			task.Progress = task.Target
			task.Claimed = true
		}
		tasks = append(tasks, task)
	}
	s.putTaskReadIfCurrent(cacheKey, generation, tasks)
	return tasks, nil
}

// AdvanceTask applies one authoritative gameplay event and returns the
// persisted task state. A completed task remains unchanged on repeated events.
func (s *Store) AdvanceTask(ctx context.Context, uid uint64, dayKey int64, taskID, amount uint32) (TaskAdvanceResult, error) {
	if uid == 0 || amount == 0 || !IsDailyTaskID(taskID) {
		return TaskAdvanceResult{}, errors.New("store: invalid task progress")
	}
	definitions := dailyTaskDefinitionsFor(uid, dayKey)
	definition, ok := dailyTaskDefinitionByID(definitions, taskID)
	// 这个动作今天没有被抽中时无需写库，也不应被调用方当作异常记录。
	if !ok {
		return TaskAdvanceResult{}, nil
	}
	initialProgress := definition.target
	if remaining := definition.target - definition.initialProgress; amount < remaining {
		initialProgress = definition.initialProgress + amount
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO player_task (
			uid, logic_day, task_id, progress, target, reward_coin
		) VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			progress = IF(
				claimed_at IS NULL,
				LEAST(VALUES(target), progress + ?),
				progress
			),
			target = VALUES(target),
			reward_coin = VALUES(reward_coin)`,
		uid, dayKey, taskID, initialProgress, definition.target, definition.rewardCoin, amount,
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
	s.invalidateTaskCache(taskReadKey{uid: uid, dayKey: dayKey})
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
	return s.claimTask(ctx, uid, dayKey, taskID, nil)
}

// ClaimTaskAtState atomically claims a completed task and replaces player.coin
// with the Actor-authoritative value plus the reward. Advancing farm_seq in
// the same statement prevents an older asynchronous farm projection from
// overwriting the direct reward after this transaction commits.
func (s *Store) ClaimTaskAtState(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	taskID uint32,
	state DirectClaimState,
) (TaskReward, error) {
	if state.NextFarmSeq == 0 {
		return TaskReward{}, errors.New("store: direct task claim has invalid next farm sequence")
	}
	return s.claimTask(ctx, uid, dayKey, taskID, &state)
}

func (s *Store) claimTask(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	taskID uint32,
	state *DirectClaimState,
) (TaskReward, error) {
	return s.claimTaskWithExecer(ctx, uid, dayKey, taskID, state, s.db)
}

func (s *Store) claimTaskWithExecer(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	taskID uint32,
	state *DirectClaimState,
	exec sqlContextExecer,
) (TaskReward, error) {
	definition, ok := dailyTaskDefinitionByID(dailyTaskDefinitionsFor(uid, dayKey), taskID)
	if !ok {
		return TaskReward{}, ErrTaskNotComplete
	}

	reward, missing, err := s.claimMaterializedTaskWithExecer(ctx, uid, dayKey, definition, state, exec)
	if err != nil || !missing {
		return reward, err
	}
	// Daily login starts completed even before its row exists. Only that cold
	// compatibility path needs to materialize the whole daily board (including
	// legacy daily_login reconciliation). Gameplay tasks with no row have no
	// persisted progress and therefore cannot be claimed.
	if taskID != TaskDailyLoginID {
		return TaskReward{}, ErrTaskNotComplete
	}
	if _, err := s.ensureDailyTasks(ctx, uid, dayKey); err != nil {
		return TaskReward{}, err
	}
	reward, _, err = s.claimMaterializedTaskWithExecer(ctx, uid, dayKey, definition, state, exec)
	return reward, err
}

// claimMaterializedTask credits player and marks the task in one atomic joined
// UPDATE. The previous success path used BEGIN + SELECT FOR UPDATE + two UPDATEs
// + COMMIT; under a burst of targeted projections those five exchanges were a
// large part of TaskClaim's tail. Definition values are deterministic server
// configuration, so no preliminary read is needed to obtain the reward.
func (s *Store) claimMaterializedTask(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	definition dailyTaskDefinition,
	state *DirectClaimState,
) (reward TaskReward, missing bool, err error) {
	return s.claimMaterializedTaskWithExecer(ctx, uid, dayKey, definition, state, s.db)
}

func (s *Store) claimMaterializedTaskWithExecer(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	definition dailyTaskDefinition,
	state *DirectClaimState,
	exec sqlContextExecer,
) (reward TaskReward, missing bool, err error) {
	now := time.Now().UnixMilli()
	var result sql.Result
	if state == nil {
		result, err = exec.ExecContext(ctx, `
			UPDATE player AS p
			JOIN player_task AS t ON t.uid = p.uid
				AND t.logic_day = ? AND t.task_id = ?
			SET p.coin = p.coin + ?, p.updated_at = ?,
				t.target = ?, t.reward_coin = ?, t.claimed_at = ?
			WHERE p.uid = ? AND t.claimed_at IS NULL AND t.progress >= ?`,
			dayKey, definition.id, definition.rewardCoin, now,
			definition.target, definition.rewardCoin, now, uid, definition.target,
		)
	} else {
		result, err = exec.ExecContext(ctx, `
			UPDATE player AS p
			JOIN player_task AS t ON t.uid = p.uid
				AND t.logic_day = ? AND t.task_id = ?
			SET p.coin = ? + ?, p.farm_seq = ?, p.updated_at = ?,
				t.target = ?, t.reward_coin = ?, t.claimed_at = ?
			WHERE p.uid = ? AND t.claimed_at IS NULL AND t.progress >= ?`,
			dayKey, definition.id, state.Coin, definition.rewardCoin, state.NextFarmSeq, now,
			definition.target, definition.rewardCoin, now, uid, definition.target,
		)
	}
	if err != nil {
		return TaskReward{}, false, fmt.Errorf("store: atomically claim task: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return TaskReward{}, false, fmt.Errorf("store: inspect claimed task: %w", err)
	}
	if affected > 0 {
		s.invalidateTaskCache(taskReadKey{uid: uid, dayKey: dayKey})
		s.invalidateFarmAfterDirectClaim(uid)
		return TaskReward{Coin: definition.rewardCoin}, false, nil
	}

	var progress uint32
	var claimedAt sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT progress, claimed_at
		FROM player_task
		WHERE uid = ? AND logic_day = ? AND task_id = ?`,
		uid, dayKey, definition.id,
	).Scan(&progress, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskReward{}, true, nil
	}
	if err != nil {
		return TaskReward{}, false, fmt.Errorf("store: diagnose unclaimed task: %w", err)
	}
	if claimedAt.Valid {
		return TaskReward{}, false, ErrTaskAlreadyClaimed
	}
	if progress < definition.target {
		return TaskReward{}, false, ErrTaskNotComplete
	}
	// A complete, unclaimed task can only miss the joined update when the player
	// row is absent/corrupt. Do not misreport it as a client retry condition.
	return TaskReward{}, false, ErrPlayerNotFound
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

	// Upsert the selected rows before deleting stale definitions. Under MySQL's
	// default REPEATABLE READ isolation, deleting an empty (uid, day) range first
	// takes next-key/gap locks. Concurrent cold initialization of adjacent UIDs
	// can then deadlock when every transaction tries to insert into a gap held by
	// another transaction. Materializing each UID's selected rows first confines
	// the following DELETE to that UID's existing primary-key records.
	query, args := dailyTaskInsertQuery(uid, dayKey, definitions)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("store: initialize daily tasks: %w", err)
	}
	deleteQuery, deleteArgs := dailyTaskDeleteStaleQuery(uid, dayKey, definitions)
	if _, err := tx.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
		return nil, fmt.Errorf("store: remove stale daily tasks: %w", err)
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
	startMs, nextStartMs, ok := gameconfig.LocalDayBounds(dayKey)
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
