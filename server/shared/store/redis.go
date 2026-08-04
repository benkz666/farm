package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"farm/server/domain/farm"
)

func sessionKey(token string) string { return "session:" + token }
func activeSessionKey(uid uint64) string {
	return "session:active:" + strconv.FormatUint(uid, 10)
}
func farmKey(uid uint64) string { return "farm:" + strconv.FormatUint(uid, 10) }

const replacedSessionGrace = 5 * time.Minute

var replaceSessionScript = redis.NewScript(`
local previous = redis.call("GET", KEYS[1])
if previous and previous ~= ARGV[1] then
	local previousKey = ARGV[4] .. previous
	local previousTTL = redis.call("PTTL", previousKey)
	if previousTTL < 0 or previousTTL > tonumber(ARGV[3]) then
		redis.call("PEXPIRE", previousKey, ARGV[3])
	end
end
redis.call("SET", KEYS[2], ARGV[2], "PX", ARGV[5])
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[5])
return previous or ""
`)

// Put 原子轮换 uid 的当前 token。旧 token 短暂保留映射，使已在线连接能明确
// 区分“被新登录替换”(1105) 与自然过期(1102)，但不再具备鉴权能力。
func (s *Store) Put(ctx context.Context, token string, uid uint64, ttl time.Duration) error {
	if s == nil || s.rdb == nil || token == "" || uid == 0 || ttl <= 0 {
		return errors.New("store: invalid session")
	}
	if _, err := replaceSessionScript.Run(
		ctx,
		s.rdb,
		[]string{activeSessionKey(uid), sessionKey(token)},
		token,
		strconv.FormatUint(uid, 10),
		replacedSessionGrace.Milliseconds(),
		"session:",
		ttl.Milliseconds(),
	).Result(); err != nil {
		return fmt.Errorf("store: put session: %w", err)
	}
	return nil
}

// Get 读取 session:{token} 对应的 uid；不存在或已过期返回 ErrSessionNotFound
// （供 Auth 映射 1101/1102，勿与账号不存在的 ErrAccountNotFound 混用）。
func (s *Store) Get(ctx context.Context, token string) (uint64, error) {
	val, err := s.rdb.Get(ctx, sessionKey(token)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: get session: %w", err)
	}
	uid, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: parse session uid: %w", err)
	}
	active, err := s.rdb.Get(ctx, activeSessionKey(uid)).Result()
	if errors.Is(err, redis.Nil) {
		ttl, ttlErr := s.rdb.PTTL(ctx, sessionKey(token)).Result()
		if ttlErr != nil {
			return 0, fmt.Errorf("store: get session ttl: %w", ttlErr)
		}
		if ttl <= 0 {
			ttl = time.Minute
		}
		claimed, claimErr := s.rdb.SetNX(ctx, activeSessionKey(uid), token, ttl).Result()
		if claimErr != nil {
			return 0, fmt.Errorf("store: claim legacy session: %w", claimErr)
		}
		if claimed {
			return uid, nil
		}
		active, err = s.rdb.Get(ctx, activeSessionKey(uid)).Result()
	}
	if err != nil {
		return 0, fmt.Errorf("store: get active session: %w", err)
	}
	if active != token {
		return 0, ErrSessionReplaced
	}
	return uid, nil
}

var deleteSessionScript = redis.NewScript(`
local uid = redis.call("GET", KEYS[1])
redis.call("DEL", KEYS[1])
if uid then
	local activeKey = ARGV[2] .. uid
	if redis.call("GET", activeKey) == ARGV[1] then
		redis.call("DEL", activeKey)
	end
end
return 1
`)

// Delete 删除 token；仅当它仍是 uid 的当前 token 时才清理反向索引。
func (s *Store) Delete(ctx context.Context, token string) error {
	if _, err := deleteSessionScript.Run(
		ctx,
		s.rdb,
		[]string{sessionKey(token)},
		token,
		"session:active:",
	).Result(); err != nil {
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
			ensureItems(&agg)
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
	ensureItems(agg)

	if err := s.cacheFarm(ctx, agg); err != nil {
		return nil, err
	}
	return agg, nil
}

func ensureItems(agg *farm.Aggregate) {
	if agg.Items == nil {
		agg.Items = make(map[farm.ItemKey]uint32)
	}
	if agg.CodexHarvests == nil {
		agg.CodexHarvests = make(map[uint16]uint32)
	}
}

// SaveFarm 写 MySQL（持久权威）后 best-effort 重写 `farm:{uid}` 缓存（规格 5.3 节写路径）。
func (s *Store) SaveFarm(ctx context.Context, agg *farm.Aggregate) error {
	return s.SaveFarms(ctx, []*farm.Aggregate{agg})
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
