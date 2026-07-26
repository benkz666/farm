package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"farm/server/internal/farm"
)

func sessionKey(token string) string { return "session:" + token }
func farmKey(uid uint64) string      { return "farm:" + strconv.FormatUint(uid, 10) }

// Put 写入 session:{token} -> uid，TTL 由调用方指定（规格 5.2 节默认 7 天）。
func (s *Store) Put(ctx context.Context, token string, uid uint64, ttl time.Duration) error {
	if err := s.rdb.Set(ctx, sessionKey(token), strconv.FormatUint(uid, 10), ttl).Err(); err != nil {
		return fmt.Errorf("store: put session: %w", err)
	}
	return nil
}

// Get 读取 session:{token} 对应的 uid；不存在或已过期返回 ErrAccountNotFound。
func (s *Store) Get(ctx context.Context, token string) (uint64, error) {
	val, err := s.rdb.Get(ctx, sessionKey(token)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, ErrAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: get session: %w", err)
	}
	uid, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: parse session uid: %w", err)
	}
	return uid, nil
}

// Delete 删除 session:{token}（登出/踢下线）。
func (s *Store) Delete(ctx context.Context, token string) error {
	if err := s.rdb.Del(ctx, sessionKey(token)).Err(); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// LoadFarm 实现规格 5.4 节加载路径：先查 Redis `farm:{uid}` 缓存（JSON），
// miss 则回落 MySQL 并回填 Redis、续期 TTL。
func (s *Store) LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error) {
	cached, err := s.rdb.Get(ctx, farmKey(uid)).Bytes()
	if err == nil {
		var agg farm.Aggregate
		if jsonErr := json.Unmarshal(cached, &agg); jsonErr == nil {
			return &agg, nil
		}
		// 缓存内容损坏：忽略并回落 MySQL 重建缓存，而不是直接失败。
	} else if !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("store: get farm cache: %w", err)
	}

	agg, err := s.loadFarmFromMySQL(ctx, uid)
	if err != nil {
		return nil, err
	}

	if err := s.cacheFarm(ctx, agg); err != nil {
		return nil, err
	}
	return agg, nil
}

// SaveFarm 写 MySQL（持久权威）后同步重写 `farm:{uid}` 缓存（规格 5.3 节写路径）。
func (s *Store) SaveFarm(ctx context.Context, agg *farm.Aggregate) error {
	if err := s.saveFarmToMySQL(ctx, agg); err != nil {
		return err
	}
	return s.cacheFarm(ctx, agg)
}

// DeleteFarmCache 删除 farm:{uid} 缓存（不动 MySQL），供 Actor 卸载/测试模拟缓存丢失使用。
// 下一次 LoadFarm 会走 MySQL 回填路径。
func (s *Store) DeleteFarmCache(ctx context.Context, uid uint64) error {
	if err := s.rdb.Del(ctx, farmKey(uid)).Err(); err != nil {
		return fmt.Errorf("store: delete farm cache: %w", err)
	}
	return nil
}

// cacheFarm 以 JSON 写入 farm:{uid}，TTL 使用 Store.farmTTL（默认 10 分钟）。
func (s *Store) cacheFarm(ctx context.Context, agg *farm.Aggregate) error {
	payload, err := json.Marshal(agg)
	if err != nil {
		return fmt.Errorf("store: marshal farm cache: %w", err)
	}
	if err := s.rdb.Set(ctx, farmKey(agg.UID), payload, s.farmTTL).Err(); err != nil {
		return fmt.Errorf("store: set farm cache: %w", err)
	}
	return nil
}
