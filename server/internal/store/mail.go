package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

const dailyLoginRewardCoin int64 = 100

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
	// 领取附件绕过 FarmActor 写库，删除缓存避免下一次 Actor 从旧金币快照启动。
	// Redis 失效失败不改变已提交的领取结果，后续农场写入会再次回填缓存。
	_ = s.DeleteFarmCache(ctx, uid)
	mail.Claimed = true
	return mail, nil
}

// ClaimDailyLogin creates exactly one reward mail per logical day.
func (s *Store) ClaimDailyLogin(ctx context.Context, uid uint64, logicDay int64) (Mail, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mail{}, fmt.Errorf("store: begin claim daily login tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO daily_login (uid, logic_day, created_at)
		VALUES (?, ?, ?)`, uid, logicDay, now); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKeyErrNo {
			return Mail{}, ErrDailyLoginAlreadyClaimed
		}
		return Mail{}, fmt.Errorf("store: record daily login: %w", err)
	}
	mail, err := createMailTx(ctx, tx, uid, "每日登录奖励", dailyLoginRewardCoin, now)
	if err != nil {
		return Mail{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mail{}, fmt.Errorf("store: commit daily login: %w", err)
	}
	return mail, nil
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
