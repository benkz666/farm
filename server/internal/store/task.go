package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 薄任务先以已完成的桩进度交付日常领取闭环；后续本分片事件只需更新 player_task.progress。
var dailyTaskDefinitions = []struct {
	id         uint32
	title      string
	rewardCoin int64
}{
	{id: 1, title: "完成一次播种", rewardCoin: 20},
	{id: 2, title: "完成一次收获", rewardCoin: 30},
	{id: 3, title: "拜访一次好友农场", rewardCoin: 40},
}

// ListTasks 返回指定逻辑日的三条任务，首次读取会初始化 stub 进度。
func (s *Store) ListTasks(ctx context.Context, uid uint64, logicDay int64) ([]Task, error) {
	if err := s.ensureDailyTasks(ctx, nil, uid, logicDay); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, progress, target, reward_coin, claimed_at IS NOT NULL
		FROM player_task
		WHERE uid = ? AND logic_day = ?
		ORDER BY task_id`, uid, logicDay)
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

// ClaimTask 仅投递奖励邮件，附件由 ClaimMail 原子入账。
func (s *Store) ClaimTask(ctx context.Context, uid uint64, logicDay int64, taskID uint32) (Mail, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mail{}, fmt.Errorf("store: begin claim task tx: %w", err)
	}
	defer tx.Rollback()

	if err := s.ensureDailyTasks(ctx, tx, uid, logicDay); err != nil {
		return Mail{}, err
	}
	var progress, target uint32
	var rewardCoin int64
	var claimedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT progress, target, reward_coin, claimed_at
		FROM player_task
		WHERE uid = ? AND logic_day = ? AND task_id = ?
		FOR UPDATE`, uid, logicDay, taskID).Scan(&progress, &target, &rewardCoin, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Mail{}, ErrTaskNotComplete
	}
	if err != nil {
		return Mail{}, fmt.Errorf("store: load task for claim: %w", err)
	}
	if claimedAt.Valid {
		return Mail{}, ErrTaskAlreadyClaimed
	}
	if progress < target {
		return Mail{}, ErrTaskNotComplete
	}

	now := time.Now().UnixMilli()
	mail, err := createMailTx(ctx, tx, uid, "每日任务奖励："+dailyTaskTitle(taskID), rewardCoin, now)
	if err != nil {
		return Mail{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE player_task SET claimed_at = ?
		WHERE uid = ? AND logic_day = ? AND task_id = ?`,
		now, uid, logicDay, taskID); err != nil {
		return Mail{}, fmt.Errorf("store: mark task claimed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Mail{}, fmt.Errorf("store: commit claim task: %w", err)
	}
	return mail, nil
}

func (s *Store) ensureDailyTasks(ctx context.Context, tx *sql.Tx, uid uint64, logicDay int64) error {
	exec := s.db.ExecContext
	if tx != nil {
		exec = tx.ExecContext
	}
	for _, task := range dailyTaskDefinitions {
		// Stub 任务默认达标，方便在事件进度接入前验收邮件领取闭环。
		if _, err := exec(ctx, `
			INSERT IGNORE INTO player_task (uid, logic_day, task_id, progress, target, reward_coin)
			VALUES (?, ?, ?, 1, 1, ?)`, uid, logicDay, task.id, task.rewardCoin); err != nil {
			return fmt.Errorf("store: initialize daily task %d: %w", task.id, err)
		}
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
