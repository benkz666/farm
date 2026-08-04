package store

import (
	"context"
	"fmt"
	"strconv"
)

// StealHintStore 维护「农场是否有可偷成熟作物」的弱一致摘要（Redis）。
type StealHintStore interface {
	SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error
	GetStealHints(ctx context.Context, uids []uint64) (map[uint64]bool, error)
}

func stealHintKey(uid uint64) string {
	return "steal_hint:" + strconv.FormatUint(uid, 10)
}

// SetStealHint 写入或清除可偷摘要。hasStealable=false 时删除键，避免 FriendList 读到陈旧 true。
func (s *Store) SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("store: steal hint redis unavailable")
	}
	key := stealHintKey(uid)
	if !hasStealable {
		if err := s.rdb.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("store: delete steal hint: %w", err)
		}
		return nil
	}
	if err := s.rdb.Set(ctx, key, "1", 0).Err(); err != nil {
		return fmt.Errorf("store: set steal hint: %w", err)
	}
	return nil
}

// GetStealHints 批量读取可偷摘要；缺失键视为 false，不出现在返回 map 中亦可（调用方按 false 处理）。
func (s *Store) GetStealHints(ctx context.Context, uids []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(uids))
	if s == nil || s.rdb == nil || len(uids) == 0 {
		return out, nil
	}
	keys := make([]string, len(uids))
	for i, uid := range uids {
		keys[i] = stealHintKey(uid)
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("store: mget steal hints: %w", err)
	}
	for i, val := range vals {
		if val == nil {
			continue
		}
		switch v := val.(type) {
		case string:
			if v == "1" || v == "true" {
				out[uids[i]] = true
			}
		}
	}
	return out, nil
}
