package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"farm/server/shared/gameconfig"
)

// FriendRequestRow 是收件箱中一条待处理申请。
type FriendRequestRow struct {
	FromUID   uint64 `json:"from_uid"`
	Nickname  string `json:"nickname"`
	CreatedAt int64  `json:"created_at"`
}

// CreateFriendRequest 发起申请。已是好友 / 已有同向待处理 / 自己 → 哨兵错误。
// 若对方已向自己发起申请，则直接建立好友并清理双方申请（互相申请视为同意）。
func (s *Store) CreateFriendRequest(ctx context.Context, fromUID, toUID uint64) error {
	if fromUID == 0 || toUID == 0 || fromUID == toUID {
		return ErrCannotFriendSelf
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin friend request tx: %w", err)
	}
	defer tx.Rollback()

	for _, uid := range []uint64{fromUID, toUID} {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM player WHERE uid = ?)`, uid,
		).Scan(&exists); err != nil {
			return fmt.Errorf("store: query player: %w", err)
		}
		if !exists {
			return ErrPlayerNotFound
		}
	}

	friends, err := areFriendsTx(ctx, tx, fromUID, toUID)
	if err != nil {
		return err
	}
	if friends {
		return ErrAlreadyFriend
	}

	var reverseExists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM friend_request WHERE from_uid = ? AND to_uid = ?)`,
		toUID, fromUID,
	).Scan(&reverseExists); err != nil {
		return fmt.Errorf("store: query reverse friend request: %w", err)
	}
	if reverseExists {
		if err := addFriendsInTx(ctx, tx, fromUID, toUID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM friend_request
			 WHERE (from_uid = ? AND to_uid = ?) OR (from_uid = ? AND to_uid = ?)`,
			fromUID, toUID, toUID, fromUID,
		); err != nil {
			return fmt.Errorf("store: clear friend requests after mutual: %w", err)
		}
		now := time.Now().UnixMilli()
		fromNick, err := playerNicknameTx(ctx, tx, fromUID)
		if err != nil {
			return err
		}
		toNick, err := playerNicknameTx(ctx, tx, toUID)
		if err != nil {
			return err
		}
		if _, err := createMailTx(ctx, tx, toUID, fmt.Sprintf("你与 %s 已成为邻里", fromNick), 0, now); err != nil {
			return err
		}
		if _, err := createMailTx(ctx, tx, fromUID, fmt.Sprintf("你与 %s 已成为邻里", toNick), 0, now); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit mutual friend request: %w", err)
		}
		s.invalidateMailboxAfterCommit(fromUID)
		s.invalidateMailboxAfterCommit(toUID)
		return nil
	}

	var pending bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM friend_request WHERE from_uid = ? AND to_uid = ?)`,
		fromUID, toUID,
	).Scan(&pending); err != nil {
		return fmt.Errorf("store: query pending friend request: %w", err)
	}
	if pending {
		return ErrFriendRequestPending
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO friend_request (from_uid, to_uid, created_at) VALUES (?, ?, ?)`,
		fromUID, toUID, now,
	); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKeyErrNo {
			return ErrFriendRequestPending
		}
		return fmt.Errorf("store: insert friend request: %w", err)
	}
	// 待处理申请由邮箱「邻里申请」待办展示；此处不写重复系统邮件，避免双红点
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit friend request: %w", err)
	}
	return nil
}

// ListIncomingFriendRequests 返回发给 uid 的待处理申请（含对方昵称）。
func (s *Store) ListIncomingFriendRequests(ctx context.Context, uid uint64) ([]FriendRequestRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.from_uid, p.nickname, r.created_at
		 FROM friend_request AS r
		 JOIN player AS p ON p.uid = r.from_uid
		 WHERE r.to_uid = ?
		 ORDER BY r.created_at ASC, r.id ASC`,
		uid,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list friend requests: %w", err)
	}
	defer rows.Close()

	out := make([]FriendRequestRow, 0)
	for rows.Next() {
		var row FriendRequestRow
		if err := rows.Scan(&row.FromUID, &row.Nickname, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan friend request: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate friend requests: %w", err)
	}
	return out, nil
}

// AcceptFriendRequest 收件人同意 fromUID 的申请：建好友并删除申请。
func (s *Store) AcceptFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	if toUID == 0 || fromUID == 0 || toUID == fromUID {
		return ErrCannotFriendSelf
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin accept friend request tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM friend_request WHERE from_uid = ? AND to_uid = ?`,
		fromUID, toUID,
	)
	if err != nil {
		return fmt.Errorf("store: delete friend request on accept: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected accept: %w", err)
	}
	if n == 0 {
		return ErrFriendRequestNotFound
	}

	if err := addFriendsInTx(ctx, tx, fromUID, toUID); err != nil {
		return err
	}
	// 顺带清掉反向待处理，避免残留
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM friend_request WHERE from_uid = ? AND to_uid = ?`,
		toUID, fromUID,
	); err != nil {
		return fmt.Errorf("store: clear reverse request: %w", err)
	}
	now := time.Now().UnixMilli()
	toNick, err := playerNicknameTx(ctx, tx, toUID)
	if err != nil {
		return err
	}
	if _, err := createMailTx(ctx, tx, fromUID, fmt.Sprintf("%s 同意了你的邻里申请", toNick), 0, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit accept friend request: %w", err)
	}
	s.invalidateMailboxAfterCommit(fromUID)
	return nil
}

