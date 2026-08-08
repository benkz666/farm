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

// FriendRow 是好友列表中的玩家标识与昵称。
type FriendRow struct {
	UID      uint64
	Nickname string
}

// FindUserByUsername 精确查询可添加好友的玩家公开资料。
func (s *Store) FindUserByUsername(ctx context.Context, username string) (UserSearchRow, error) {
	var user UserSearchRow
	err := s.db.QueryRowContext(ctx,
		`SELECT a.uid, p.nickname
		FROM account AS a
		JOIN player AS p ON p.uid = a.uid
		WHERE a.username = ?`,
		username,
	).Scan(&user.UID, &user.Nickname)
	if errors.Is(err, sql.ErrNoRows) {
		return UserSearchRow{}, ErrAccountNotFound
	}
	if err != nil {
		return UserSearchRow{}, fmt.Errorf("store: find user by username: %w", err)
	}
	return user, nil
}

// AreFriends 返回两个 uid 是否已有一条双向好友关系。
func (s *Store) AreFriends(ctx context.Context, a, b uint64) (bool, error) {
	uidLo, uidHi := friendshipPair(a, b)

	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM friendship WHERE uid_lo = ? AND uid_hi = ?)`,
		uidLo, uidHi,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: query friendship: %w", err)
	}
	return exists, nil
}

// AddFriends 建立一条规范化的双向好友关系。已有关系返回 ErrAlreadyFriend。
func (s *Store) AddFriends(ctx context.Context, a, b uint64) error {
	uidLo, uidHi := friendshipPair(a, b)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin add friendship tx: %w", err)
	}
	defer tx.Rollback()

	for _, uid := range []uint64{a, b} {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM player WHERE uid = ?)`,
			uid,
		).Scan(&exists); err != nil {
			return fmt.Errorf("store: query player: %w", err)
		}
		if !exists {
			return ErrPlayerNotFound
		}
	}

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM friendship WHERE uid_lo = ? AND uid_hi = ?)`,
		uidLo, uidHi,
	).Scan(&exists); err != nil {
		return fmt.Errorf("store: query existing friendship: %w", err)
	}
	if exists {
		return ErrAlreadyFriend
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
			return ErrAlreadyFriend
		}
		return fmt.Errorf("store: insert friendship: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit add friendship: %w", err)
	}
	return nil
}

// RemoveFriends 删除一条双向好友关系；不存在时同样成功。
func (s *Store) RemoveFriends(ctx context.Context, a, b uint64) error {
	uidLo, uidHi := friendshipPair(a, b)
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM friendship WHERE uid_lo = ? AND uid_hi = ?`,
		uidLo, uidHi,
	); err != nil {
		return fmt.Errorf("store: remove friendship: %w", err)
	}
	return nil
}

// ListFriends 返回 uid 的所有好友及其当前昵称。
func (s *Store) ListFriends(ctx context.Context, uid uint64) ([]FriendRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.uid, p.nickname
		FROM (
			SELECT uid_hi AS friend_uid, created_at
			FROM friendship
			WHERE uid_lo = ?
			UNION ALL
			SELECT uid_lo AS friend_uid, created_at
			FROM friendship
			WHERE uid_hi = ?
		) AS f
		JOIN player AS p ON p.uid = f.friend_uid
		ORDER BY f.created_at ASC, p.uid ASC`,
		uid, uid,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list friends: %w", err)
	}
	defer rows.Close()

	var friends []FriendRow
	for rows.Next() {
		var friend FriendRow
		if err := rows.Scan(&friend.UID, &friend.Nickname); err != nil {
			return nil, fmt.Errorf("store: scan friend: %w", err)
		}
		friends = append(friends, friend)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate friends: %w", err)
	}
	return friends, nil
}

// CountFriends 返回 uid 当前的好友数。
func (s *Store) CountFriends(ctx context.Context, uid uint64) (int, error) {
	return countFriends(ctx, s.db, uid)
}

func countFriendsTx(ctx context.Context, tx *sql.Tx, uid uint64) (int, error) {
	return countFriends(ctx, tx, uid)
}

type friendshipCounter interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func countFriends(ctx context.Context, queryer friendshipCounter, uid uint64) (int, error) {
	var count int
	if err := queryer.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM friendship WHERE uid_lo = ? OR uid_hi = ?`,
		uid, uid,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count friends: %w", err)
	}
	return count, nil
}

func friendshipPair(a, b uint64) (uint64, uint64) {
	if a < b {
		return a, b
	}
	return b, a
}
