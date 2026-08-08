package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ListMailsEncoded returns the cached JSON array used by the Farm hot path.
// The encoded view shares the mailbox generation barrier with ListMails, so a
// committed write cannot leave a stale pre-encoded response behind.
func (s *Store) ListMailsEncoded(ctx context.Context, uid uint64) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if encoded, ok := s.mailbox.encoded.get(uid, time.Now()); ok {
			return append([]byte(nil), encoded...), nil
		}
		generation := s.mailboxGeneration(uid)
		mails, err := s.ListMails(ctx, uid)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(mails)
		if err != nil {
			return nil, fmt.Errorf("store: encode mail list: %w", err)
		}
		state := &s.mailbox.state[mailboxStateIndex(uid)]
		state.mu.Lock()
		if state.version == generation {
			s.mailbox.encoded.put(uid, append([]byte(nil), encoded...), time.Now())
			state.mu.Unlock()
			return encoded, nil
		}
		state.mu.Unlock()
	}
	// A mailbox receiving sustained writes is still readable. Bypass the
	// encoded cache after bounded retries instead of surfacing an internal
	// invalidation sentinel to the client.
	mails, err := s.ListMails(ctx, uid)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(mails)
	if err != nil {
		return nil, fmt.Errorf("store: encode mail list: %w", err)
	}
	return encoded, nil
}

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
			mails, err := s.listMailsFromMySQL(ctx, uid)
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
	now := time.Now().UnixMilli()
	var (
		result sql.Result
		err    error
	)
	if mailID == 0 {
		result, err = s.db.ExecContext(ctx,
			`UPDATE mail SET read_at = ? WHERE uid = ? AND read_at IS NULL`,
			now, uid,
		)
	} else {
		result, err = s.db.ExecContext(ctx,
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
	var (
		result sql.Result
		err    error
	)
	if mailID == 0 {
		result, err = s.db.ExecContext(ctx, `
			DELETE FROM mail
			WHERE uid = ?
			  AND (attachment_coin = 0 OR claimed_at IS NOT NULL)`,
			uid,
		)
	} else {
		result, err = s.db.ExecContext(ctx, `
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
	if _, err := tx.ExecContext(ctx, `UPDATE mail SET claimed_at = ?, read_at = COALESCE(read_at, ?) WHERE id = ?`, now, now, mail.ID); err != nil {
		return Mail{}, fmt.Errorf("store: mark mail claimed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Mail{}, fmt.Errorf("store: commit claim mail: %w", err)
	}
	s.invalidateMailboxAfterCommit(uid)
	// ClaimMail 由 Gateway/FarmRPC 放在 uid 权威 Actor 的串行段内调用，并同步
	// 更新在线聚合；删除缓存同时保护离线调用者不会加载旧金币快照。
	_ = s.DeleteFarmCache(ctx, uid)
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
