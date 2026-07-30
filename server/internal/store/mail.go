package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ListMails 返回玩家的个人邮件，附件状态由 Claimed 明示。
func (s *Store) ListMails(ctx context.Context, uid uint64) ([]Mail, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, attachment_coin, claimed_at IS NOT NULL, created_at
		FROM mail
		WHERE uid = ?
		ORDER BY id DESC`, uid)
	if err != nil {
		return nil, fmt.Errorf("store: list mails: %w", err)
	}
	defer rows.Close()

	mails := make([]Mail, 0)
	for rows.Next() {
		var mail Mail
		if err := rows.Scan(&mail.ID, &mail.Title, &mail.AttachmentCoin, &mail.Claimed, &mail.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan mail: %w", err)
		}
		mails = append(mails, mail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate mails: %w", err)
	}
	return mails, nil
}

// ClaimMail 原子标记附件、增加金币，避免重复领取资损。
func (s *Store) ClaimMail(ctx context.Context, uid uint64, mailID uint64) (Mail, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mail{}, fmt.Errorf("store: begin claim mail tx: %w", err)
	}
	defer tx.Rollback()

	var mail Mail
	var claimedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT id, title, attachment_coin, claimed_at, created_at
		FROM mail
		WHERE id = ? AND uid = ?
		FOR UPDATE`, mailID, uid).Scan(
		&mail.ID, &mail.Title, &mail.AttachmentCoin, &claimedAt, &mail.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Mail{}, ErrMailNotFound
	}
	if err != nil {
		return Mail{}, fmt.Errorf("store: load mail for claim: %w", err)
	}
	if mail.AttachmentCoin == 0 {
		return Mail{}, ErrMailNoAttachment
	}
	if claimedAt.Valid {
		return Mail{}, ErrMailAlreadyClaimed
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE player SET coin = coin + ?, updated_at = ? WHERE uid = ?`, mail.AttachmentCoin, now, uid); err != nil {
		return Mail{}, fmt.Errorf("store: credit mail attachment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mail SET claimed_at = ? WHERE id = ?`, now, mail.ID); err != nil {
		return Mail{}, fmt.Errorf("store: mark mail claimed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Mail{}, fmt.Errorf("store: commit claim mail: %w", err)
	}
	// ClaimMail 由 Gateway/FarmRPC 放在 uid 权威 Actor 的串行段内调用，并同步
	// 更新在线聚合；删除缓存同时保护离线调用者不会加载旧金币快照。
	_ = s.DeleteFarmCache(ctx, uid)
	mail.Claimed = true
	return mail, nil
}

// ClaimDailyLogin keeps command 614 compatible by delegating it to the same
// task 4 state used by ordinary ClaimTask requests.
func (s *Store) ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (TaskReward, error) {
	return s.ClaimTask(ctx, uid, dayKey, TaskDailyLoginID)
}

func createMailTx(ctx context.Context, tx *sql.Tx, uid uint64, title string, attachmentCoin, now int64) (Mail, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO mail (uid, title, attachment_coin, created_at)
		VALUES (?, ?, ?, ?)`, uid, title, attachmentCoin, now)
	if err != nil {
		return Mail{}, fmt.Errorf("store: insert mail: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Mail{}, fmt.Errorf("store: get mail ID: %w", err)
	}
	return Mail{
		ID:             uint64(id),
		Title:          title,
		AttachmentCoin: attachmentCoin,
		CreatedAt:      now,
	}, nil
}
