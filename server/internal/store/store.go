// Package store 是 farm/player/session 持久化的唯一入口。
//
// 上层（auth、actor）只依赖本包的 SessionStore / FarmStore 接口，不直接持有
// *sql.DB 或 *redis.Client（规格 2026-07-26-engineering-standards.md 4 节）。
//
// 具体实现 Store 组合 MySQL（durable）与 Redis（session + farm 缓存）：
//   - mysql.go：account/player/farm_plot 的读写（Store 的 MySQL-only 方法）
//   - redis.go：session:{token} 与 farm:{uid} 缓存（Store 的 Redis-only 方法 +
//     LoadFarm/SaveFarm 的回填编排）
//   - farm_codec.go：farm_plot.blob 列的单一编解码函数
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"farm/server/internal/farm"
)

// SessionStore 管理 token -> uid 的会话映射（Redis `session:{token}`）。
type SessionStore interface {
	Put(ctx context.Context, token string, uid uint64, ttl time.Duration) error
	Get(ctx context.Context, token string) (uint64, error)
	Delete(ctx context.Context, token string) error
}

// FarmStore 管理账号与农场聚合的持久化（MySQL）与缓存（Redis `farm:{uid}`）。
type FarmStore interface {
	// SaveAccount 在一个事务内写入 account + player + MaxPlots 行 farm_plot
	// （规格 5.1 节「注册事务」），uid 由调用方（Auth）生成。
	SaveAccount(ctx context.Context, uid uint64, username, passwordHash string) error
	// GetAccountByUsername 供登录校验密码使用。
	GetAccountByUsername(ctx context.Context, username string) (uid uint64, passwordHash string, err error)

	LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error)
	SaveFarm(ctx context.Context, agg *farm.Aggregate) error
}

// 哨兵错误，供上层用 errors.Is 判定并映射到 pkgerr 协议码。
var (
	ErrUsernameTaken   = errors.New("store: username already taken")
	ErrAccountNotFound = errors.New("store: account not found")
	ErrSessionNotFound = errors.New("store: session not found")
	ErrFarmNotFound    = errors.New("store: farm not found")
)

const (
	// DefaultFarmCacheTTL 是 farm:{uid} 缓存的默认 TTL（规格 5.2 节：10 分钟，与 Actor 空闲超时一致）。
	DefaultFarmCacheTTL = 10 * time.Minute
)

// Store 是 SessionStore 与 FarmStore 的唯一实现，组合 MySQL 与 Redis。
type Store struct {
	db      *sql.DB
	rdb     *redis.Client
	farmTTL time.Duration
}

// New 用已建立的 *sql.DB / *redis.Client 组装 Store，farmTTL<=0 时用 DefaultFarmCacheTTL。
func New(db *sql.DB, rdb *redis.Client, farmTTL time.Duration) *Store {
	if farmTTL <= 0 {
		farmTTL = DefaultFarmCacheTTL
	}
	return &Store{db: db, rdb: rdb, farmTTL: farmTTL}
}

// Open 按 DSN / addr 建立 MySQL 与 Redis 连接并 ping 通，返回 Store 与统一 Close。
func Open(ctx context.Context, mysqlDSN, redisAddr string, farmTTL time.Duration) (*Store, func() error, error) {
	db, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("store: open mysql: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("store: ping mysql: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("store: ping redis: %w", err)
	}

	closeFn := func() error {
		rdbErr := rdb.Close()
		dbErr := db.Close()
		if dbErr != nil {
			return dbErr
		}
		return rdbErr
	}
	return New(db, rdb, farmTTL), closeFn, nil
}

var (
	_ SessionStore = (*Store)(nil)
	_ FarmStore    = (*Store)(nil)
)
