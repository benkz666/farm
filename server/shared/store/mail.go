package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ListMails 返回玩家的个人邮件，附件与已读状态分别由 Claimed / Read 明示。
func (s *Store) ListMails(ctx context.Context, uid uint64) ([]Mail, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if mails, ok := s.mailbox.local.get(uid, time.Now()); ok {
			return cloneMails(mails), nil
		}
		mails, err := s.coalesceMailbox(ctx, uid, func() ([]Mail, error) {
			if cached, ok := s.mailbox.local.get(uid, time.Now()); ok {
				return cached, nil
			}
			cached, hit, version, cacheErr := s.loadMailboxCache(ctx, uid)
			if hit {
				return cached, nil
			}
			mails, err := s.listMailsFromMySQLBatched(ctx, uid)
			if err != nil {
				return nil, err
			}
			// Redis is an acceleration layer. A cache read/write failure must not
			// turn an authoritative MySQL success into a MailList failure.
			if cacheErr == nil {
				_ = s.writeMailboxCache(ctx, uid, version, mails)
			}
			return mails, nil
		})
		if errors.Is(err, errMailboxInvalidated) {
			continue
		}
		return cloneMails(mails), err
	}
	return s.listMailsFromMySQL(ctx, uid)
}

func (s *Store) listMailsFromMySQL(ctx context.Context, uid uint64) ([]Mail, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, attachment_coin, claimed_at IS NOT NULL, read_at IS NOT NULL, created_at
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
		if err := rows.Scan(&mail.ID, &mail.Title, &mail.AttachmentCoin, &mail.Claimed, &mail.Read, &mail.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan mail: %w", err)
		}
		mails = append(mails, mail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate mails: %w", err)
	}
	return cloneMails(mails), nil
}

// MarkMailsRead 持久化玩家的阅读进度。mailID=0 时批量处理当前收件箱；
// UPDATE 只触碰尚未阅读的行，因此重复打开邮箱是幂等的。
func (s *Store) MarkMailsRead(ctx context.Context, uid uint64, mailID uint64) (int64, error) {
	return s.markMailsRead(ctx, uid, mailID, s.db)
}

func (s *Store) markMailsRead(
	ctx context.Context,
	uid uint64,
	mailID uint64,
	exec sqlContextExecer,
) (int64, error) {
	now := time.Now().UnixMilli()
	var (
		result sql.Result
		err    error
	)
	if mailID == 0 {
		result, err = exec.ExecContext(ctx,
			`UPDATE mail SET read_at = ? WHERE uid = ? AND read_at IS NULL`,
			now, uid,
		)
	} else {
		result, err = exec.ExecContext(ctx,
			`UPDATE mail SET read_at = ? WHERE uid = ? AND id = ? AND read_at IS NULL`,
			now, uid, mailID,
		)
	}
	if err != nil {
		return 0, fmt.Errorf("store: mark mails read: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count marked mails: %w", err)
	}
	if affected > 0 {
		s.invalidateMailboxAfterCommit(uid)
	}
	return affected, nil
}

// DeleteMails 删除玩家主动清理的邮件。未领取附件属于玩家资产，单封与批量
// 清理都必须保留；uid 始终进入 WHERE，禁止跨玩家清理。
func (s *Store) DeleteMails(ctx context.Context, uid uint64, mailID uint64) (int64, error) {
	return s.deleteMails(ctx, uid, mailID, s.db)
}

func (s *Store) deleteMails(
	ctx context.Context,
	uid uint64,
	mailID uint64,
	exec sqlContextExecer,
) (int64, error) {
	var (
		result sql.Result
		err    error
	)
	if mailID == 0 {
		result, err = exec.ExecContext(ctx, `
			DELETE FROM mail
			WHERE uid = ?
			  AND (attachment_coin = 0 OR claimed_at IS NOT NULL)`,
			uid,
		)
	} else {
		result, err = exec.ExecContext(ctx, `
			DELETE FROM mail
			WHERE uid = ? AND id = ?
			  AND (attachment_coin = 0 OR claimed_at IS NOT NULL)`,
			uid, mailID,
		)
	}
	if err != nil {
		return 0, fmt.Errorf("store: delete mails: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count deleted mails: %w", err)
	}
	if affected > 0 {
		s.invalidateMailboxAfterCommit(uid)
	}
	return affected, nil
}

