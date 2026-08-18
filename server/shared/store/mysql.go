package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"farm/server/domain/farm"
	"farm/server/shared/gameconfig"
)

// mysqlDuplicateKey 是 MySQL 唯一键冲突（account.username UNIQUE）的错误码。
const mysqlDuplicateKeyErrNo = 1062

// CreateAccount lets MySQL AUTO_INCREMENT allocate a uid shared by every
// Gateway, then creates the player and initial plots in the same transaction.
func (s *Store) CreateAccount(ctx context.Context, username, passwordHash string) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin create account tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx,
		`INSERT INTO account (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, passwordHash, now,
	)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKeyErrNo {
			return 0, ErrUsernameTaken
		}
		return 0, fmt.Errorf("store: insert account: %w", err)
	}
	insertID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: allocate account uid: %w", err)
	}
	if insertID <= 0 {
		return 0, errors.New("store: allocated account uid is not positive")
	}
	uid := uint64(insertID)
	if err := initializeFarmTx(ctx, tx, uid, username, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit create account tx: %w", err)
	}
	return uid, nil
}

// SaveAccount persists an explicitly supplied uid for integration fixtures and
// data import. Online registration uses CreateAccount.
func (s *Store) SaveAccount(ctx context.Context, uid uint64, username, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin save account tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account (uid, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		uid, username, passwordHash, now,
	); err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateKeyErrNo {
			return ErrUsernameTaken
		}
		return fmt.Errorf("store: insert account: %w", err)
	}
	if err := initializeFarmTx(ctx, tx, uid, username, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit save account tx: %w", err)
	}
	return nil
}

func initializeFarmTx(ctx context.Context, tx *sql.Tx, uid uint64, username string, now int64) error {
	agg := farm.NewAggregate(uid, username)
	dailyBlob, err := json.Marshal(agg.Daily)
	if err != nil {
		return fmt.Errorf("store: encode daily state: %w", err)
	}
	petBlob, err := json.Marshal(agg.Pet)
	if err != nil {
		return fmt.Errorf("store: encode pet state: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO player (
			uid, nickname, level, exp, coin, unlocked_plots,
			codex_bitmap, friend_ids, daily_blob, pet_blob,
			farm_seq, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agg.UID, agg.Nickname, agg.Level, agg.Exp, agg.Coin, agg.UnlockedPlots,
		make([]byte, 8), []byte{}, dailyBlob, petBlob,
		agg.FarmSeq, now, now,
	); err != nil {
		return fmt.Errorf("store: insert player: %w", err)
	}

	for i := 0; i < gameconfig.MaxPlots; i++ {
		blob, err := EncodePlot(agg.Plots[i])
		if err != nil {
			return fmt.Errorf("store: encode plot %d: %w", i, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO farm_plot (uid, plot_index, `blob`) VALUES (?, ?, ?)",
			uid, i, blob,
		); err != nil {
			return fmt.Errorf("store: insert farm_plot %d: %w", i, err)
		}
	}
	return nil
}

// GetAccountByUsername 供登录校验密码使用；未找到返回 ErrAccountNotFound。
func (s *Store) GetAccountByUsername(ctx context.Context, username string) (uint64, string, error) {
	var uid uint64
	var passwordHash string
	err := s.db.QueryRowContext(ctx,
		`SELECT uid, password_hash FROM account WHERE username = ?`, username,
	).Scan(&uid, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrAccountNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("store: query account by username: %w", err)
	}
	return uid, passwordHash, nil
}

// UpdatePasswordHash atomically replaces the hash only when it has not changed
// since authentication read it. This keeps opportunistic bcrypt migrations
// from overwriting a concurrent password update or another login's migration.
func (s *Store) UpdatePasswordHash(ctx context.Context, uid uint64, previousHash, passwordHash string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE account SET password_hash = ? WHERE uid = ? AND password_hash = ?`,
		passwordHash, uid, previousHash,
	)
	if err != nil {
		return false, fmt.Errorf("store: update account password hash: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: inspect password hash update: %w", err)
	}
	return rows == 1, nil
}