// RejectFriendRequest 收件人拒绝申请。
func (s *Store) RejectFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	if toUID == 0 || fromUID == 0 {
		return ErrFriendRequestNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin reject friend request tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM friend_request WHERE from_uid = ? AND to_uid = ?`,
		fromUID, toUID,
	)
	if err != nil {
		return fmt.Errorf("store: reject friend request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected reject: %w", err)
	}
	if n == 0 {
		return ErrFriendRequestNotFound
	}
	now := time.Now().UnixMilli()
	toNick, err := playerNicknameTx(ctx, tx, toUID)
	if err != nil {
		return err
	}
	if _, err := createMailTx(ctx, tx, fromUID, fmt.Sprintf("%s 拒绝了你的邻里申请", toNick), 0, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reject friend request: %w", err)
	}
	s.invalidateMailboxAfterCommit(fromUID)
	return nil
}

func playerNicknameTx(ctx context.Context, tx *sql.Tx, uid uint64) (string, error) {
	var nickname string
	err := tx.QueryRowContext(ctx, `SELECT nickname FROM player WHERE uid = ?`, uid).Scan(&nickname)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPlayerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: query nickname: %w", err)
	}
	if nickname == "" {
		return fmt.Sprintf("UID %d", uid), nil
	}
	return nickname, nil
}

func areFriendsTx(ctx context.Context, tx *sql.Tx, a, b uint64) (bool, error) {
	uidLo, uidHi := friendshipPair(a, b)
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM friendship WHERE uid_lo = ? AND uid_hi = ?)`,
		uidLo, uidHi,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: query friendship in tx: %w", err)
	}
	return exists, nil
}

// addFriendsInTx 在已有事务内建立好友（调用方负责 Commit）。已是好友视为成功幂等。
func addFriendsInTx(ctx context.Context, tx *sql.Tx, a, b uint64) error {
	uidLo, uidHi := friendshipPair(a, b)
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM friendship WHERE uid_lo = ? AND uid_hi = ?)`,
		uidLo, uidHi,
	).Scan(&exists); err != nil {
		return fmt.Errorf("store: query friendship before insert: %w", err)
	}
	if exists {
		return nil
	}

	count, err := countFriendsTx(ctx, tx, a)
	if err != nil {
		return err
	}
	if count >= gameconfig.FriendLimit {
		return ErrFriendLimitSelf
	}
	count, err = countFriendsTx(ctx, tx, b)
	if err != nil {
		return err
	}
	if count >= gameconfig.FriendLimit {
		return ErrFriendLimitPeer
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO friendship (uid_lo, uid_hi, created_at) VALUES (?, ?, ?)`,
		uidLo, uidHi, time.Now().UnixMilli(),
	); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKeyErrNo {
			return nil
		}
		return fmt.Errorf("store: insert friendship in tx: %w", err)
	}
	return nil
}