// ClaimMail 原子标记附件、增加金币，避免重复领取资损。
func (s *Store) ClaimMail(ctx context.Context, uid uint64, mailID uint64) (Mail, error) {
	return s.claimMail(ctx, uid, mailID, nil)
}

// ClaimMailAtState claims an attachment while persisting the Actor's absolute
// post-reward coin value at the next Farm sequence. This removes the need to
// synchronously project unrelated pending farm records before a mail claim.
func (s *Store) ClaimMailAtState(
	ctx context.Context,
	uid uint64,
	mailID uint64,
	state DirectClaimState,
) (Mail, error) {
	if state.NextFarmSeq == 0 {
		return Mail{}, errors.New("store: direct mail claim has invalid next farm sequence")
	}
	return s.claimMail(ctx, uid, mailID, &state)
}

func (s *Store) claimMail(ctx context.Context, uid uint64, mailID uint64, state *DirectClaimState) (Mail, error) {
	return s.claimMailWithExecer(ctx, uid, mailID, state, s.db)
}

func (s *Store) claimMailWithExecer(
	ctx context.Context,
	uid uint64,
	mailID uint64,
	state *DirectClaimState,
	exec sqlContextExecer,
) (Mail, error) {
	var mail Mail
	var claimedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, title, attachment_coin, claimed_at, created_at
		FROM mail
		WHERE id = ? AND uid = ?`, mailID, uid).Scan(
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
	// Credit and claim in one joined UPDATE. The initial non-locking read only
	// builds the response; claimed_at in the WHERE clause is the atomic
	// exactly-once boundary when another process races this request.
	var result sql.Result
	if state == nil {
		result, err = exec.ExecContext(ctx, `
			UPDATE player AS p
			JOIN mail AS m ON m.uid = p.uid AND m.id = ?
			SET p.coin = p.coin + m.attachment_coin, p.updated_at = ?,
				m.claimed_at = ?, m.read_at = COALESCE(m.read_at, ?)
			WHERE p.uid = ? AND m.attachment_coin > 0 AND m.claimed_at IS NULL`,
			mailID, now, now, now, uid,
		)
	} else {
		result, err = exec.ExecContext(ctx, `
			UPDATE player AS p
			JOIN mail AS m ON m.uid = p.uid AND m.id = ?
			SET p.coin = ? + m.attachment_coin, p.farm_seq = ?, p.updated_at = ?,
				m.claimed_at = ?, m.read_at = COALESCE(m.read_at, ?)
			WHERE p.uid = ? AND m.attachment_coin > 0 AND m.claimed_at IS NULL`,
			mailID, state.Coin, state.NextFarmSeq, now, now, now, uid,
		)
	}
	if err != nil {
		return Mail{}, fmt.Errorf("store: atomically claim mail: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Mail{}, fmt.Errorf("store: inspect claimed mail: %w", err)
	}
	if affected == 0 {
		return Mail{}, ErrMailAlreadyClaimed
	}
	s.invalidateMailAndFarmAfterClaim(uid)
	mail.Claimed = true
	mail.Read = true
	return mail, nil
}

// ClaimDailyLogin keeps command 614 compatible by delegating it to the same
// task 4 state used by ordinary ClaimTask requests.
func (s *Store) ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (TaskReward, error) {
	return s.ClaimTask(ctx, uid, dayKey, TaskDailyLoginID)
}

func createMailTx(ctx context.Context, tx *sql.Tx, uid uint64, title string, attachmentCoin, now int64) (Mail, error) {
	// 调用方必须只在事务提交成功后调用 invalidateMailboxAfterCommit(uid)。
	// 事务内提前失效会让并发 MailList 回源并缓存尚未提交的旧邮箱。
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
