package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/gameconfig"
)

// CodexRewardStore creates idempotent reward mails for newly reached plaque
// milestones. Calling it repeatedly with the same or larger count is safe.
type CodexRewardStore interface {
	IssueCodexRewards(ctx context.Context, uid uint64, progress farm.CodexProgress) ([]farm.CodexRewardNotice, error)
}

// IssueCodexRewards inserts every eligible, not-yet-created milestone mail.
func (s *Store) IssueCodexRewards(ctx context.Context, uid uint64, progress farm.CodexProgress) ([]farm.CodexRewardNotice, error) {
	crop, ok := gameconfig.CropByID(progress.CropID)
	if s == nil || s.db == nil || uid == 0 || !ok || progress.HarvestCount == 0 {
		return nil, fmt.Errorf("store: invalid codex reward request")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin codex reward: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	issued := make([]farm.CodexRewardNotice, 0, len(gameconfig.CodexTiers))
	for _, milestone := range gameconfig.CodexTiers {
		if progress.HarvestCount < milestone.HarvestCount {
			continue
		}
		sourceKey := "codex:" + strconv.FormatUint(uint64(progress.CropID), 10) + ":" + milestone.Tier
		title := "图鉴里程碑：" + crop.Name + "·" + codexTierLabel(milestone.Tier)
		result, insertErr := tx.ExecContext(ctx, `
			INSERT INTO mail (uid, source_key, title, attachment_coin, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE source_key = VALUES(source_key)`,
			uid, sourceKey, title, milestone.RewardCoin, now,
		)
		if insertErr != nil {
			return nil, fmt.Errorf("store: insert codex reward mail: %w", insertErr)
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return nil, fmt.Errorf("store: count codex reward mail: %w", affectedErr)
		}
		if affected == 1 {
			issued = append(issued, farm.CodexRewardNotice{
				CropID:     progress.CropID,
				Tier:       milestone.Tier,
				Target:     milestone.HarvestCount,
				RewardCoin: milestone.RewardCoin,
			})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit codex reward: %w", err)
	}
	if len(issued) > 0 {
		s.invalidateMailboxAfterCommit(uid)
	}
	return issued, nil
}

func codexTierLabel(tier string) string {
	switch tier {
	case "bronze":
		return "铜牌"
	case "silver":
		return "银牌"
	case "gold":
		return "金牌"
	default:
		return "收藏牌"
	}
}

func loadCodexFromMySQL(ctx context.Context, db *sql.DB, uid uint64, legacyBitmap []byte) (map[uint16]uint32, error) {
	counts := make(map[uint16]uint32)
	rows, err := db.QueryContext(ctx,
		`SELECT crop_id, harvest_count FROM player_codex WHERE uid = ?`,
		uid,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query player_codex: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cropID uint16
		var count uint32
		if err := rows.Scan(&cropID, &count); err != nil {
			return nil, fmt.Errorf("store: scan player_codex: %w", err)
		}
		if _, ok := gameconfig.CropByID(cropID); ok && count > 0 {
			counts[cropID] = count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate player_codex: %w", err)
	}
	// Preserve any legacy unlock bits by treating them as one historical harvest.
	for cropID := uint16(1); cropID <= 64; cropID++ {
		bit := cropID - 1
		if int(bit/8) < len(legacyBitmap) && legacyBitmap[bit/8]&(1<<(bit%8)) != 0 && counts[cropID] == 0 {
			if _, ok := gameconfig.CropByID(cropID); ok {
				counts[cropID] = 1
			}
		}
	}
	return counts, nil
}

func encodeCodexBitmap(counts map[uint16]uint32) []byte {
	bitmap := make([]byte, 8)
	for cropID, count := range counts {
		if cropID == 0 || cropID > 64 || count == 0 {
			continue
		}
		bit := cropID - 1
		bitmap[bit/8] |= 1 << (bit % 8)
	}
	return bitmap
}
