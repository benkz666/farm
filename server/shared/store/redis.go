package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
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

// getSessionScript validates both token -> uid and uid -> active token in one
// Redis round trip. It also atomically claims the active index for sessions
// created before that reverse index existed.
var getSessionScript = redis.NewScript(`
local uid = redis.call("GET", KEYS[1])
if not uid then
	return {0}
end

local activeKey = ARGV[1] .. uid
local active = redis.call("GET", activeKey)
if not active then
	local ttl = redis.call("PTTL", KEYS[1])
	if ttl <= 0 then
		ttl = tonumber(ARGV[2])
	end
	if redis.call("SET", activeKey, ARGV[3], "PX", ttl, "NX") then
		active = ARGV[3]
	else
		active = redis.call("GET", activeKey)
	end
end

if active ~= ARGV[3] then
	return {2}
end
return {1, uid}
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
	result, err := getSessionScript.Run(
		ctx,
		s.rdb,
		[]string{sessionKey(token)},
		"session:active:",
		time.Minute.Milliseconds(),
		token,
	).Slice()
	if err != nil {
		return 0, fmt.Errorf("store: validate session: %w", err)
	}
	if len(result) == 0 {
		return 0, errors.New("store: empty session validation result")
	}
	status, err := sessionValidationStatus(result[0])
	if err != nil {
		return 0, err
	}
	switch status {
	case 0:
		return 0, ErrSessionNotFound
	case 2:
		return 0, ErrSessionReplaced
	case 1:
		if len(result) != 2 {
			return 0, errors.New("store: missing session uid")
		}
	default:
		return 0, fmt.Errorf("store: unknown session validation status %d", status)
	}
	uid, err := strconv.ParseUint(fmt.Sprint(result[1]), 10, 64)
	if err != nil || uid == 0 {
		return 0, fmt.Errorf("store: parse session uid %q", fmt.Sprint(result[1]))
	}
	return uid, nil
}

func sessionValidationStatus(value any) (int64, error) {
	switch status := value.(type) {
	case int64:
		return status, nil
	case string:
		parsed, err := strconv.ParseInt(status, 10, 64)
		if err == nil {
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("store: invalid session validation status %q", fmt.Sprint(value))
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

// LoadFarm 实现规格 5.4 节加载路径：先查 Redis `farm:{uid}` 缓存，
// miss 则回落 MySQL 并回填 Redis、续期 TTL。
func (s *Store) LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error) {
	cached, err := s.rdb.Get(ctx, farmKey(uid)).Bytes()
	if err == nil {
		if agg, decodeErr := decodeFarmCache(cached); decodeErr == nil && agg.UID == uid {
			return agg, nil
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

// LoadFarms folds concurrent cold-Actor cache lookups into one Redis MGET. A
// corrupt or absent value falls back to MySQL with bounded concurrency, then a
// single pipeline refills every miss. Results are keyed by UID so callers can
// preserve their own request order without copying aggregates again.
func (s *Store) LoadFarms(ctx context.Context, uids []uint64) (map[uint64]*farm.Aggregate, error) {
	results := make(map[uint64]*farm.Aggregate, len(uids))
	unique := make([]uint64, 0, len(uids))
	keys := make([]string, 0, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return nil, errors.New("store: invalid farm UID")
		}
		if _, exists := results[uid]; exists {
			continue
		}
		// A nil marker records de-duplication until the cache value is decoded.
		results[uid] = nil
		unique = append(unique, uid)
		keys = append(keys, farmKey(uid))
	}
	if len(unique) == 0 {
		return results, nil
	}

	values, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("store: mget farm caches: %w", err)
	}
	misses := make([]uint64, 0, len(unique))
	for index, uid := range unique {
		value, ok := values[index].(string)
		if !ok {
			misses = append(misses, uid)
			continue
		}
		agg, decodeErr := decodeFarmCache([]byte(value))
		if decodeErr != nil || agg.UID != uid {
			misses = append(misses, uid)
			continue
		}
		results[uid] = agg
	}
	if len(misses) == 0 {
		return results, nil
	}

	const mysqlLoadConcurrency = 16
	workers := min(mysqlLoadConcurrency, len(misses))
	jobs := make(chan uint64)
	loaded := make(chan *farm.Aggregate, len(misses))
	errCh := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for uid := range jobs {
				agg, loadErr := s.loadFarmFromMySQL(ctx, uid)
				if loadErr != nil {
					select {
					case errCh <- loadErr:
					default:
					}
					continue
				}
				ensureItems(agg)
				loaded <- agg
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, uid := range misses {
			select {
			case jobs <- uid:
			case <-ctx.Done():
				return
			}
		}
	}()
	wait.Wait()
	close(loaded)
	select {
	case loadErr := <-errCh:
		return nil, loadErr
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refills := make([]*farm.Aggregate, 0, len(misses))
	for agg := range loaded {
		results[agg.UID] = agg
		refills = append(refills, agg)
	}
	if len(refills) != len(misses) {
		return nil, errors.New("store: incomplete farm cache refill")
	}
	if err := s.cacheFarmsPipeline(ctx, refills); err != nil {
		return nil, err
	}
	return results, nil
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

// cacheFarm 以版本化 Protobuf 写入 farm:{uid}，TTL 使用 Store.farmTTL。
func (s *Store) cacheFarm(ctx context.Context, agg *farm.Aggregate) error {
	payload, err := encodeFarmCache(agg)
	if err != nil {
		return err
	}
	if err := s.rdb.Set(ctx, farmKey(agg.UID), payload, s.farmTTL).Err(); err != nil {
		return fmt.Errorf("store: set farm cache: %w", err)
	}
	return nil
}