// loadFarmFromMySQL 读取 player + farm_plot 组装聚合；未找到返回 ErrFarmNotFound。
func (s *Store) loadFarmFromMySQL(ctx context.Context, uid uint64) (*farm.Aggregate, error) {
	agg := &farm.Aggregate{UID: uid}

	var codexBitmap, dailyBlob, petBlob, crossBlob, crossReceiptBlob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT nickname, level, exp, coin, unlocked_plots, codex_bitmap, daily_blob, pet_blob, cross_blob, cross_receipt_blob, farm_seq FROM player WHERE uid = ?`, uid,
	).Scan(&agg.Nickname, &agg.Level, &agg.Exp, &agg.Coin, &agg.UnlockedPlots, &codexBitmap, &dailyBlob, &petBlob, &crossBlob, &crossReceiptBlob, &agg.FarmSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFarmNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: query player: %w", err)
	}
	if len(dailyBlob) > 0 {
		if err := json.Unmarshal(dailyBlob, &agg.Daily); err != nil {
			return nil, fmt.Errorf("store: decode daily state: %w", err)
		}
	}
	if len(petBlob) > 0 {
		if err := json.Unmarshal(petBlob, &agg.Pet); err != nil {
			return nil, fmt.Errorf("store: decode pet state: %w", err)
		}
	}
	if len(crossBlob) > 0 {
		if err := json.Unmarshal(crossBlob, &agg.CrossPending); err != nil {
			return nil, fmt.Errorf("store: decode cross pending: %w", err)
		}
	}
	if len(crossReceiptBlob) > 0 {
		if err := json.Unmarshal(crossReceiptBlob, &agg.CrossReceipts); err != nil {
			return nil, fmt.Errorf("store: decode cross receipts: %w", err)
		}
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT plot_index, `blob` FROM farm_plot WHERE uid = ?", uid,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query farm_plot: %w", err)
	}
	defer rows.Close()

	var plotCount int
	seen := make([]bool, gameconfig.MaxPlots)
	for rows.Next() {
		var idx uint8
		var blob []byte
		if err := rows.Scan(&idx, &blob); err != nil {
			return nil, fmt.Errorf("store: scan farm_plot: %w", err)
		}
		if int(idx) >= gameconfig.MaxPlots {
			return nil, fmt.Errorf("store: farm_plot idx %d out of range [0,%d)", idx, gameconfig.MaxPlots)
		}
		if seen[idx] {
			return nil, fmt.Errorf("store: duplicate farm_plot idx %d for uid %d", idx, uid)
		}
		p, err := DecodePlot(blob)
		if err != nil {
			return nil, fmt.Errorf("store: decode farm_plot[%d]: %w", idx, err)
		}
		agg.Plots[idx] = p
		seen[idx] = true
		plotCount++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate farm_plot: %w", err)
	}
	if plotCount != gameconfig.MaxPlots {
		return nil, fmt.Errorf("store: farm_plot count %d want %d for uid %d", plotCount, gameconfig.MaxPlots, uid)
	}

	items, err := s.loadItemsFromMySQL(ctx, uid)
	if err != nil {
		return nil, err
	}
	agg.Items = items
	codex, err := loadCodexFromMySQL(ctx, s.db, uid, codexBitmap)
	if err != nil {
		return nil, err
	}
	agg.CodexHarvests = codex

	return agg, nil
}

// loadItemsFromMySQL 读取 item 表并映射为聚合 Items。表不存在行时返回空 map。
func (s *Store) loadItemsFromMySQL(ctx context.Context, uid uint64) (map[farm.ItemKey]uint32, error) {
	items := make(map[farm.ItemKey]uint32)
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, item_id, count FROM item WHERE uid = ?`, uid,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query item: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind uint8
		var itemID uint16
		var count uint32
		if err := rows.Scan(&kind, &itemID, &count); err != nil {
			return nil, fmt.Errorf("store: scan item: %w", err)
		}
		if count == 0 {
			continue
		}
		key, err := FormatItemKey(kind, itemID)
		if err != nil {
			return nil, fmt.Errorf("store: format item kind=%d id=%d: %w", kind, itemID, err)
		}
		items[key] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate item: %w", err)
	}
	return items, nil
}
