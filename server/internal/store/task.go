package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"farm/server/internal/gameconf"
)

const (
	TaskPlantID      uint32 = 1
	TaskHarvestID    uint32 = 2
	TaskVisitID      uint32 = 3
	TaskDailyLoginID uint32 = 4
)

type dailyTaskDefinition struct {
	id              uint32
	title           string
	initialProgress uint32
	target          uint32
	rewardCoin      int64
}

var dailyTaskDefinitions = []dailyTaskDefinition{
	{id: TaskPlantID, title: "完成一次播种", target: 1, rewardCoin: 20},
	{id: TaskHarvestID, title: "完成一次收获", target: 1, rewardCoin: 30},
	{id: TaskVisitID, title: "拜访一次好友农场", target: 1, rewardCoin: 40},
	{id: TaskDailyLoginID, title: "每日登录", initialProgress: 1, target: 1, rewardCoin: 100},
}

// ListTasks returns the four tasks for a server-local calendar day. The
// persisted logic_day column name is retained for schema compatibility.
func (s *Store) ListTasks(ctx context.Context, uid uint64, dayKey int64) ([]Task, error) {
	if err := s.ensureDailyTasks(ctx, nil, uid, dayKey); err != nil {
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

	tasks := make([]Task, 0, len(dailyTaskDefinitions))
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Progress, &task.Target, &task.RewardCoin, &task.Claimed); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		task.Title = dailyTaskTitle(task.ID)
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tasks: %w", err)
	}
	return tasks, nil
}

// AdvanceTask applies one authoritative gameplay event and returns the
// persisted task state. A completed task remains unchanged on repeated events.
func (s *Store) AdvanceTask(ctx context.Context, uid uint64, dayKey int64, taskID, amount uint32) (TaskAdvanceResult, error) {
	if uid == 0 || amount == 0 || dailyTaskTitle(taskID) == "未知任务" {
		return TaskAdvanceResult{}, errors.New("store: invalid task progress")
	}
	if err := s.ensureDailyTasks(ctx, nil, uid, dayKey); err != nil {
		return TaskAdvanceResult{}, err
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
	var task Task
	if err := s.db.QueryRowContext(ctx, `
		SELECT task_id, progress, target, reward_coin, claimed_at IS NOT NULL
		FROM player_task
		WHERE uid = ? AND logic_day = ? AND task_id = ?`,
		uid, dayKey, taskID,
	).Scan(&task.ID, &task.Progress, &task.Target, &task.RewardCoin, &task.Claimed); err != nil {
		return TaskAdvanceResult{}, fmt.Errorf("store: load advanced task %d: %w", taskID, err)
	}
	task.Title = dailyTaskTitle(task.ID)
	return TaskAdvanceResult{
		Task:          task,
		Changed:       changed > 0,
		JustCompleted: changed > 0 && task.Progress == task.Target,
	}, nil
}

// ClaimTask atomically marks and credits one calendar-day task.
func (s *Store) ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (TaskReward, error) {
	// Initialize outside the reward transaction. Concurrent INSERT IGNORE calls
	// while another claimant holds the task row FOR UPDATE can otherwise form an
	// InnoDB deadlock; initialization is idempotent and has no reward side effect.
	if err := s.ensureDailyTasks(ctx, nil, uid, dayKey); err != nil {
		return TaskReward{}, err
	}
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

func (s *Store) ensureDailyTasks(ctx context.Context, tx *sql.Tx, uid uint64, dayKey int64) error {
	exec := s.db.ExecContext
	if tx != nil {
		exec = tx.ExecContext
	}
	for _, task := range dailyTaskDefinitions {
		if _, err := exec(ctx, `
			INSERT IGNORE INTO player_task (uid, logic_day, task_id, progress, target, reward_coin)
			VALUES (?, ?, ?, ?, ?, ?)`,
			uid, dayKey, task.id, task.initialProgress, task.target, task.rewardCoin,
		); err != nil {
			return fmt.Errorf("store: initialize daily task %d: %w", task.id, err)
		}
	}
	if err := markLegacyDailyLoginClaimed(ctx, exec, uid, dayKey); err != nil {
		return err
	}
	return nil
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

func dailyTaskTitle(taskID uint32) string {
	for _, task := range dailyTaskDefinitions {
		if task.id == taskID {
			return task.title
		}
	}
	return "未知任务"
}
