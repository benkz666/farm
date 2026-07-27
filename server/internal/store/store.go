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

// FriendStore 管理以 friendship 表为准的双向好友关系。
type FriendStore interface {
	AreFriends(ctx context.Context, a, b uint64) (bool, error)
	AddFriends(ctx context.Context, a, b uint64) error
	RemoveFriends(ctx context.Context, a, b uint64) error
	ListFriends(ctx context.Context, uid uint64) ([]FriendRow, error)
	CountFriends(ctx context.Context, uid uint64) (int, error)
	FindUserByUsername(ctx context.Context, username string) (UserSearchRow, error)
}

// TaskMailStore 管理逻辑日任务、奖励邮件和附件领取。
// 任务领奖与每日登录只创建邮件；附件在 MailClaim 时才入账。
type TaskMailStore interface {
	ListTasks(ctx context.Context, uid uint64, logicDay int64) ([]Task, error)
	ClaimTask(ctx context.Context, uid uint64, logicDay int64, taskID uint32) (Mail, error)
	ListMails(ctx context.Context, uid uint64) ([]Mail, error)
	ClaimMail(ctx context.Context, uid uint64, mailID uint64) (Mail, error)
	ClaimDailyLogin(ctx context.Context, uid uint64, logicDay int64) (Mail, error)
}

// Task 是当前逻辑日任务的客户端安全视图。
type Task struct {
	ID         uint32 `json:"id"`
	Title      string `json:"title"`
	Progress   uint32 `json:"progress"`
	Target     uint32 `json:"target"`
	RewardCoin int64  `json:"reward_coin"`
	Claimed    bool   `json:"claimed"`
}

// Mail 是个人邮件与其可领取金币附件。
type Mail struct {
	ID             uint64 `json:"id"`
	Title          string `json:"title"`
	AttachmentCoin int64  `json:"attachment_coin"`
	Claimed        bool   `json:"claimed"`
	CreatedAt      int64  `json:"created_at"`
}

// UserSearchRow 是按用户名搜索好友时可公开的玩家标识与昵称。
// 不返回账号密码哈希，避免认证字段穿过社交边界。
type UserSearchRow struct {
	UID      uint64
	Nickname string
}

// 哨兵错误，供上层用 errors.Is 判定并映射到 pkgerr 协议码。
var (
	ErrUsernameTaken            = errors.New("store: username already taken")
	ErrAccountNotFound          = errors.New("store: account not found")
	ErrSessionNotFound          = errors.New("store: session not found")
	ErrFarmNotFound             = errors.New("store: farm not found")
	ErrPlayerNotFound           = errors.New("store: player not found")
	ErrAlreadyFriend            = errors.New("store: already friends")
	ErrFriendLimitSelf          = errors.New("store: friend limit reached for self")
	ErrFriendLimitPeer          = errors.New("store: friend limit reached for peer")
	ErrTaskNotComplete          = errors.New("store: task not complete")
	ErrTaskAlreadyClaimed       = errors.New("store: task already claimed")
	ErrMailNotFound             = errors.New("store: mail not found")
	ErrMailNoAttachment         = errors.New("store: mail has no attachment")
	ErrMailAlreadyClaimed       = errors.New("store: mail attachment already claimed")
	ErrDailyLoginAlreadyClaimed = errors.New("store: daily login already claimed")
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
	_ SessionStore  = (*Store)(nil)
	_ FarmStore     = (*Store)(nil)
	_ FriendStore   = (*Store)(nil)
	_ TaskMailStore = (*Store)(nil)
)
