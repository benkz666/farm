package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
)

// mysqlDuplicateKey 是 MySQL 唯一键冲突（account.username UNIQUE）的错误码。
const mysqlDuplicateKeyErrNo = 1062

// SaveAccount 在一个事务内写入 account + player + MaxPlots 行 farm_plot，
// 玩家初始数值取自 farm.NewAggregate（策划 4.2 节）。uid 由调用方（Auth）生成。
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

	agg := farm.NewAggregate(uid, username)
	dailyBlob, err := json.Marshal(agg.Daily)
	if err != nil {
		return fmt.Errorf("store: encode daily state: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO player (
			uid, nickname, level, exp, coin, unlocked_plots,
			codex_bitmap, friend_ids, daily_blob, pet_blob,
			farm_seq, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agg.UID, agg.Nickname, agg.Level, agg.Exp, agg.Coin, agg.UnlockedPlots,
		make([]byte, 8), []byte{}, dailyBlob, []byte{},
		agg.FarmSeq, now, now,
	); err != nil {
		return fmt.Errorf("store: insert player: %w", err)
	}

	for i := 0; i < gameconf.MaxPlots; i++ {
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

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit save account tx: %w", err)
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

// loadFarmFromMySQL 读取 player + farm_plot 组装聚合；未找到返回 ErrFarmNotFound。
func (s *Store) loadFarmFromMySQL(ctx context.Context, uid uint64) (*farm.Aggregate, error) {
	agg := &farm.Aggregate{UID: uid}

	var dailyBlob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT nickname, level, exp, coin, unlocked_plots, daily_blob, farm_seq FROM player WHERE uid = ?`, uid,
	).Scan(&agg.Nickname, &agg.Level, &agg.Exp, &agg.Coin, &agg.UnlockedPlots, &dailyBlob, &agg.FarmSeq)
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

	rows, err := s.db.QueryContext(ctx,
		"SELECT plot_index, `blob` FROM farm_plot WHERE uid = ?", uid,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query farm_plot: %w", err)
	}
	defer rows.Close()

	var plotCount int
	seen := make([]bool, gameconf.MaxPlots)
	for rows.Next() {
		var idx uint8
		var blob []byte
		if err := rows.Scan(&idx, &blob); err != nil {
			return nil, fmt.Errorf("store: scan farm_plot: %w", err)
		}
		if int(idx) >= gameconf.MaxPlots {
			return nil, fmt.Errorf("store: farm_plot idx %d out of range [0,%d)", idx, gameconf.MaxPlots)
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
	if plotCount != gameconf.MaxPlots {
		return nil, fmt.Errorf("store: farm_plot count %d want %d for uid %d", plotCount, gameconf.MaxPlots, uid)
	}

	items, err := s.loadItemsFromMySQL(ctx, uid)
	if err != nil {
		return nil, err
	}
	agg.Items = items

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

// saveFarmToMySQL 更新 player 行与全部 MaxPlots 行 farm_plot（regard 期 1 无脏位跟踪，整块覆盖）。
func (s *Store) saveFarmToMySQL(ctx context.Context, agg *farm.Aggregate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin save farm tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	dailyBlob, err := json.Marshal(agg.Daily)
	if err != nil {
		return fmt.Errorf("store: encode daily state: %w", err)
	}
	// 不做 RowsAffected==0 → ErrFarmNotFound：MySQL 默认 RowsAffected 只计「实际改动的行」，
	// 无脏写时会误判；注册路径已保证 player 行存在，缺行由后续业务/读路径暴露。
	if _, err := tx.ExecContext(ctx,
		`UPDATE player SET nickname = ?, level = ?, exp = ?, coin = ?, unlocked_plots = ?, daily_blob = ?, farm_seq = ?, updated_at = ? WHERE uid = ?`,
		agg.Nickname, agg.Level, agg.Exp, agg.Coin, agg.UnlockedPlots, dailyBlob, agg.FarmSeq, now, agg.UID,
	); err != nil {
		return fmt.Errorf("store: update player: %w", err)
	}

	for i := 0; i < gameconf.MaxPlots; i++ {
		blob, err := EncodePlot(agg.Plots[i])
		if err != nil {
			return fmt.Errorf("store: encode plot %d: %w", i, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO farm_plot (uid, plot_index, `blob`) VALUES (?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE `blob` = VALUES(`blob`)",
			agg.UID, i, blob,
		); err != nil {
			return fmt.Errorf("store: upsert farm_plot %d: %w", i, err)
		}
	}

	if err := replaceItemsTx(ctx, tx, agg); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit save farm tx: %w", err)
	}
	return nil
}

// replaceItemsTx 用聚合 Items 全量替换该 uid 的 item 行（期 2 无脏位，简单可靠）。
func replaceItemsTx(ctx context.Context, tx *sql.Tx, agg *farm.Aggregate) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM item WHERE uid = ?`, agg.UID); err != nil {
		return fmt.Errorf("store: delete item: %w", err)
	}
	for key, count := range agg.Items {
		if count == 0 {
			continue
		}
		kind, itemID, err := ParseItemKey(key)
		if err != nil {
			return fmt.Errorf("store: parse item key %q: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO item (uid, kind, item_id, count) VALUES (?, ?, ?, ?)`,
			agg.UID, kind, itemID, count,
		); err != nil {
			return fmt.Errorf("store: insert item %q: %w", key, err)
		}
	}
	return nil
}
