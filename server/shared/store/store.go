// Package store 提供共享数据库上的持久化适配器。
//
// 上层服务只依赖按领域拆分的 AccountStore、FarmStore、FriendStore、
// TaskMailStore 等接口，不直接持有 *sql.DB 或 *redis.Client。
//
// 具体实现 Store 组合 MySQL（durable）与 Redis（session + farm 缓存）：
//   - mysql.go：account/player/farm_plot 的读写（Store 的 MySQL-only 方法）
//   - redis.go：session:{token} 与 farm:{uid} 缓存（Store 的 Redis-only 方法 +
//     LoadFarm/SaveFarm 的回填编排）
//   - Presence 可选独立 Redis（FARM_PRESENCE_REDIS_ADDR）
//   - farm_codec.go：farm_plot.blob 列的单一编解码函数
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"farm/server/domain/farm"
)

// SessionStore 管理 token -> uid 以及 uid -> 当前 token 的单会话索引。
type SessionStore interface {
	Put(ctx context.Context, token string, uid uint64, ttl time.Duration) error
	Get(ctx context.Context, token string) (uint64, error)
	Delete(ctx context.Context, token string) error
}

// AccountStore allocates globally unique account IDs in the shared database.
// Gateway instances must not generate uid values from process-local state.
type AccountStore interface {
	CreateAccount(ctx context.Context, username, passwordHash string) (uint64, error)
	GetAccountByUsername(ctx context.Context, username string) (uid uint64, passwordHash string, err error)
	// UpdatePasswordHash uses the previously read hash as a compare-and-swap
	// guard, so concurrent logins cannot overwrite a newer password hash.
	UpdatePasswordHash(ctx context.Context, uid uint64, previousHash, passwordHash string) (bool, error)
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
	// SaveFarms 在一个事务内批量持久化；单元素 SaveFarm 应走同一实现路径。
	SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error
}

// FriendStore 管理以 friendship 表为准的双向好友关系，以及待处理申请。
type FriendStore interface {
	AreFriends(ctx context.Context, a, b uint64) (bool, error)
	AddFriends(ctx context.Context, a, b uint64) error
	RemoveFriends(ctx context.Context, a, b uint64) error
	ListFriends(ctx context.Context, uid uint64) ([]FriendRow, error)
	CountFriends(ctx context.Context, uid uint64) (int, error)
	FindUserByUsername(ctx context.Context, username string) (UserSearchRow, error)
	CreateFriendRequest(ctx context.Context, fromUID, toUID uint64) error
	ListIncomingFriendRequests(ctx context.Context, uid uint64) ([]FriendRequestRow, error)
	AcceptFriendRequest(ctx context.Context, toUID, fromUID uint64) error
	RejectFriendRequest(ctx context.Context, toUID, fromUID uint64) error
}

// TaskMailStore manages server-local calendar-day tasks, direct task rewards,
// mail and attachment claims.
// 任务与每日登录奖励直接入账；系统/管理员邮件附件在 MailClaim 时入账。
type TaskMailStore interface {
	ListTasks(ctx context.Context, uid uint64, dayKey int64) ([]Task, error)
	AdvanceTask(ctx context.Context, uid uint64, dayKey int64, taskID, amount uint32) (TaskAdvanceResult, error)
	ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (TaskReward, error)
	ListMails(ctx context.Context, uid uint64) ([]Mail, error)
	// MarkMailsRead 将一封邮件标记为已读；mailID=0 表示当前玩家的全部邮件。
	MarkMailsRead(ctx context.Context, uid uint64, mailID uint64) (int64, error)
	// DeleteMails 删除一封可清理邮件；mailID=0 表示批量清理。未领取附件始终保留。
	DeleteMails(ctx context.Context, uid uint64, mailID uint64) (int64, error)
	ClaimMail(ctx context.Context, uid uint64, mailID uint64) (Mail, error)
	ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (TaskReward, error)
}

// Task is the client-safe view of one calendar-day task.
type Task struct {
	ID uint32 `json:"id"`
	// DayKey identifies the server-local calendar day this task belongs to.
	// It lets clients discard a delayed TaskNotify after the daily reset.
	DayKey     int64  `json:"day_key"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Progress   uint32 `json:"progress"`
	Target     uint32 `json:"target"`
	RewardCoin int64  `json:"reward_coin"`
	Claimed    bool   `json:"claimed"`
}

// TaskAdvanceResult reports the persisted, client-safe task state after a
// gameplay event. Changed and JustCompleted describe this specific advancement.
type TaskAdvanceResult struct {
	Task          Task
	Changed       bool
	JustCompleted bool
}

// TaskReward 是任务领取时直接入账的玩家奖励。
type TaskReward struct {
	Coin int64  `json:"coin"`
	Exp  uint32 `json:"exp"`
}

// DirectClaimState is the Actor-owned economy before a direct task/mail
// reward and the sequence that the successful reward will produce. A claim
// transaction writes the resulting absolute economy at NextFarmSeq, fencing
// older asynchronous journal projections without first materializing them.
type DirectClaimState struct {
	Coin        int64
	NextFarmSeq uint64
}

// Mail 是个人邮件与其可领取金币附件。
type Mail struct {
	ID             uint64 `json:"id"`
	Title          string `json:"title"`
	AttachmentCoin int64  `json:"attachment_coin"`
	Claimed        bool   `json:"claimed"`
	Read           bool   `json:"read"`
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
	ErrSessionReplaced          = errors.New("store: session replaced by a newer login")
	ErrFarmNotFound             = errors.New("store: farm not found")
	ErrPlayerNotFound           = errors.New("store: player not found")
	ErrAlreadyFriend            = errors.New("store: already friends")
	ErrFriendLimitSelf          = errors.New("store: friend limit reached for self")
	ErrFriendLimitPeer          = errors.New("store: friend limit reached for peer")
	ErrCannotFriendSelf         = errors.New("store: cannot friend self")
	ErrFriendRequestPending     = errors.New("store: friend request already pending")
	ErrFriendRequestNotFound    = errors.New("store: friend request not found")
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

	// 单进程连接池必须有硬上限。database/sql 默认允许无限并发建连，好友冷缓存的
	// 突发查询会让多个服务一起打穿 MySQL max_connections，而不是形成可控排队。
	defaultMySQLMaxOpenConns = 24
	defaultMySQLMaxIdleConns = 12
	mysqlMaxOpenConnsPerCPU  = 32
	defaultMySQLConnLifetime = 5 * time.Minute
	defaultMySQLConnIdleTime = 1 * time.Minute

	// Go-redis defaults PoolSize to 10*GOMAXPROCS. Farm is deliberately limited
	// to one CPU in the benchmark topology, but cold Actor loads are I/O bound
	// and may have hundreds of independent Redis reads in flight. Keep cache
	// concurrency independent from the CPU quota so those loads do not queue
	// behind a ten-connection default pool.
	defaultRedisPoolSize     = 128
	defaultRedisMinIdleConns = 32
)

// Store 是 SessionStore 与 FarmStore 的唯一实现，组合 MySQL 与 Redis。
type Store struct {
	db             *sql.DB
	rdb            *redis.Client
	presenceRDB    *redis.Client
	farmTTL        time.Duration
	outboxFarmID   string
	taskInit       dailyTaskInitCache
	taskRead       boundedTTLCache[taskReadKey, []Task]
	taskCacheState [taskCacheStateShardCount]taskCacheState
	mailbox        mailboxCache
	taskBatches    taskReadBatcher
	mailBatches    mailReadBatcher
}

// SetOutboxFarmID scopes durable cross-farm events to the Farm instance that
// produced them. It must be called during startup, before projectors or request
// handlers begin writing.
func (s *Store) SetOutboxFarmID(instanceID string) {
	if s == nil {
		return
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		instanceID = "farm-0"
	}
	s.outboxFarmID = instanceID
}

func (s *Store) outboxScope() string {
	if s == nil || s.outboxFarmID == "" {
		return "farm-0"
	}
	return s.outboxFarmID
}

// CachedFriendStore returns the Social-facing friendship store with bounded
// process-local caches and Redis cache-aside reads. MySQL remains authoritative.
// Other services should continue to use Social's gRPC boundary instead of
// constructing this wrapper themselves.
func (s *Store) CachedFriendStore() FriendStore {
	if s == nil {
		return s
	}
	if s.rdb == nil {
		return newCachedFriendStore(s, nil)
	}
	return newCachedFriendStoreWithBus(s, s.rdb, redisFriendInvalidationBus{client: s.rdb})
}

// New 用已建立的 *sql.DB / *redis.Client 组装 Store，farmTTL<=0 时用 DefaultFarmCacheTTL。
func New(db *sql.DB, rdb *redis.Client, farmTTL time.Duration) *Store {
	if farmTTL <= 0 {
		farmTTL = DefaultFarmCacheTTL
	}
	storage := &Store{db: db, rdb: rdb, farmTTL: farmTTL}
	storage.taskRead.capacity = defaultReadCacheCapacity
	storage.mailbox.local.ttl = mailLocalCacheTTL
	storage.mailbox.local.capacity = mailLocalCacheCapacity
	return storage
}

// Open 按 DSN / addr 建立 MySQL 与 Redis 连接并 ping 通，返回 Store 与统一 Close。
func Open(ctx context.Context, mysqlDSN, redisAddr string, farmTTL time.Duration) (*Store, func() error, error) {
	optimizedDSN, err := mysqlDSNWithInterpolation(mysqlDSN)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("mysql", optimizedDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("store: open mysql: %w", err)
	}
	configureMySQLPool(db)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("store: ping mysql: %w", err)
	}

	rdb, err := openRedisClient(ctx, redisAddr)
	if err != nil {
		db.Close()
		return nil, nil, err
	}

	storage := New(db, rdb, farmTTL)
	storage.startMailboxInvalidations()
	closeFn := func() error {
		storage.stopMailboxInvalidations()
		rdbErr := rdb.Close()
		dbErr := db.Close()
		if dbErr != nil {
			return dbErr
		}
		return rdbErr
	}
	return storage, closeFn, nil
}

func openRedisClient(ctx context.Context, addr string) (*redis.Client, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, errors.New("store: redis address is empty")
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		PoolSize:     defaultRedisPoolSize,
		MinIdleConns: defaultRedisMinIdleConns,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("store: ping redis %s: %w", addr, err)
	}
	return rdb, nil
}

// OpenWithPresence opens MySQL plus a cache Redis, and optionally a dedicated
// Presence Redis. An empty presenceAddr, or one equal to cacheAddr, reuses the
// cache client so local/dev topologies stay single-instance.
func OpenWithPresence(
	ctx context.Context,
	mysqlDSN, cacheAddr, presenceAddr string,
	farmTTL time.Duration,
) (*Store, func() error, error) {
	storage, closeStorage, err := Open(ctx, mysqlDSN, cacheAddr, farmTTL)
	if err != nil {
		return nil, nil, err
	}
	presenceAddr = strings.TrimSpace(presenceAddr)
	cacheAddr = strings.TrimSpace(cacheAddr)
	if presenceAddr == "" || presenceAddr == cacheAddr {
		return storage, closeStorage, nil
	}
	client, err := openRedisClient(ctx, presenceAddr)
	if err != nil {
		_ = closeStorage()
		return nil, nil, fmt.Errorf("store: presence redis: %w", err)
	}
	storage.presenceRDB = client
	return storage, func() error {
		return errors.Join(client.Close(), closeStorage())
	}, nil
}

// mysqlDSNWithInterpolation lets the driver safely encode placeholders into
// COM_QUERY. This removes the PREPARE/EXECUTE/CLOSE round trips that otherwise
// occurred for almost every short request in this service.
func mysqlDSNWithInterpolation(dsn string) (string, error) {
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("store: parse mysql DSN: %w", err)
	}
	config.InterpolateParams = true
	return config.FormatDSN(), nil
}

func configureMySQLPool(db *sql.DB) {
	if db == nil {
		return
	}
	maxOpen, maxIdle := mysqlPoolSizes(runtime.GOMAXPROCS(0))
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(defaultMySQLConnLifetime)
	db.SetConnMaxIdleTime(defaultMySQLConnIdleTime)
}

func mysqlPoolSizes(processors int) (maxOpen, maxIdle int) {
	processors = max(processors, 1)
	maxOpen = max(defaultMySQLMaxOpenConns, processors*mysqlMaxOpenConnsPerCPU)
	maxIdle = max(defaultMySQLMaxIdleConns, maxOpen/2)
	return maxOpen, maxIdle
}

// Ping 检查 MySQL 与 Redis 是否仍可响应，供 /readyz 使用。
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil || s.rdb == nil {
		return errors.New("store: not open")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	if s.presenceRDB != nil && s.presenceRDB != s.rdb {
		if err := s.presenceRDB.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("presence redis: %w", err)
		}
	}
	return nil
}

func (s *Store) presenceClient() *redis.Client {
	if s == nil {
		return nil
	}
	if s.presenceRDB != nil {
		return s.presenceRDB
	}
	return s.rdb
}

var (
	_ SessionStore  = (*Store)(nil)
	_ AccountStore  = (*Store)(nil)
	_ FarmStore     = (*Store)(nil)
	_ FriendStore   = (*Store)(nil)
	_ TaskMailStore = (*Store)(nil)
)
