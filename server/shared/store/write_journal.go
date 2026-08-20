package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"

	"google.golang.org/protobuf/proto"
)

const (
	writeJournalVersion       = 2
	writeJournalFarmCommit    = "farm_commit"
	writeJournalTaskAdvance   = "task_advance"
	writeJournalCodexReward   = "codex_reward"
	writeJournalOutboxAck     = "outbox_ack"
	writeJournalBarrier       = "direct_write_barrier"
	defaultWriteJournalShards = 32
	defaultWriteJournalBatch  = 1024
)

const (
	projectionClaimPollMin = 500 * time.Millisecond
	// Redis 会在第一条消息到达时立即唤醒每个 shard。高并发下若马上
	// 开 MySQL 事务，即使 BatchSize 很大，实际也只有几条记录，最终把
	// innodb_flush_log_at_trx_commit=1 的磁盘变成大量小事务。获取投影
	// permit 后短暂等待新的同 shard 记录；此时没有占用 DB 连接或行锁，
	// 但能显著提高每次事务的有效批量。
	projectionCoalesceWindow = 50 * time.Millisecond
)

const maxRetainedJournalProtoBuffer = 64 << 10

var journalProtoBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, 0, 512)
	return &buffer
}}

func acquireJournalProtoBuffer() *[]byte {
	buffer := journalProtoBufferPool.Get().(*[]byte)
	*buffer = (*buffer)[:0]
	return buffer
}

func releaseJournalProtoBuffer(buffer *[]byte) {
	if buffer == nil {
		return
	}
	if cap(*buffer) > maxRetainedJournalProtoBuffer {
		*buffer = nil
	} else {
		clear(*buffer)
		*buffer = (*buffer)[:0]
	}
	journalProtoBufferPool.Put(buffer)
}

// FarmWriteJournalConfig controls the Redis Streams durability boundary and
// the asynchronous MySQL projector. A stream belongs to one Farm instance and
// one UID shard, so a single worker can preserve strict per-UID ordering while
// different shards materialize in parallel.
type FarmWriteJournalConfig struct {
	Prefix         string
	InstanceID     string
	Shards         int
	Projectors     int
	BatchSize      int64
	Block          time.Duration
	LatestTTL      time.Duration
	IOTimeout      time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	ClaimIdle      time.Duration
	ReplicaAcks    int
	ReplicaTimeout time.Duration
}

func DefaultFarmWriteJournalConfig(instanceID string) FarmWriteJournalConfig {
	return FarmWriteJournalConfig{
		Prefix:         "farm:write",
		InstanceID:     instanceID,
		Shards:         defaultWriteJournalShards,
		Projectors:     4,
		BatchSize:      defaultWriteJournalBatch,
		Block:          50 * time.Millisecond,
		LatestTTL:      24 * time.Hour,
		IOTimeout:      5 * time.Second,
		RetryMin:       10 * time.Millisecond,
		RetryMax:       time.Second,
		ClaimIdle:      5 * time.Second,
		ReplicaTimeout: 100 * time.Millisecond,
	}
}

func normalizeFarmWriteJournalConfig(config FarmWriteJournalConfig) FarmWriteJournalConfig {
	defaults := DefaultFarmWriteJournalConfig(config.InstanceID)
	if strings.TrimSpace(config.Prefix) == "" {
		config.Prefix = defaults.Prefix
	}
	config.Prefix = strings.TrimSuffix(config.Prefix, ":")
	config.InstanceID = sanitizeJournalPart(config.InstanceID)
	if config.InstanceID == "" {
		config.InstanceID = "farm-0"
	}
	if config.Shards <= 0 {
		config.Shards = defaults.Shards
	}
	if config.Projectors <= 0 {
		config.Projectors = defaults.Projectors
	}
	config.Projectors = min(config.Projectors, config.Shards)
	if config.BatchSize <= 0 {
		config.BatchSize = defaults.BatchSize
	}
	if config.Block <= 0 {
		config.Block = defaults.Block
	}
	if config.LatestTTL <= 0 {
		config.LatestTTL = defaults.LatestTTL
	}
	if config.IOTimeout <= 0 {
		config.IOTimeout = defaults.IOTimeout
	}
	if config.RetryMin <= 0 {
		config.RetryMin = defaults.RetryMin
	}
	if config.RetryMax < config.RetryMin {
		config.RetryMax = defaults.RetryMax
	}
	if config.ClaimIdle <= 0 {
		config.ClaimIdle = defaults.ClaimIdle
	}
	if config.ReplicaTimeout <= 0 {
		config.ReplicaTimeout = defaults.ReplicaTimeout
	}
	return config
}

func sanitizeJournalPart(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

type writeJournalRecord struct {
	Version int                 `json:"version"`
	EventID string              `json:"event_id"`
	Kind    string              `json:"kind"`
	UID     string              `json:"uid"`
	FarmSeq string              `json:"farm_seq,omitempty"`
	Commit  *outbox.FarmCommit  `json:"commit,omitempty"`
	Task    *journalTaskAdvance `json:"task,omitempty"`
	Codex   *journalCodexReward `json:"codex,omitempty"`
	Ack     *journalOutboxAck   `json:"ack,omitempty"`
}

type journalTaskAdvance struct {
	DayKey int64  `json:"day_key"`
	TaskID uint32 `json:"task_id"`
	Amount uint32 `json:"amount"`
}

type journalCodexReward struct {
	Progress farm.CodexProgress `json:"progress"`
}

type journalTaskClaimProjection struct {
	uid                 uint64
	dayKey              int64
	taskID              uint32
	claimedAt           int64
	streamMS, streamSeq uint64
}

type journalMailMutationProjection struct {
	uid                 uint64
	mailID              uint64
	kind                farmv1.FarmWriteMailMutationKind
	occurredAt          int64
	streamMS, streamSeq uint64
}

type journalOutboxAck struct {
	EventID string `json:"event_id"`
}

var appendWriteJournalScript = redis.NewScript(`
local stream_id = redis.call('XADD', KEYS[1], '*',
  'event_id', ARGV[1], 'kind', ARGV[2], 'uid', ARGV[3],
  'farm_seq', ARGV[4], 'body', ARGV[5])
if ARGV[6] == '1' then
  redis.call('HSET', KEYS[2], 'event_id', ARGV[1], 'body', ARGV[5])
  redis.call('PEXPIRE', KEYS[2], ARGV[8])
end
if ARGV[7] == '1' then
  redis.call('RPUSH', KEYS[3], stream_id)
end
return stream_id
`)

// appendFarmWriteBatchScript makes a multi-UID farm commit one Redis command.
// Each UID still owns its shard stream record (so projector ordering remains
// unchanged), while all records become visible atomically and share one
// foreground network round trip.
var appendFarmWriteBatchScript = redis.NewScript(`
local result = {}
local arg = 1
for key = 1, #KEYS, 3 do
  local stream_id = redis.call('XADD', KEYS[key], '*',
    'event_id', ARGV[arg], 'kind', ARGV[arg + 1], 'uid', ARGV[arg + 2],
    'farm_seq', ARGV[arg + 3], 'body', ARGV[arg + 4])
  if ARGV[arg + 5] == '1' then
    redis.call('HSET', KEYS[key + 1], 'event_id', ARGV[arg], 'body', ARGV[arg + 4])
    redis.call('PEXPIRE', KEYS[key + 1], ARGV[arg + 7])
  end
  if ARGV[arg + 6] == '1' then
    redis.call('RPUSH', KEYS[key + 2], stream_id)
  end
  result[#result + 1] = stream_id
  arg = arg + 8
end
return result
`)

var deleteLatestJournalScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'event_id') == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

var acknowledgeWriteJournalScript = redis.NewScript(`
local acknowledged = redis.call('XACK', KEYS[1], ARGV[1], unpack(ARGV, 2))
redis.call('XDEL', KEYS[1], unpack(ARGV, 2))
for key = 2, #KEYS do
  redis.call('LREM', KEYS[key], 1, ARGV[key])
  if redis.call('LLEN', KEYS[key]) == 0 then
    redis.call('DEL', KEYS[key])
  end
end
return acknowledged
`)

// FarmWriteJournal is both the low-latency Redis Streams append boundary and
// the owner of ordered MySQL projector workers.
type FarmWriteJournal struct {
	base      *Store
	rdb       *redis.Client // blocking projector/read-side pool
	lookupRDB *redis.Client // non-blocking latest/barrier metadata pool
	appendRDB *redis.Client // foreground durable-append pool
	config    FarmWriteJournalConfig
	metrics   *telemetry.Metrics

	started atomic.Bool
	closed  atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	observerMu        sync.RWMutex
	taskObserver      func(uint64, Task)
	mailObserver      func(uint64)
	consumerID        string
	barrierMu         sync.Mutex
	barriers          map[string]chan struct{}
	projectLimiter    *adaptiveProjectionLimiter
	barrierWaiters    atomic.Int64
	appendInFlight    atomic.Int32
	foregroundQueue   atomic.Int64
	projectionBacklog atomic.Int64
	targetedReady     atomic.Bool
	targetedPermits   chan struct{}
}

// OpenFarmWriteJournal opens dedicated connection pools against the journal
// Redis instance (FARM_EVENT_REDIS_ADDR, falling back to FARM_REDIS_ADDR).
// Separate pools prevent blocking projector reads from starving foreground
// appends; cache and presence traffic belongs on their own Redis processes.
func OpenFarmWriteJournal(
	ctx context.Context,
	base *Store,
	redisAddr string,
	config FarmWriteJournalConfig,
) (*FarmWriteJournal, func() error, error) {
	if base == nil || base.db == nil {
		return nil, nil, errors.New("store: write journal requires MySQL storage")
	}
	if strings.TrimSpace(redisAddr) == "" {
		return nil, nil, errors.New("store: write journal Redis address is empty")
	}
	// Every journal shard owns a blocking XREADGROUP loop. The projector pool
	// therefore needs at least one connection per shard, but metadata lookups
	// must not share it: otherwise all connections can remain blocked for the
	// configured XREAD window and every cold Actor EXISTS inherits that delay.
	projectPoolSize := max(config.Shards+config.Projectors+8, 64)
	projectClient := redis.NewClient(&redis.Options{
		Addr: redisAddr, PoolSize: projectPoolSize, MinIdleConns: min(config.Shards, projectPoolSize),
	})
	if err := projectClient.Ping(ctx).Err(); err != nil {
		_ = projectClient.Close()
		return nil, nil, fmt.Errorf("store: ping write journal Redis: %w", err)
	}
	lookupClient := redis.NewClient(&redis.Options{Addr: redisAddr, PoolSize: 128, MinIdleConns: 32})
	if err := lookupClient.Ping(ctx).Err(); err != nil {
		_ = lookupClient.Close()
		_ = projectClient.Close()
		return nil, nil, fmt.Errorf("store: ping write journal lookup Redis: %w", err)
	}
	appendClient := redis.NewClient(&redis.Options{Addr: redisAddr, PoolSize: 64, MinIdleConns: 8})
	if err := appendClient.Ping(ctx).Err(); err != nil {
		_ = appendClient.Close()
		_ = lookupClient.Close()
		_ = projectClient.Close()
		return nil, nil, fmt.Errorf("store: ping write journal append Redis: %w", err)
	}
	journal := NewFarmWriteJournal(base, projectClient, config)
	journal.lookupRDB = lookupClient
	journal.appendRDB = appendClient
	closeClients := func() error {
		return errors.Join(appendClient.Close(), lookupClient.Close(), projectClient.Close())
	}
	return journal, closeClients, nil
}

func NewFarmWriteJournal(base *Store, client *redis.Client, config FarmWriteJournalConfig) *FarmWriteJournal {
	config = normalizeFarmWriteJournalConfig(config)
	return &FarmWriteJournal{
		base: base, rdb: client, lookupRDB: client, appendRDB: client, config: config,
		// Keep rolling instances distinct. A replacement only takes over an old
		// worker's pending message after ClaimIdle instead of racing the active
		// projector and applying the same batch concurrently.
		consumerID: config.InstanceID + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
			strconv.FormatUint(journalEventCounter.Add(1), 36),
		barriers:       make(map[string]chan struct{}),
		projectLimiter: newAdaptiveProjectionLimiter(config.Projectors),
		// Claim projection is foreground I/O. It shares MySQL with background
		// projectors but must absorb short task/mail bursts without building a
		// seconds-long gRPC barrier queue. Two slots per configured projector kept
		// the 10k mixed tier below the DB's CPU capacity while preserving a hard
		// bound on direct projection concurrency.
		targetedPermits: make(chan struct{}, max(config.Projectors*2, 8)),
	}
}

func (journal *FarmWriteJournal) SetMetrics(metrics *telemetry.Metrics) {
	journal.metrics = metrics
	if metrics != nil {
		metrics.SetWriteJournalProjectionLimit(journal.projectLimiter.Limit())
	}
}

func (journal *FarmWriteJournal) SetTaskObserver(observer func(uint64, Task)) {
	journal.observerMu.Lock()
	journal.taskObserver = observer
	journal.observerMu.Unlock()
}

func (journal *FarmWriteJournal) SetMailObserver(observer func(uint64)) {
	journal.observerMu.Lock()
	journal.mailObserver = observer
	journal.observerMu.Unlock()
}

func (journal *FarmWriteJournal) Start(ctx context.Context) error {
	if journal == nil || journal.base == nil || journal.rdb == nil || journal.appendRDB == nil {
		return errors.New("store: invalid write journal")
	}
	if !journal.started.CompareAndSwap(false, true) {
		return nil
	}
	for shard := 0; shard < journal.config.Shards; shard++ {
		err := journal.rdb.XGroupCreateMkStream(ctx, journal.streamKey(shard), journal.groupName(), "0").Err()
		if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
			journal.started.Store(false)
			return fmt.Errorf("store: create write journal group shard %d: %w", shard, err)
		}
	}
	// The per-UID pending index did not exist in the original stream format.
	// Enable targeted projection only when this process starts with empty logs;
	// otherwise an older, unindexed record could be skipped during a rolling
	// deployment. A process that drains legacy backlog safely keeps the old
	// shard barrier until its next restart.
	targetedReady := true
	for shard := 0; shard < journal.config.Shards; shard++ {
		length, err := journal.rdb.XLen(ctx, journal.streamKey(shard)).Result()
		if err != nil {
			journal.started.Store(false)
			return fmt.Errorf("store: inspect write journal shard %d: %w", shard, err)
		}
		if length != 0 {
			targetedReady = false
		}
	}
	journal.targetedReady.Store(targetedReady)
	journal.ctx, journal.cancel = context.WithCancel(context.Background())
	for shard := 0; shard < journal.config.Shards; shard++ {
		journal.wg.Add(1)
		go journal.runProjector(shard)
	}
	return nil
}

func (journal *FarmWriteJournal) Ping(ctx context.Context) error {
	if journal == nil || journal.rdb == nil || !journal.started.Load() || journal.closed.Load() {
		return errors.New("write journal is not running")
	}
	if err := journal.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("write journal Redis: %w", err)
	}
	if journal.appendRDB != journal.rdb {
		if err := journal.appendRDB.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("write journal append Redis: %w", err)
		}
	}
	return nil
}

func (journal *FarmWriteJournal) WrapFarmStore(base FarmStore) FarmStore {
	return &journalFarmStore{FarmStore: base, journal: journal}
}

func (journal *FarmWriteJournal) WrapOutboxStore(base OutboxStore) OutboxStore {
	return &journalOutboxStore{OutboxStore: base, journal: journal}
}

// WrapDirectStore protects the remaining low-frequency transactional claims.
// Those operations still validate and credit in MySQL, but first wait until
// older asynchronous farm snapshots for the UID have been projected so an
// old snapshot cannot overwrite a newly claimed reward.
func (journal *FarmWriteJournal) WrapDirectStore(base *Store) *JournalDirectStore {
	return &JournalDirectStore{
		Store: base, journal: journal,
		directWrites: newDirectWriteBatcher(journal.ctx, base.db),
	}
}

type journalFarmStore struct {
	FarmStore
	journal *FarmWriteJournal
}

type farmBatchLoader interface {
	LoadFarms(context.Context, []uint64) (map[uint64]*farm.Aggregate, error)
}

func (*journalFarmStore) SupportsIncrementalFarmCommits() bool { return true }

func (store *journalFarmStore) AdjustForegroundPressure(delta int) {
	if store != nil && store.journal != nil {
		depth := store.journal.foregroundQueue.Add(int64(delta))
		if depth < 0 {
			store.journal.foregroundQueue.Store(0)
		}
		store.journal.adjustProjectionLimit()
	}
}

func (store *journalFarmStore) LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error) {
	loaded, err := store.LoadFarms(ctx, []uint64{uid})
	if err != nil {
		return nil, err
	}
	agg := loaded[uid]
	if agg == nil {
		return nil, fmt.Errorf("store: batch farm load omitted uid %d", uid)
	}
	return agg, nil
}

// LoadFarms overlaps the event-log freshness lookup with the business-cache
// MGET. The common cache-hot path therefore costs one parallel Redis round
// trip instead of two serial round trips per cold Actor. Only UIDs with an
// unprojected mutation pay the barrier and targeted reload cost.
func (store *journalFarmStore) LoadFarms(ctx context.Context, uids []uint64) (map[uint64]*farm.Aggregate, error) {
	type loadResult struct {
		farms map[uint64]*farm.Aggregate
		err   error
	}
	loadedCh := make(chan loadResult, 1)
	go func() {
		farms, err := store.loadBaseFarms(ctx, uids)
		loadedCh <- loadResult{farms: farms, err: err}
	}()

	pending, latestErr := store.journal.latestFarmUIDs(ctx, uids)
	loaded := <-loadedCh
	if latestErr != nil {
		return nil, latestErr
	}
	if loaded.err != nil {
		return nil, loaded.err
	}
	if len(pending) == 0 {
		return loaded.farms, nil
	}
	// Barriers are rare on cold EnterFarm. Keep them ordered and explicit so a
	// reload can never observe a partially projected sequence for the same UID.
	for _, uid := range pending {
		if err := store.journal.WaitUIDProjected(ctx, uid, "actor_load"); err != nil {
			return nil, err
		}
	}
	refreshed, err := store.loadBaseFarms(ctx, pending)
	if err != nil {
		return nil, err
	}
	for uid, aggregate := range refreshed {
		loaded.farms[uid] = aggregate
	}
	return loaded.farms, nil
}

func (store *journalFarmStore) loadBaseFarms(ctx context.Context, uids []uint64) (map[uint64]*farm.Aggregate, error) {
	if batch, ok := store.FarmStore.(farmBatchLoader); ok {
		return batch.LoadFarms(ctx, uids)
	}
	loaded := make(map[uint64]*farm.Aggregate, len(uids))
	for _, uid := range uids {
		if _, exists := loaded[uid]; exists {
			continue
		}
		agg, err := store.FarmStore.LoadFarm(ctx, uid)
		if err != nil {
			return nil, err
		}
		loaded[uid] = agg
	}
	return loaded, nil
}

func (store *journalFarmStore) SaveFarm(ctx context.Context, aggregate *farm.Aggregate) error {
	return store.CommitFarms(ctx, []outbox.FarmCommit{{Snapshot: aggregate}})
}

func (store *journalFarmStore) SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error {
	commits := make([]outbox.FarmCommit, len(snapshots))
	for index, snapshot := range snapshots {
		commits[index] = outbox.FarmCommit{Snapshot: snapshot}
	}
	return store.CommitFarms(ctx, commits)
}

func (store *journalFarmStore) CommitFarms(ctx context.Context, commits []outbox.FarmCommit) error {
	return store.journal.AppendFarmCommits(ctx, commits)
}

type journalOutboxStore struct {
	OutboxStore
	journal *FarmWriteJournal
}

type JournalDirectStore struct {
	*Store
	journal      *FarmWriteJournal
	directWrites *directWriteBatcher
}

func (store *JournalDirectStore) ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (TaskReward, error) {
	barrierCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	if err := store.journal.WaitUIDProjected(barrierCtx, uid, "task_claim"); err != nil {
		return TaskReward{}, err
	}
	return store.Store.ClaimTask(ctx, uid, dayKey, taskID)
}

// ClaimTaskAtState keeps the common claim path off the full-UID projection
// barrier. Most task rows are already complete in MySQL, so the first atomic
// claim succeeds immediately. If only an asynchronous task advancement is
// missing, project that task's high-water events and retry without
// materializing unrelated farm plots, inventory, codex or outbox records.
func (store *JournalDirectStore) ClaimTaskAtState(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	taskID uint32,
	state DirectClaimState,
) (TaskReward, error) {
	if state.NextFarmSeq == 0 {
		return TaskReward{}, errors.New("store: direct task claim has invalid next farm sequence")
	}
	reward, err := store.Store.claimTaskWithExecer(
		ctx, uid, dayKey, taskID, &state,
		directUIDExecer{batcher: store.directWrites, uid: uid},
	)
	if !errors.Is(err, ErrTaskNotComplete) {
		if err == nil && store.journal.metrics != nil {
			store.journal.metrics.ObserveWriteJournalBarrierFastPath("task_claim")
		}
		return reward, err
	}
	projectionCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	started := time.Now()
	err = store.journal.projectTaskThrough(projectionCtx, uid, dayKey, taskID)
	if store.journal.metrics != nil {
		store.journal.metrics.ObserveWriteJournalTargetedProjection(
			"task_claim", time.Since(started), err,
		)
	}
	if err != nil {
		return TaskReward{}, err
	}
	return store.Store.ClaimTaskAtState(ctx, uid, dayKey, taskID, state)
}

func (store *JournalDirectStore) ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (TaskReward, error) {
	barrierCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	if err := store.journal.WaitUIDProjected(barrierCtx, uid, "daily_login_claim"); err != nil {
		return TaskReward{}, err
	}
	return store.Store.ClaimDailyLogin(ctx, uid, dayKey)
}

func (store *JournalDirectStore) ClaimMail(ctx context.Context, uid, mailID uint64) (Mail, error) {
	barrierCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	if err := store.journal.WaitUIDProjected(barrierCtx, uid, "mail_claim"); err != nil {
		return Mail{}, err
	}
	return store.Store.ClaimMail(ctx, uid, mailID)
}

// ClaimMailAtState needs no projection barrier: the requested mail ID already
// names a durable MySQL row, while the fenced absolute economy update prevents
// older pending Farm mutations from overwriting the credited attachment.
func (store *JournalDirectStore) ClaimMailAtState(
	ctx context.Context,
	uid, mailID uint64,
	state DirectClaimState,
) (Mail, error) {
	if state.NextFarmSeq == 0 {
		return Mail{}, errors.New("store: direct mail claim has invalid next farm sequence")
	}
	mail, err := store.Store.claimMailWithExecer(
		ctx, uid, mailID, &state,
		directUIDExecer{batcher: store.directWrites, uid: uid},
	)
	if err == nil && store.journal.metrics != nil {
		store.journal.metrics.ObserveWriteJournalBarrierFastPath("mail_claim")
	}
	return mail, err
}

func (store *JournalDirectStore) MarkMailsRead(
	ctx context.Context,
	uid uint64,
	mailID uint64,
) (int64, error) {
	return store.Store.markMailsRead(
		ctx, uid, mailID,
		directUIDExecer{batcher: store.directWrites, uid: uid},
	)
}

func (store *JournalDirectStore) DeleteMails(
	ctx context.Context,
	uid uint64,
	mailID uint64,
) (int64, error) {
	return store.Store.deleteMails(
		ctx, uid, mailID,
		directUIDExecer{batcher: store.directWrites, uid: uid},
	)
}

func (journal *FarmWriteJournal) directWriteContext(parent context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := parent.Deadline(); hasDeadline {
		return context.WithCancel(parent)
	}
	// Direct claims are deliberately low-frequency. Bound their projection
	// barrier so an unhealthy projector cannot pin one Actor forever.
	timeout := journal.config.IOTimeout + journal.config.ClaimIdle + journal.config.IOTimeout
	return context.WithTimeout(parent, timeout)
}

func (store *journalOutboxStore) MarkOutboxPublished(ctx context.Context, eventID string) error {
	return store.journal.AppendOutboxAck(ctx, eventID)
}

func (store *journalOutboxStore) MarkOutboxPublishedBatch(ctx context.Context, eventIDs []string) error {
	return store.journal.AppendOutboxAcks(ctx, eventIDs)
}

func (journal *FarmWriteJournal) AppendFarmCommits(ctx context.Context, commits []outbox.FarmCommit) error {
	if err := journal.accepting(); err != nil {
		return err
	}
	if len(commits) == 0 {
		return nil
	}
	journal.appendInFlight.Add(1)
	journal.adjustProjectionLimit()
	defer func() {
		journal.appendInFlight.Add(-1)
		journal.adjustProjectionLimit()
	}()
	type encodedRecord struct {
		record writeJournalRecord
		body   []byte
		buffer *[]byte
	}
	encoded := make([]encodedRecord, 0, len(commits))
	defer func() {
		for index := range encoded {
			releaseJournalProtoBuffer(encoded[index].buffer)
		}
	}()
	for _, commit := range commits {
		mutation := commit.Mutation
		if mutation == nil && commit.Snapshot != nil {
			var err error
			mutation, err = outbox.NewFarmWriteMutation(
				commit.Snapshot, commit.Plan, nil, nil, nil,
				commit.Outbox, commit.TaskAdvances, commit.CodexRewards,
				commit.TaskClaims, commit.MailMutations,
			)
			if err != nil {
				return fmt.Errorf("store: build journal farm mutation: %w", err)
			}
		}
		if mutation == nil || mutation.Uid == 0 {
			return errors.New("store: invalid journal farm mutation")
		}
		record := writeJournalRecord{
			Version: writeJournalVersion,
			Kind:    writeJournalFarmCommit,
			UID:     strconv.FormatUint(mutation.Uid, 10),
			FarmSeq: strconv.FormatUint(mutation.FarmSeq, 10),
			Commit:  &outbox.FarmCommit{Mutation: mutation},
		}
		buffer := acquireJournalProtoBuffer()
		body, err := proto.MarshalOptions{Deterministic: true}.MarshalAppend((*buffer)[:0], mutation)
		if err != nil {
			releaseJournalProtoBuffer(buffer)
			return fmt.Errorf("store: encode protobuf farm mutation: %w", err)
		}
		*buffer = body
		record.EventID = deterministicJournalID(writeJournalFarmCommit, body)
		encoded = append(encoded, encodedRecord{record: record, body: body, buffer: buffer})
	}

	started := time.Now()
	connection := journal.appendRDB.Conn()
	defer connection.Close()
	keys := make([]string, 0, len(encoded)*3)
	args := make([]any, 0, len(encoded)*8)
	for _, item := range encoded {
		uid, _ := strconv.ParseUint(item.record.UID, 10, 64)
		shard := journal.shard(uid)
		keys = append(keys, journal.streamKey(shard), journal.latestKey(shard, uid), journal.pendingUIDKey(shard, uid))
		args = append(args, item.record.EventID, item.record.Kind, item.record.UID, item.record.FarmSeq,
			item.body, "1", "1", journal.config.LatestTTL.Milliseconds())
	}
	_, err := appendFarmWriteBatchScript.Eval(ctx, connection, keys, args...).Result()
	journal.observeAppend(started, len(encoded), err)
	if err != nil {
		return fmt.Errorf("store: append farm write journal: %w", err)
	}
	journal.cacheAppendedFarms(ctx, commits)
	return journal.waitForReplicas(ctx, connection)
}

// cacheAppendedFarms write-throughs the in-memory snapshot to the cache Redis
// after the durable journal append succeeds. The projector then leaves the
// hot key alone, so projection lag no longer turns every subsequent read into
// a cache miss against the same Redis that is already draining the stream.
func (journal *FarmWriteJournal) cacheAppendedFarms(ctx context.Context, commits []outbox.FarmCommit) {
	if journal == nil || journal.base == nil {
		return
	}
	snapshots := make([]*farm.Aggregate, 0, len(commits))
	for _, commit := range commits {
		if commit.Snapshot != nil && commit.Snapshot.UID != 0 {
			snapshots = append(snapshots, commit.Snapshot)
		}
	}
	if len(snapshots) == 0 {
		return
	}
	if err := journal.base.cacheFarmsPipeline(ctx, snapshots); err != nil {
		logFarmCacheFailure("cache_appended_farms_pipeline", snapshots, err)
		journal.base.invalidateFarmCaches(snapshots)
	}
}

// AdvanceTask implements the Farm gameplay task side effect as an event-log
// append. The final TaskNotify is emitted by the projector after MySQL commit.
func (journal *FarmWriteJournal) AdvanceTask(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	taskID, amount uint32,
) (TaskAdvanceResult, error) {
	if uid == 0 || amount == 0 || !IsDailyTaskID(taskID) {
		return TaskAdvanceResult{}, errors.New("store: invalid journal task advancement")
	}
	record := writeJournalRecord{
		Version: writeJournalVersion,
		EventID: uniqueJournalID(writeJournalTaskAdvance, uid),
		Kind:    writeJournalTaskAdvance,
		UID:     strconv.FormatUint(uid, 10),
		Task:    &journalTaskAdvance{DayKey: dayKey, TaskID: taskID, Amount: amount},
	}
	if err := journal.appendRecord(ctx, uid, record, false, true); err != nil {
		return TaskAdvanceResult{}, err
	}
	return TaskAdvanceResult{}, nil
}

// IssueCodexRewards logs idempotent mail materialization and returns the
// deterministic milestone notices immediately. The mail row itself is created
// by the projector.
func (journal *FarmWriteJournal) IssueCodexRewards(
	ctx context.Context,
	uid uint64,
	progress farm.CodexProgress,
) ([]farm.CodexRewardNotice, error) {
	if uid == 0 || progress.CropID == 0 || progress.HarvestCount == 0 {
		return nil, errors.New("store: invalid journal codex reward")
	}
	record := writeJournalRecord{
		Version: writeJournalVersion,
		Kind:    writeJournalCodexReward,
		UID:     strconv.FormatUint(uid, 10),
		Codex:   &journalCodexReward{Progress: progress},
	}
	identity, _ := json.Marshal(record)
	record.EventID = deterministicJournalID(writeJournalCodexReward, identity)
	if err := journal.appendRecord(ctx, uid, record, false, true); err != nil {
		return nil, err
	}
	return eligibleCodexRewardNotices(progress), nil
}

func eligibleCodexRewardNotices(progress farm.CodexProgress) []farm.CodexRewardNotice {
	notices := make([]farm.CodexRewardNotice, 0, len(gameconfig.CodexTiers))
	for _, milestone := range gameconfig.CodexTiers {
		if progress.HarvestCount != milestone.HarvestCount {
			continue
		}
		notices = append(notices, farm.CodexRewardNotice{
			CropID: progress.CropID, Tier: milestone.Tier,
			Target: milestone.HarvestCount, RewardCoin: milestone.RewardCoin,
		})
	}
	return notices
}

// PreviewCodexRewardNotices returns the deterministic response notices while
// the bundled journal projector creates the idempotent mail rows later.
func PreviewCodexRewardNotices(progress farm.CodexProgress) []farm.CodexRewardNotice {
	return eligibleCodexRewardNotices(progress)
}

func (journal *FarmWriteJournal) AppendOutboxAck(ctx context.Context, eventID string) error {
	ownerUID, err := ownerUIDFromOutboxEventID(eventID)
	if err != nil {
		return err
	}
	record := writeJournalRecord{
		Version: writeJournalVersion,
		Kind:    writeJournalOutboxAck,
		UID:     strconv.FormatUint(ownerUID, 10),
		Ack:     &journalOutboxAck{EventID: eventID},
	}
	record.EventID = deterministicJournalID(writeJournalOutboxAck, []byte(eventID))
	return journal.appendRecord(ctx, ownerUID, record, false, false)
}

// AppendOutboxAcks writes a group of independent ACK records with one Redis
// pipeline and one optional WAIT barrier. Outbox recovery makes the operation
// safe to retry or lose during process shutdown.
func (journal *FarmWriteJournal) AppendOutboxAcks(ctx context.Context, eventIDs []string) error {
	if err := journal.accepting(); err != nil {
		return err
	}
	if len(eventIDs) == 0 {
		return nil
	}
	type encodedAck struct {
		ownerUID uint64
		record   writeJournalRecord
		body     []byte
	}
	encoded := make([]encodedAck, 0, len(eventIDs))
	seen := make(map[string]struct{}, len(eventIDs))
	for _, eventID := range eventIDs {
		if _, duplicate := seen[eventID]; duplicate {
			continue
		}
		seen[eventID] = struct{}{}
		ownerUID, err := ownerUIDFromOutboxEventID(eventID)
		if err != nil {
			return err
		}
		record := writeJournalRecord{
			Version: writeJournalVersion,
			Kind:    writeJournalOutboxAck,
			UID:     strconv.FormatUint(ownerUID, 10),
			Ack:     &journalOutboxAck{EventID: eventID},
		}
		record.EventID = deterministicJournalID(writeJournalOutboxAck, []byte(eventID))
		body, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("store: encode outbox ack journal record: %w", err)
		}
		encoded = append(encoded, encodedAck{ownerUID: ownerUID, record: record, body: body})
	}
	if len(encoded) == 0 {
		return nil
	}
	started := time.Now()
	connection := journal.appendRDB.Conn()
	defer connection.Close()
	pipe := connection.Pipeline()
	for _, item := range encoded {
		shard := journal.shard(item.ownerUID)
		appendWriteJournalScript.Eval(ctx, pipe, []string{
			journal.streamKey(shard), journal.latestKey(shard, item.ownerUID), journal.pendingUIDKey(shard, item.ownerUID),
		}, item.record.EventID, item.record.Kind, item.record.UID, item.record.FarmSeq,
			item.body, "0", "0", journal.config.LatestTTL.Milliseconds())
	}
	_, err := pipe.Exec(ctx)
	journal.observeAppend(started, len(encoded), err)
	if err != nil {
		return fmt.Errorf("store: append outbox ack batch: %w", err)
	}
	return journal.waitForReplicas(ctx, connection)
}

func (journal *FarmWriteJournal) WaitUIDProjected(ctx context.Context, uid uint64, reasons ...string) (err error) {
	if uid == 0 {
		return errors.New("store: invalid write journal barrier UID")
	}
	reason := "other"
	if len(reasons) != 0 {
		reason = reasons[0]
	}
	started := time.Now()
	journal.barrierWaiters.Add(1)
	if journal.metrics != nil {
		journal.metrics.AddWriteJournalBarrierWaiter(reason, 1)
	}
	journal.adjustProjectionLimit()
	defer func() {
		journal.barrierWaiters.Add(-1)
		journal.adjustProjectionLimit()
		if journal.metrics != nil {
			journal.metrics.AddWriteJournalBarrierWaiter(reason, -1)
			journal.metrics.ObserveWriteJournalBarrier(reason, time.Since(started), err)
		}
	}()
	if journal.targetedReady.Load() {
		var cutoff string
		cutoff, err = journal.projectionCutoff(ctx, uid)
		if err != nil {
			return err
		}
		if cutoff == "" {
			if journal.metrics != nil {
				journal.metrics.ObserveWriteJournalBarrierFastPath(reason)
			}
			return nil
		}
		projectionStarted := time.Now()
		projectionErr := journal.projectUIDThrough(ctx, uid, cutoff)
		if journal.metrics != nil {
			journal.metrics.ObserveWriteJournalTargetedProjection(
				reason, time.Since(projectionStarted), projectionErr,
			)
		}
		if projectionErr == nil {
			return nil
		}
		// A transient targeted projection failure does not violate consistency:
		// append a recovery-only barrier and wait for the ordinary projector
		// instead of failing a recoverable claim.
	}
	record := writeJournalRecord{
		Version: writeJournalVersion,
		EventID: uniqueJournalID(writeJournalBarrier, uid),
		Kind:    writeJournalBarrier,
		UID:     strconv.FormatUint(uid, 10),
	}
	done := make(chan struct{})
	journal.barrierMu.Lock()
	journal.barriers[record.EventID] = done
	journal.barrierMu.Unlock()
	remove := func() {
		journal.barrierMu.Lock()
		delete(journal.barriers, record.EventID)
		journal.barrierMu.Unlock()
	}
	if err = journal.appendRecord(ctx, uid, record, false, false); err != nil {
		remove()
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		remove()
		err = fmt.Errorf("store: wait for UID %d projection: %w", uid, ctx.Err())
		return err
	case <-journal.ctx.Done():
		remove()
		err = errors.New("store: write journal stopped while waiting for projection")
		return err
	}
}

// projectionCutoff snapshots the last already-appended record for one UID.
// Redis executes this read atomically relative to the append Lua scripts: a
// concurrent mutation is therefore either included in cutoff or ordered after
// the Claim. The common successful path needs no synthetic Stream record.
func (journal *FarmWriteJournal) projectionCutoff(ctx context.Context, uid uint64) (string, error) {
	if err := journal.accepting(); err != nil {
		return "", err
	}
	shard := journal.shard(uid)
	cutoff, err := journal.lookupRDB.LIndex(ctx, journal.pendingUIDKey(shard, uid), -1).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: load UID %d projection cutoff: %w", uid, err)
	}
	return cutoff, nil
}

// projectUIDThrough materializes only one UID's records through cutoff. It can
// overtake unrelated UIDs in the shared shard while retaining the exact Redis
// append order for the claimed UID. The ordinary projector may have already
// read the same records; all projections are replay-safe and the stream ACK is
// idempotent, while farm_seq prevents an old player snapshot from overwriting
// a direct reward transaction.
func (journal *FarmWriteJournal) projectUIDThrough(ctx context.Context, uid uint64, cutoff string) error {
	select {
	case journal.targetedPermits <- struct{}{}:
		defer func() { <-journal.targetedPermits }()
	case <-ctx.Done():
		return ctx.Err()
	case <-journal.ctx.Done():
		return errors.New("store: write journal stopped during targeted projection")
	}
	messages, err := journal.pendingUIDMessagesThrough(ctx, uid, cutoff)
	if err != nil || len(messages) == 0 {
		return err
	}
	return journal.processMessages(ctx, journal.shard(uid), messages)
}

func (journal *FarmWriteJournal) pendingUIDMessagesThrough(
	ctx context.Context,
	uid uint64,
	cutoff string,
) ([]redis.XMessage, error) {
	shard := journal.shard(uid)
	ids, err := journal.lookupRDB.LRange(ctx, journal.pendingUIDKey(shard, uid), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("store: load UID %d pending projection index: %w", uid, err)
	}
	cutoffIndex := -1
	for index, id := range ids {
		if id == cutoff {
			cutoffIndex = index
			break
		}
	}
	if cutoffIndex < 0 {
		// The background projector removes an index entry only after its MySQL
		// materialization committed, so a missing cutoff is already satisfied.
		return nil, nil
	}
	ids = ids[:cutoffIndex+1]
	pipe := journal.lookupRDB.Pipeline()
	commands := make([]*redis.XMessageSliceCmd, 0, len(ids))
	for _, id := range ids {
		commands = append(commands, pipe.XRangeN(ctx, journal.streamKey(shard), id, id, 1))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("store: load UID %d pending projection records: %w", uid, err)
	}
	messages := make([]redis.XMessage, 0, len(commands))
	for _, command := range commands {
		entries, commandErr := command.Result()
		if commandErr != nil && !errors.Is(commandErr, redis.Nil) {
			return nil, fmt.Errorf("store: load UID %d projection record: %w", uid, commandErr)
		}
		if len(entries) != 0 {
			messages = append(messages, entries[0])
		}
	}
	if len(messages) == 0 {
		return nil, nil
	}
	return messages, nil
}

// projectTaskThrough applies only task progress events for one task. It leaves
// the Redis stream and pending UID index untouched; the ordinary projector
// later materializes and acknowledges the complete Farm records. Task stream
// high-water columns make both projections idempotent when they overlap.
func (journal *FarmWriteJournal) projectTaskThrough(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	taskID uint32,
) error {
	cutoff, err := journal.projectionCutoff(ctx, uid)
	if err != nil || cutoff == "" {
		return err
	}
	select {
	case journal.targetedPermits <- struct{}{}:
		defer func() { <-journal.targetedPermits }()
	case <-ctx.Done():
		return ctx.Err()
	case <-journal.ctx.Done():
		return errors.New("store: write journal stopped during targeted task projection")
	}
	messages, err := journal.pendingUIDMessagesThrough(ctx, uid, cutoff)
	if err != nil || len(messages) == 0 {
		return err
	}
	events := make([]journalTaskProjection, 0, len(messages))
	for _, message := range messages {
		record, decodeErr := recordFromXMessage(message)
		if decodeErr != nil {
			return decodeErr
		}
		streamMS, streamSeq, parseErr := parseJournalStreamID(message.ID)
		if parseErr != nil {
			return parseErr
		}
		appendAdvance := func(advance *farmv1.FarmWriteTaskAdvance) {
			if advance != nil && advance.DayKey == dayKey && advance.TaskId == taskID && advance.Amount > 0 {
				events = append(events, journalTaskProjection{
					uid: uid, dayKey: dayKey, taskID: taskID, amount: advance.Amount,
					streamMS: streamMS, streamSeq: streamSeq,
				})
			}
		}
		switch record.Kind {
		case writeJournalTaskAdvance:
			if record.Task != nil && record.Task.DayKey == dayKey && record.Task.TaskID == taskID && record.Task.Amount > 0 {
				events = append(events, journalTaskProjection{
					uid: uid, dayKey: dayKey, taskID: taskID, amount: record.Task.Amount,
					streamMS: streamMS, streamSeq: streamSeq,
				})
			}
		case writeJournalFarmCommit:
			if record.Commit != nil && record.Commit.Mutation != nil {
				for _, advance := range record.Commit.Mutation.TaskAdvances {
					appendAdvance(advance)
				}
			}
		}
	}
	if len(events) == 0 {
		return nil
	}
	_, err = journal.base.materializeTaskJournal(ctx, events)
	return err
}

func ownerUIDFromOutboxEventID(eventID string) (uint64, error) {
	parts := strings.Split(eventID, ":")
	if len(parts) != 4 || parts[0] != string(outbox.KindCrossResult) {
		return 0, fmt.Errorf("store: invalid journal outbox event ID %q", eventID)
	}
	uid, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || uid == 0 {
		return 0, fmt.Errorf("store: invalid journal outbox owner in %q", eventID)
	}
	return uid, nil
}

func (journal *FarmWriteJournal) appendRecord(
	ctx context.Context,
	uid uint64,
	record writeJournalRecord,
	latest, trackPending bool,
) error {
	if err := journal.accepting(); err != nil {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("store: encode %s journal record: %w", record.Kind, err)
	}
	shard := journal.shard(uid)
	started := time.Now()
	connection := journal.appendRDB.Conn()
	defer connection.Close()
	_, err = appendWriteJournalScript.Run(ctx, connection, []string{
		journal.streamKey(shard), journal.latestKey(shard, uid), journal.pendingUIDKey(shard, uid),
	}, record.EventID, record.Kind, record.UID, record.FarmSeq, body,
		boolJournalArg(latest), boolJournalArg(trackPending), journal.config.LatestTTL.Milliseconds()).Result()
	journal.observeAppend(started, 1, err)
	if err != nil {
		return fmt.Errorf("store: append %s write journal: %w", record.Kind, err)
	}
	return journal.waitForReplicas(ctx, connection)
}

func boolJournalArg(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func (journal *FarmWriteJournal) waitForReplicas(ctx context.Context, connection *redis.Conn) error {
	if journal.config.ReplicaAcks <= 0 {
		return nil
	}
	acknowledged, err := connection.Wait(ctx, journal.config.ReplicaAcks, journal.config.ReplicaTimeout).Result()
	if err != nil {
		return fmt.Errorf("store: wait for write journal replicas: %w", err)
	}
	if int(acknowledged) < journal.config.ReplicaAcks {
		return fmt.Errorf("store: write journal replicas acknowledged %d, want %d", acknowledged, journal.config.ReplicaAcks)
	}
	return nil
}

func (journal *FarmWriteJournal) accepting() error {
	if journal == nil || !journal.started.Load() || journal.closed.Load() {
		return errors.New("store: write journal is not accepting events")
	}
	return nil
}

func (journal *FarmWriteJournal) hasLatestFarm(ctx context.Context, uid uint64) (bool, error) {
	if journal == nil || journal.lookupRDB == nil || uid == 0 {
		return false, nil
	}
	exists, err := journal.lookupRDB.Exists(ctx, journal.latestKey(journal.shard(uid), uid)).Result()
	if err != nil {
		return false, fmt.Errorf("store: check latest journal farm: %w", err)
	}
	return exists > 0, nil
}

func (journal *FarmWriteJournal) latestFarmUIDs(ctx context.Context, uids []uint64) ([]uint64, error) {
	if journal == nil || journal.lookupRDB == nil || len(uids) == 0 {
		return nil, nil
	}
	unique := make([]uint64, 0, len(uids))
	keys := make([]string, 0, len(uids))
	seen := make(map[uint64]struct{}, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			continue
		}
		if _, exists := seen[uid]; exists {
			continue
		}
		seen[uid] = struct{}{}
		unique = append(unique, uid)
		keys = append(keys, journal.latestKey(journal.shard(uid), uid))
	}
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := journal.lookupRDB.Pipeline()
	commands := make([]*redis.BoolCmd, 0, len(keys))
	for _, key := range keys {
		commands = append(commands, pipe.HExists(ctx, key, "event_id"))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("store: check latest journal farms: %w", err)
	}
	pending := make([]uint64, 0)
	for index := range commands {
		exists, err := commands[index].Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("store: check latest journal farm: %w", err)
		}
		if exists {
			pending = append(pending, unique[index])
		}
	}
	return pending, nil
}

func (journal *FarmWriteJournal) runProjector(shard int) {
	defer journal.wg.Done()
	retry := journal.config.RetryMin
	var nextPendingClaim time.Time
	for {
		if journal.ctx.Err() != nil {
			return
		}
		var messages []redis.XMessage
		var err error
		now := time.Now()
		if !now.Before(nextPendingClaim) {
			messages, err = journal.claimPending(shard)
			// XAUTOCLAIM is a recovery scan, not a prerequisite for every normal
			// XREADGROUP. Polling it on every projected batch used to add one Redis
			// round trip per tiny MySQL transaction under sustained load.
			if err == nil && int64(len(messages)) < journal.config.BatchSize {
				claimPoll := max(journal.config.ClaimIdle/2, projectionClaimPollMin)
				nextPendingClaim = now.Add(claimPoll)
			}
		}
		if err == nil && len(messages) == 0 {
			messages, err = journal.readNew(shard)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, redis.Nil) || journal.ctx.Err() != nil {
				if journal.ctx.Err() != nil {
					return
				}
			}
			journal.observeProjectionError()
			journal.sleepRetry(retry)
			retry = min(retry*2, journal.config.RetryMax)
			continue
		}
		if len(messages) == 0 {
			continue
		}
		journal.markProjectionBacklog(len(messages))
		journal.adjustProjectionLimit()
		if !journal.projectLimiter.Acquire(journal.ctx) {
			return
		}
		if int64(len(messages)) < journal.config.BatchSize {
			timer := time.NewTimer(projectionCoalesceWindow)
			select {
			case <-timer.C:
			case <-journal.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				journal.projectLimiter.Release()
				return
			}
		}
		// A shard may have waited behind another projector after its first small
		// XREADGROUP. Pull everything that accumulated meanwhile without blocking
		// so MySQL receives useful batches instead of many tiny transactions.
		if remaining := journal.config.BatchSize - int64(len(messages)); remaining > 0 {
			additional, readErr := journal.readNewAvailable(shard, remaining)
			if readErr != nil {
				journal.projectLimiter.Release()
				journal.observeProjectionError()
				journal.sleepRetry(retry)
				retry = min(retry*2, journal.config.RetryMax)
				continue
			}
			messages = append(messages, additional...)
		}
		if journal.metrics != nil {
			journal.metrics.AddWriteJournalProjectionActive(1)
		}
		if err := journal.processMessages(journal.ctx, shard, messages); err != nil {
			if journal.metrics != nil {
				journal.metrics.AddWriteJournalProjectionActive(-1)
			}
			journal.projectLimiter.Release()
			journal.observeProjectionError()
			telemetry.L().Error("write journal projection failed",
				"component", "write_journal", "shard", shard, "err", err.Error())
			journal.sleepRetry(retry)
			retry = min(retry*2, journal.config.RetryMax)
			continue
		}
		if journal.metrics != nil {
			journal.metrics.AddWriteJournalProjectionActive(-1)
		}
		journal.projectLimiter.Release()
		retry = journal.config.RetryMin
	}
}

func (journal *FarmWriteJournal) adjustProjectionLimit() {
	if journal == nil || journal.projectLimiter == nil {
		return
	}
	// Keep the configured projector width even while foreground writes are in
	// flight. Narrowing here was the main reason 20 journal shards ran with
	// ~6 active projectors under 1U mixed load and lost the race against lag.
	limit := journal.config.Projectors
	changed := journal.projectLimiter.SetLimit(limit)
	if changed && journal.metrics != nil {
		journal.metrics.SetWriteJournalProjectionLimit(limit)
	}
}

func (journal *FarmWriteJournal) markProjectionBacklog(records int) {
	if journal == nil || journal.config.BatchSize <= 0 || int64(records) < journal.config.BatchSize {
		return
	}
	for {
		current := journal.projectionBacklog.Load()
		if current >= int64(records) || journal.projectionBacklog.CompareAndSwap(current, int64(records)) {
			return
		}
	}
}

type adaptiveProjectionLimiter struct {
	mu     sync.Mutex
	active int
	limit  int
	wake   chan struct{}
}

func newAdaptiveProjectionLimiter(limit int) *adaptiveProjectionLimiter {
	return &adaptiveProjectionLimiter{limit: max(limit, 1), wake: make(chan struct{})}
}

func (limiter *adaptiveProjectionLimiter) Acquire(ctx context.Context) bool {
	for {
		limiter.mu.Lock()
		if limiter.active < limiter.limit {
			limiter.active++
			limiter.mu.Unlock()
			return true
		}
		wake := limiter.wake
		limiter.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return false
		}
	}
}

func (limiter *adaptiveProjectionLimiter) Release() {
	limiter.mu.Lock()
	if limiter.active > 0 {
		limiter.active--
	}
	close(limiter.wake)
	limiter.wake = make(chan struct{})
	limiter.mu.Unlock()
}

func (limiter *adaptiveProjectionLimiter) SetLimit(limit int) bool {
	limiter.mu.Lock()
	changed := false
	limit = max(limit, 1)
	if limiter.limit != limit {
		limiter.limit = limit
		changed = true
		close(limiter.wake)
		limiter.wake = make(chan struct{})
	}
	limiter.mu.Unlock()
	return changed
}

func (limiter *adaptiveProjectionLimiter) Limit() int {
	if limiter == nil {
		return 1
	}
	limiter.mu.Lock()
	limit := limiter.limit
	limiter.mu.Unlock()
	return limit
}

func (journal *FarmWriteJournal) claimPending(shard int) ([]redis.XMessage, error) {
	messages, _, err := journal.rdb.XAutoClaim(journal.ctx, &redis.XAutoClaimArgs{
		Stream: journal.streamKey(shard), Group: journal.groupName(),
		Consumer: journal.consumerName(shard), MinIdle: journal.config.ClaimIdle,
		Start: "0-0", Count: journal.config.BatchSize,
	}).Result()
	return messages, err
}

func (journal *FarmWriteJournal) readNew(shard int) ([]redis.XMessage, error) {
	streams, err := journal.rdb.XReadGroup(journal.ctx, &redis.XReadGroupArgs{
		Group: journal.groupName(), Consumer: journal.consumerName(shard),
		Streams: []string{journal.streamKey(shard), ">"},
		Count:   journal.config.BatchSize, Block: journal.config.Block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil || len(streams) == 0 {
		return nil, err
	}
	return streams[0].Messages, nil
}

func (journal *FarmWriteJournal) readNewAvailable(shard int, count int64) ([]redis.XMessage, error) {
	if count <= 0 {
		return nil, nil
	}
	streams, err := journal.rdb.XReadGroup(journal.ctx, &redis.XReadGroupArgs{
		Group: journal.groupName(), Consumer: journal.consumerName(shard),
		Streams: []string{journal.streamKey(shard), ">"},
		Count:   count, Block: -1,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil || len(streams) == 0 {
		return nil, err
	}
	return streams[0].Messages, nil
}

func (journal *FarmWriteJournal) processMessages(ctx context.Context, shard int, messages []redis.XMessage) error {
	records := make([]writeJournalRecord, len(messages))
	for index := range messages {
		record, err := recordFromXMessage(messages[index])
		if err != nil {
			return err
		}
		records[index] = record
	}
	for index := 0; index < len(messages); {
		end := index
		if records[index].Kind == writeJournalBarrier {
			for end < len(messages) && records[end].Kind == writeJournalBarrier {
				end++
			}
		} else {
			// A barrier is an ordering fence: everything before it must reach MySQL
			// before the waiter is released. Inside a barrier-free segment, outbox
			// ACKs are independent and may safely move behind state materialization.
			for end < len(messages) && records[end].Kind != writeJournalBarrier {
				end++
			}
		}
		if err := journal.processProjectionSegment(ctx, shard, messages[index:end], records[index:end]); err != nil {
			return err
		}
		index = end
	}
	return nil
}

type projectionMessageGroup struct {
	kind     string
	messages []redis.XMessage
	records  []writeJournalRecord
}

// projectionGroups removes outbox ACKs from the state-record sequence and
// appends them as one final group. ACKs are emitted only after delivery, so
// moving them behind state projection is causally safe. This also joins Farm
// mutations that ACK traffic previously split into many one-row MySQL
// transactions while retaining the relative order of Farm/task/codex records.
func projectionGroups(messages []redis.XMessage, records []writeJournalRecord) []projectionMessageGroup {
	groups := make([]projectionMessageGroup, 0, 5)
	ackMessages := make([]redis.XMessage, 0)
	ackRecords := make([]writeJournalRecord, 0)
	appendRecord := func(message redis.XMessage, record writeJournalRecord) {
		if record.Kind == writeJournalOutboxAck {
			ackMessages = append(ackMessages, message)
			ackRecords = append(ackRecords, record)
			return
		}
		if len(groups) == 0 || groups[len(groups)-1].kind != record.Kind {
			groups = append(groups, projectionMessageGroup{kind: record.Kind})
		}
		group := &groups[len(groups)-1]
		group.messages = append(group.messages, message)
		group.records = append(group.records, record)
	}
	for index := range records {
		if index >= len(messages) {
			break
		}
		appendRecord(messages[index], records[index])
	}
	if len(ackRecords) != 0 {
		groups = append(groups, projectionMessageGroup{
			kind: writeJournalOutboxAck, messages: ackMessages, records: ackRecords,
		})
	}
	return groups
}

func (journal *FarmWriteJournal) processProjectionSegment(
	ctx context.Context,
	shard int,
	messages []redis.XMessage,
	records []writeJournalRecord,
) error {
	for _, group := range projectionGroups(messages, records) {
		started := time.Now()
		var err error
		switch group.kind {
		case writeJournalFarmCommit:
			err = journal.materializeFarmGroup(ctx, shard, group.messages, group.records)
		case writeJournalTaskAdvance:
			err = journal.materializeTaskGroup(ctx, group.messages, group.records)
		case writeJournalCodexReward:
			err = journal.materializeCodexGroup(ctx, group.records)
		case writeJournalOutboxAck:
			err = journal.materializeOutboxAckGroup(ctx, group.records)
		case writeJournalBarrier:
			err = journal.materializeBarrierGroup(group.records)
		default:
			err = fmt.Errorf("store: unsupported write journal kind %q", group.kind)
		}
		journal.observeProjection(started, len(group.messages), err)
		if err != nil {
			return err
		}
	}
	return journal.acknowledgeProjectedMessages(ctx, shard, messages, records)
}

func (journal *FarmWriteJournal) acknowledgeProjectedMessages(
	ctx context.Context,
	shard int,
	messages []redis.XMessage,
	records []writeJournalRecord,
) error {
	ids := make([]string, len(messages))
	for index := range messages {
		ids[index] = messages[index].ID
	}
	// The stream is a recovery log, not long-term analytics storage. One Lua
	// call preserves ACK-before-delete semantics while removing an extra RTT.
	args := make([]any, 0, len(ids)+1)
	args = append(args, journal.groupName())
	keys := make([]string, 0, len(ids)+1)
	keys = append(keys, journal.streamKey(shard))
	for _, id := range ids {
		args = append(args, id)
	}
	for _, record := range records {
		uid, parseErr := strconv.ParseUint(record.UID, 10, 64)
		if parseErr != nil || uid == 0 {
			return errors.New("store: invalid journal UID while acknowledging projection")
		}
		keys = append(keys, journal.pendingUIDKey(shard, uid))
	}
	if err := acknowledgeWriteJournalScript.Run(ctx, journal.rdb, keys, args...).Err(); err != nil {
		return fmt.Errorf("store: acknowledge and trim write journal: %w", err)
	}
	return nil
}

func (journal *FarmWriteJournal) materializeFarmGroup(
	parent context.Context,
	shard int,
	messages []redis.XMessage,
	records []writeJournalRecord,
) error {
	for index := range records {
		record := &records[index]
		var err error
		if record.Commit != nil && record.Commit.Mutation == nil && record.Commit.Snapshot != nil {
			record.Commit.Mutation, err = outbox.NewFarmWriteMutation(
				record.Commit.Snapshot, outbox.PersistPlan{Mode: outbox.PersistFull}, nil, nil, nil,
				record.Commit.Outbox, record.Commit.TaskAdvances, record.Commit.CodexRewards,
				record.Commit.TaskClaims, record.Commit.MailMutations,
			)
		}
		if err != nil || record.Commit == nil || record.Commit.Mutation == nil {
			return fmt.Errorf("store: decode farm journal record: %w", err)
		}
	}
	commits, latestIDs := coalesceJournalFarmCommits(records)
	ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
	defer cancel()
	if err := journal.base.MaterializeFarmCommits(ctx, commits); err != nil {
		return err
	}
	if err := journal.materializeBundledSideEffects(parent, records, messages); err != nil {
		return err
	}
	pipe := journal.rdb.Pipeline()
	for uid, eventID := range latestIDs {
		deleteLatestJournalScript.Eval(ctx, pipe,
			[]string{journal.latestKey(shard, uid)}, eventID)
	}
	_, _ = pipe.Exec(ctx)
	return nil
}

func (journal *FarmWriteJournal) materializeBundledSideEffects(
	parent context.Context,
	records []writeJournalRecord,
	messages []redis.XMessage,
) error {
	tasks := make([]journalTaskProjection, 0)
	claims := make([]journalTaskClaimProjection, 0)
	mailMutations := make([]journalMailMutationProjection, 0)
	for index, record := range records {
		if record.Commit == nil || record.Commit.Mutation == nil || index >= len(messages) {
			continue
		}
		uid := record.Commit.Mutation.Uid
		streamMS, streamSeq, err := parseJournalStreamID(messages[index].ID)
		if err != nil {
			return err
		}
		for _, advance := range record.Commit.Mutation.TaskAdvances {
			if advance.DayKey == 0 || advance.TaskId == 0 || advance.Amount == 0 {
				return errors.New("store: invalid bundled task advancement")
			}
			tasks = append(tasks, journalTaskProjection{
				uid: uid, dayKey: advance.DayKey, taskID: advance.TaskId, amount: advance.Amount,
				streamMS: streamMS, streamSeq: streamSeq,
			})
		}
		for _, claim := range record.Commit.Mutation.TaskClaims {
			if claim.DayKey == 0 || claim.TaskId == 0 || claim.ClaimedAt <= 0 {
				return errors.New("store: invalid bundled task claim")
			}
			claims = append(claims, journalTaskClaimProjection{
				uid: uid, dayKey: claim.DayKey, taskID: claim.TaskId, claimedAt: claim.ClaimedAt,
				streamMS: streamMS, streamSeq: streamSeq,
			})
		}
		for _, mutation := range record.Commit.Mutation.MailMutations {
			if mutation.MailId == 0 || mutation.OccurredAt <= 0 ||
				mutation.Kind == farmv1.FarmWriteMailMutationKind_FARM_WRITE_MAIL_MUTATION_KIND_UNSPECIFIED {
				return errors.New("store: invalid bundled mail mutation")
			}
			mailMutations = append(mailMutations, journalMailMutationProjection{
				uid: uid, mailID: mutation.MailId, kind: mutation.Kind, occurredAt: mutation.OccurredAt,
				streamMS: streamMS, streamSeq: streamSeq,
			})
		}
	}
	if len(claims) > 0 {
		ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
		err := journal.base.materializeTaskClaims(ctx, claims)
		cancel()
		if err != nil {
			return err
		}
	}
	if len(mailMutations) > 0 {
		ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
		err := journal.base.materializeMailMutations(ctx, mailMutations)
		cancel()
		if err != nil {
			return err
		}
	}
	if len(tasks) > 0 {
		ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
		results, err := journal.base.materializeTaskJournal(ctx, tasks)
		cancel()
		if err != nil {
			return err
		}
		for key, result := range results {
			if result.Changed {
				journal.notifyTask(key.uid, result.Task)
			}
		}
	}
	for _, record := range records {
		if record.Commit == nil || record.Commit.Mutation == nil {
			continue
		}
		uid := record.Commit.Mutation.Uid
		for _, reward := range record.Commit.Mutation.CodexRewards {
			ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
			issued, err := journal.base.IssueCodexRewards(ctx, uid, farm.CodexProgress{
				CropID: uint16(reward.CropId), HarvestCount: reward.HarvestCount,
			})
			cancel()
			if err != nil {
				return err
			}
			if len(issued) > 0 {
				journal.notifyMail(uid)
			}
		}
	}
	return nil
}

func coalesceJournalFarmCommits(records []writeJournalRecord) ([]outbox.FarmCommit, map[uint64]string) {
	hasMutation := false
	for _, record := range records {
		if record.Commit != nil && record.Commit.Mutation != nil {
			hasMutation = true
			break
		}
	}
	if !hasMutation {
		return coalesceLegacyJournalFarmCommits(records)
	}
	order := make([]uint64, 0, len(records))
	byUID := make(map[uint64]outbox.FarmCommit, len(records))
	latestIDs := make(map[uint64]string, len(records))
	for _, record := range records {
		if record.Commit == nil || record.Commit.Mutation == nil {
			continue
		}
		uid := record.Commit.Mutation.Uid
		// Decoded journal messages are exclusively owned by this projection. Keep
		// the first mutation for a UID directly and clone only when two records for
		// the same UID actually need merging. With a large account pool, duplicates
		// inside one batch are rare, so the old unconditional clone was pure CPU and
		// allocation overhead on the projector hot path.
		incoming := outbox.FarmCommit{Mutation: record.Commit.Mutation}
		if current, ok := byUID[uid]; ok {
			incoming.Mutation = mergeJournalFarmMutation(current.Mutation, incoming.Mutation)
		} else {
			order = append(order, uid)
		}
		byUID[uid] = incoming
		latestIDs[uid] = record.EventID
	}
	commits := make([]outbox.FarmCommit, 0, len(order))
	for _, uid := range order {
		commits = append(commits, byUID[uid])
	}
	return commits, latestIDs
}

func coalesceLegacyJournalFarmCommits(records []writeJournalRecord) ([]outbox.FarmCommit, map[uint64]string) {
	order := make([]uint64, 0, len(records))
	byUID := make(map[uint64]outbox.FarmCommit, len(records))
	latestIDs := make(map[uint64]string, len(records))
	for _, record := range records {
		if record.Commit == nil || record.Commit.Snapshot == nil {
			continue
		}
		uid := record.Commit.Snapshot.UID
		incoming := *record.Commit
		if current, ok := byUID[uid]; ok {
			incoming.Plan = mergeJournalPersistPlan(current.Plan, incoming.Plan)
			incoming.Outbox = mergeJournalOutboxEvents(current.Outbox, incoming.Outbox)
		} else {
			order = append(order, uid)
		}
		byUID[uid] = incoming
		latestIDs[uid] = record.EventID
	}
	commits := make([]outbox.FarmCommit, 0, len(order))
	for _, uid := range order {
		commits = append(commits, byUID[uid])
	}
	return commits, latestIDs
}

func mergeJournalFarmMutation(current, incoming *farmv1.FarmWriteMutation) *farmv1.FarmWriteMutation {
	if current == nil {
		return incoming
	}
	if incoming == nil {
		return current
	}
	merged := proto.Clone(incoming).(*farmv1.FarmWriteMutation)
	copyMaskedPlayerFields(merged, current, current.PlayerMask&^incoming.PlayerMask)
	merged.PlayerMask |= current.PlayerMask
	merged.Plots = mergeWritePlots(current.Plots, incoming.Plots)
	merged.Items = mergeWriteItems(current.Items, incoming.Items, incoming.ReplaceItems)
	merged.ReplaceItems = current.ReplaceItems || incoming.ReplaceItems
	merged.Codex = mergeWriteCodex(current.Codex, incoming.Codex, incoming.ReplaceCodex)
	merged.ReplaceCodex = current.ReplaceCodex || incoming.ReplaceCodex
	merged.Outbox = mergeWriteOutbox(current.Outbox, incoming.Outbox)
	return merged
}

func copyMaskedPlayerFields(target, source *farmv1.FarmWriteMutation, mask uint32) {
	if mask&outbox.PlayerIdentity != 0 {
		target.Nickname = source.Nickname
		target.UnlockedPlots = source.UnlockedPlots
	}
	if mask&outbox.PlayerEconomy != 0 {
		target.Level = source.Level
		target.Exp = source.Exp
		target.Coin = source.Coin
	}
	if mask&outbox.PlayerCodexBitmap != 0 {
		target.CodexBitmap = append([]byte(nil), source.CodexBitmap...)
	}
	if mask&outbox.PlayerDaily != 0 {
		target.DailyJson = append([]byte(nil), source.DailyJson...)
	}
	if mask&outbox.PlayerPet != 0 {
		target.PetJson = append([]byte(nil), source.PetJson...)
	}
	if mask&outbox.PlayerCrossPending != 0 {
		target.CrossPendingJson = append([]byte(nil), source.CrossPendingJson...)
	}
	if mask&outbox.PlayerCrossReceipts != 0 {
		target.CrossReceiptJson = append([]byte(nil), source.CrossReceiptJson...)
	}
}

func mergeWritePlots(existing, incoming []*farmv1.FarmWritePlot) []*farmv1.FarmWritePlot {
	byIndex := make(map[uint32]*farmv1.FarmWritePlot, len(existing)+len(incoming))
	for _, group := range [][]*farmv1.FarmWritePlot{existing, incoming} {
		for _, entry := range group {
			if entry != nil {
				byIndex[entry.Index] = entry
			}
		}
	}
	indexes := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, int(index))
	}
	sort.Ints(indexes)
	result := make([]*farmv1.FarmWritePlot, 0, len(indexes))
	for _, index := range indexes {
		result = append(result, byIndex[uint32(index)])
	}
	return result
}

func mergeWriteItems(existing, incoming []*farmv1.FarmWriteItem, incomingReplace bool) []*farmv1.FarmWriteItem {
	byKey := make(map[string]*farmv1.FarmWriteItem, len(existing)+len(incoming))
	if !incomingReplace {
		for _, entry := range existing {
			if entry != nil {
				byKey[entry.Key] = entry
			}
		}
	}
	for _, entry := range incoming {
		if entry != nil {
			byKey[entry.Key] = entry
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]*farmv1.FarmWriteItem, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result
}

func mergeWriteCodex(existing, incoming []*farmv1.FarmWriteCodex, incomingReplace bool) []*farmv1.FarmWriteCodex {
	byID := make(map[uint32]*farmv1.FarmWriteCodex, len(existing)+len(incoming))
	if !incomingReplace {
		for _, entry := range existing {
			if entry != nil {
				byID[entry.CropId] = entry
			}
		}
	}
	for _, entry := range incoming {
		if entry != nil {
			byID[entry.CropId] = entry
		}
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	result := make([]*farmv1.FarmWriteCodex, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[uint32(id)])
	}
	return result
}

func mergeWriteOutbox(existing, incoming []*farmv1.FarmWriteOutbox) []*farmv1.FarmWriteOutbox {
	byID := make(map[string]*farmv1.FarmWriteOutbox, len(existing)+len(incoming))
	order := make([]string, 0, len(existing)+len(incoming))
	for _, group := range [][]*farmv1.FarmWriteOutbox{existing, incoming} {
		for _, entry := range group {
			if entry == nil || entry.EventId == "" {
				continue
			}
			if _, ok := byID[entry.EventId]; !ok {
				order = append(order, entry.EventId)
			}
			byID[entry.EventId] = entry
		}
	}
	result := make([]*farmv1.FarmWriteOutbox, 0, len(order))
	for _, id := range order {
		result = append(result, byID[id])
	}
	return result
}

func mergeJournalOutboxEvents(existing, incoming []outbox.Event) []outbox.Event {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]outbox.Event, 0, len(existing)+len(incoming))
	for _, events := range [][]outbox.Event{existing, incoming} {
		for _, event := range events {
			if event.EventID == "" {
				continue
			}
			if _, ok := seen[event.EventID]; ok {
				continue
			}
			seen[event.EventID] = struct{}{}
			merged = append(merged, event)
		}
	}
	return merged
}

func mergeJournalPersistPlan(existing, incoming outbox.PersistPlan) outbox.PersistPlan {
	if existing.Mode == outbox.PersistFull || incoming.Mode == outbox.PersistFull {
		return outbox.PersistPlan{Mode: outbox.PersistFull}
	}
	if existing.Mode != incoming.Mode {
		return outbox.PersistPlan{Mode: outbox.PersistFull}
	}
	merged := incoming
	switch incoming.Mode {
	case outbox.PersistPlot, outbox.PersistCrossOwner:
		if existing.PlotIndex != incoming.PlotIndex {
			return outbox.PersistPlan{Mode: outbox.PersistFull}
		}
	case outbox.PersistEconomy:
		// The latest immutable snapshot contains the final economy state.
	case outbox.PersistCrossVisitor:
	default:
		return outbox.PersistPlan{Mode: outbox.PersistFull}
	}
	merged.IncludeItems = existing.IncludeItems || incoming.IncludeItems
	merged.IncludeCodex = existing.IncludeCodex || incoming.IncludeCodex
	return merged
}

func (journal *FarmWriteJournal) materializeTaskGroup(
	parent context.Context,
	messages []redis.XMessage,
	records []writeJournalRecord,
) error {
	events := make([]journalTaskProjection, 0, len(messages))
	for index, message := range messages {
		if index >= len(records) || records[index].Task == nil {
			return errors.New("store: decode task journal record: missing task")
		}
		record := records[index]
		uid, err := strconv.ParseUint(record.UID, 10, 64)
		if err != nil || uid == 0 {
			return errors.New("store: invalid task journal UID")
		}
		events = append(events, journalTaskProjection{
			uid: uid, dayKey: record.Task.DayKey,
			taskID: record.Task.TaskID, amount: record.Task.Amount,
		})
		streamMS, streamSeq, err := parseJournalStreamID(message.ID)
		if err != nil {
			return err
		}
		events[len(events)-1].streamMS = streamMS
		events[len(events)-1].streamSeq = streamSeq
	}
	ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
	results, err := journal.base.materializeTaskJournal(ctx, events)
	cancel()
	if err != nil {
		return err
	}
	for key, result := range results {
		if result.Changed {
			journal.notifyTask(key.uid, result.Task)
		}
	}
	return nil
}

type journalTaskKey struct {
	uid    uint64
	dayKey int64
	taskID uint32
}

type journalTaskProjection struct {
	uid       uint64
	dayKey    int64
	taskID    uint32
	amount    uint32
	streamMS  uint64
	streamSeq uint64
}

func parseJournalStreamID(value string) (uint64, uint64, error) {
	parts := strings.SplitN(value, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("store: invalid journal stream ID %q", value)
	}
	milliseconds, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("store: invalid journal stream milliseconds %q", value)
	}
	sequence, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("store: invalid journal stream sequence %q", value)
	}
	return milliseconds, sequence, nil
}

func journalStreamAfter(ms, sequence, previousMS, previousSequence uint64) bool {
	return ms > previousMS || ms == previousMS && sequence > previousSequence
}

// materializeTaskJournal stores one Redis Stream high-water mark per task row.
// A crash after MySQL COMMIT but before XACK therefore replays no progress,
// without growing a separate deduplication table for every gameplay action.
func (s *Store) materializeTaskJournal(
	ctx context.Context,
	events []journalTaskProjection,
) (map[journalTaskKey]TaskAdvanceResult, error) {
	// Task high-water updates are current-state projections. READ COMMITTED
	// keeps the explicit FOR UPDATE row lock while avoiding unrelated gap locks
	// when several journal shards materialize new task rows concurrently.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("store: begin task journal projection: %w", err)
	}
	defer tx.Rollback()

	order := make([]journalTaskKey, 0, len(events))
	byKey := make(map[journalTaskKey][]journalTaskProjection, len(events))
	for _, event := range events {
		key := journalTaskKey{uid: event.uid, dayKey: event.dayKey, taskID: event.taskID}
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], event)
	}
	sort.Slice(order, func(left, right int) bool {
		if order[left].uid != order[right].uid {
			return order[left].uid < order[right].uid
		}
		if order[left].dayKey != order[right].dayKey {
			return order[left].dayKey < order[right].dayKey
		}
		return order[left].taskID < order[right].taskID
	})

	type taskProjectionState struct {
		progress            uint32
		claimed             bool
		streamMS, streamSeq uint64
	}
	definitions := make(map[journalTaskKey]dailyTaskDefinition, len(order))
	selectedOrder := make([]journalTaskKey, 0, len(order))
	for _, key := range order {
		definition, selected := dailyTaskDefinitionByID(dailyTaskDefinitionsFor(key.uid, key.dayKey), key.taskID)
		if selected {
			definitions[key] = definition
			selectedOrder = append(selectedOrder, key)
		}
	}

	// Lock every existing task row in primary-key order with one round trip.
	// The old per-key SELECT + UPDATE path needed two MySQL exchanges for every
	// task touched by a journal batch; claim-driven projection amplifies that
	// cost because many UIDs arrive concurrently.
	states := make(map[journalTaskKey]taskProjectionState, len(selectedOrder))
	if len(selectedOrder) > 0 {
		marks := make([]string, len(selectedOrder))
		args := make([]any, 0, len(selectedOrder)*3)
		for index, key := range selectedOrder {
			marks[index] = "(?, ?, ?)"
			args = append(args, key.uid, key.dayKey, key.taskID)
		}
		rows, queryErr := tx.QueryContext(ctx, `
			SELECT uid, logic_day, task_id, progress, claimed_at IS NOT NULL,
				journal_stream_ms, journal_stream_seq
			FROM player_task
			WHERE (uid, logic_day, task_id) IN (`+strings.Join(marks, ",")+`)
			ORDER BY uid, logic_day, task_id
			FOR UPDATE`, args...)
		if queryErr != nil {
			return nil, fmt.Errorf("store: lock projected task batch: %w", queryErr)
		}
		for rows.Next() {
			var key journalTaskKey
			var state taskProjectionState
			if scanErr := rows.Scan(
				&key.uid, &key.dayKey, &key.taskID, &state.progress, &state.claimed,
				&state.streamMS, &state.streamSeq,
			); scanErr != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan projected task batch: %w", scanErr)
			}
			states[key] = state
		}
		if rowsErr := rows.Close(); rowsErr != nil {
			return nil, fmt.Errorf("store: close projected task batch: %w", rowsErr)
		}
	}

	type taskProjectionUpdate struct {
		key                 journalTaskKey
		definition          dailyTaskDefinition
		previous, progress  uint32
		claimed             bool
		streamMS, streamSeq uint64
	}
	results := make(map[journalTaskKey]TaskAdvanceResult, len(order))
	updates := make([]taskProjectionUpdate, 0, len(selectedOrder))
	for _, key := range selectedOrder {
		definition := definitions[key]
		state, exists := states[key]
		if !exists {
			state.progress = definition.initialProgress
		}
		var amount uint64
		latestMS, latestSequence := state.streamMS, state.streamSeq
		for _, event := range byKey[key] {
			if !journalStreamAfter(event.streamMS, event.streamSeq, state.streamMS, state.streamSeq) {
				continue
			}
			amount += uint64(event.amount)
			if journalStreamAfter(event.streamMS, event.streamSeq, latestMS, latestSequence) {
				latestMS, latestSequence = event.streamMS, event.streamSeq
			}
		}
		if amount == 0 {
			continue
		}
		newProgress := state.progress
		if !state.claimed {
			newProgress = uint32(min(uint64(definition.target), uint64(state.progress)+amount))
		}
		updates = append(updates, taskProjectionUpdate{
			key: key, definition: definition, previous: state.progress, progress: newProgress,
			claimed: state.claimed, streamMS: latestMS, streamSeq: latestSequence,
		})
		if newProgress != state.progress {
			results[key] = TaskAdvanceResult{
				Task: Task{
					ID: key.taskID, DayKey: key.dayKey, Title: definition.title,
					Progress: newProgress, Target: definition.target,
					RewardCoin: definition.rewardCoin, Claimed: state.claimed, Kind: definition.kind,
				},
				Changed: true, JustCompleted: state.progress < definition.target && newProgress == definition.target,
			}
		}
	}
	if len(updates) > 0 {
		values := make([]string, len(updates))
		args := make([]any, 0, len(updates)*8)
		for index, update := range updates {
			values[index] = "(?, ?, ?, ?, ?, ?, ?, ?)"
			args = append(args, update.key.uid, update.key.dayKey, update.key.taskID,
				update.progress, update.definition.target, update.definition.rewardCoin,
				update.streamMS, update.streamSeq)
		}
		// A missing row can still be inserted concurrently by targeted and shard
		// projection. The stream high-water predicate makes the multi-row upsert
		// safe in that race and on replay after COMMIT-before-XACK crashes.
		newer := `(VALUES(journal_stream_ms) > journal_stream_ms OR
			(VALUES(journal_stream_ms) = journal_stream_ms AND VALUES(journal_stream_seq) > journal_stream_seq))`
		query := `INSERT INTO player_task (
			uid, logic_day, task_id, progress, target, reward_coin,
			journal_stream_ms, journal_stream_seq
		) VALUES ` + strings.Join(values, ",") + `
		ON DUPLICATE KEY UPDATE
			progress = IF(` + newer + `, VALUES(progress), progress),
			target = IF(` + newer + `, VALUES(target), target),
			reward_coin = IF(` + newer + `, VALUES(reward_coin), reward_coin),
			journal_stream_ms = IF(` + newer + `, VALUES(journal_stream_ms), journal_stream_ms),
			journal_stream_seq = IF(` + newer + `, VALUES(journal_stream_seq), journal_stream_seq)`
		if _, execErr := tx.ExecContext(ctx, query, args...); execErr != nil {
			return nil, fmt.Errorf("store: materialize projected task batch: %w", execErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit task journal projection: %w", err)
	}
	for _, update := range updates {
		s.invalidateTaskCache(taskReadKey{uid: update.key.uid, dayKey: update.key.dayKey})
	}
	return results, nil
}

func (journal *FarmWriteJournal) materializeCodexGroup(parent context.Context, records []writeJournalRecord) error {
	for _, record := range records {
		if record.Codex == nil {
			return errors.New("store: decode codex journal record: missing reward")
		}
		uid, err := strconv.ParseUint(record.UID, 10, 64)
		if err != nil || uid == 0 {
			return errors.New("store: invalid codex journal UID")
		}
		ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
		issued, issueErr := journal.base.IssueCodexRewards(ctx, uid, record.Codex.Progress)
		cancel()
		if issueErr != nil {
			return issueErr
		}
		if len(issued) > 0 {
			journal.notifyMail(uid)
		}
	}
	return nil
}

func (journal *FarmWriteJournal) materializeOutboxAckGroup(parent context.Context, records []writeJournalRecord) error {
	eventIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.Ack == nil || record.Ack.EventID == "" {
			return errors.New("store: decode outbox ack journal record: missing event")
		}
		eventIDs = append(eventIDs, record.Ack.EventID)
	}
	// Gateway already appends delivered outbox acknowledgements in batches. Do
	// not explode that batch back into one MySQL UPDATE per event during
	// projection: cross-farm traffic otherwise adds roughly one extra SQL round
	// trip for every successful action and fragments the shared projector lane.
	ctx, cancel := context.WithTimeout(parent, journal.config.IOTimeout)
	err := journal.base.MarkOutboxPublishedBatch(ctx, eventIDs)
	cancel()
	if err != nil {
		return err
	}
	return nil
}

func (journal *FarmWriteJournal) materializeBarrierGroup(records []writeJournalRecord) error {
	journal.barrierMu.Lock()
	defer journal.barrierMu.Unlock()
	for _, record := range records {
		if waiter := journal.barriers[record.EventID]; waiter != nil {
			close(waiter)
			delete(journal.barriers, record.EventID)
		}
	}
	return nil
}

func (journal *FarmWriteJournal) notifyTask(uid uint64, task Task) {
	journal.observerMu.RLock()
	observer := journal.taskObserver
	journal.observerMu.RUnlock()
	if observer != nil {
		observer(uid, task)
	}
}

func (journal *FarmWriteJournal) notifyMail(uid uint64) {
	journal.observerMu.RLock()
	observer := journal.mailObserver
	journal.observerMu.RUnlock()
	if observer != nil {
		observer(uid)
	}
}

func recordFromXMessage(message redis.XMessage) (writeJournalRecord, error) {
	raw, ok := message.Values["body"]
	if !ok {
		return writeJournalRecord{}, errors.New("store: write journal message has no body")
	}
	var body []byte
	switch value := raw.(type) {
	case string:
		body = []byte(value)
	case []byte:
		body = value
	default:
		body = []byte(fmt.Sprint(value))
	}
	kind := fmt.Sprint(message.Values["kind"])
	if kind == writeJournalFarmCommit {
		mutation := &farmv1.FarmWriteMutation{}
		if err := proto.Unmarshal(body, mutation); err == nil && mutation.Uid != 0 {
			return writeJournalRecord{
				Version: writeJournalVersion,
				EventID: fmt.Sprint(message.Values["event_id"]),
				Kind:    kind,
				UID:     fmt.Sprint(message.Values["uid"]),
				FarmSeq: fmt.Sprint(message.Values["farm_seq"]),
				Commit:  &outbox.FarmCommit{Mutation: mutation},
			}, nil
		}
		// Accept an old JSON snapshot while draining a rolling deployment.
	}
	return decodeWriteJournalRecord(body)
}

func decodeWriteJournalRecord(body []byte) (writeJournalRecord, error) {
	var record writeJournalRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return writeJournalRecord{}, fmt.Errorf("store: decode write journal: %w", err)
	}
	if record.Version != 1 && record.Version != writeJournalVersion || record.EventID == "" || record.Kind == "" || record.UID == "" {
		return writeJournalRecord{}, errors.New("store: invalid write journal envelope")
	}
	return record, nil
}

func deterministicJournalID(kind string, body []byte) string {
	digest := sha256.Sum256(append(append([]byte(kind), 0), body...))
	return kind + ":" + hex.EncodeToString(digest[:16])
}

var journalEventCounter atomic.Uint64

func uniqueJournalID(kind string, uid uint64) string {
	seed := strconv.FormatUint(uid, 10) + ":" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":" +
		strconv.FormatUint(journalEventCounter.Add(1), 10)
	return deterministicJournalID(kind, []byte(seed))
}

func (journal *FarmWriteJournal) shard(uid uint64) int {
	return int(uid % uint64(journal.config.Shards))
}

func (journal *FarmWriteJournal) streamTag(shard int) string {
	return journal.config.InstanceID + "-" + strconv.Itoa(shard)
}

func (journal *FarmWriteJournal) streamKey(shard int) string {
	return FarmWriteJournalStreamKey(journal.config.Prefix, journal.config.InstanceID, shard)
}

func (journal *FarmWriteJournal) latestKey(shard int, uid uint64) string {
	return journal.config.Prefix + ":{" + journal.streamTag(shard) + "}:latest:" + strconv.FormatUint(uid, 10)
}

func (journal *FarmWriteJournal) pendingUIDKey(shard int, uid uint64) string {
	return journal.config.Prefix + ":{" + journal.streamTag(shard) + "}:pending:" + strconv.FormatUint(uid, 10)
}

// FarmWriteJournalProjectorGroup is the Redis consumer group that materializes
// the recovery log into MySQL. Farm admission samples this group directly.
const FarmWriteJournalProjectorGroup = "mysql-projector"

// FarmWriteJournalStreamKey returns the canonical stream key shared by Farm
// writers, Projectors and Farm backlog admission.
func FarmWriteJournalStreamKey(prefix, instanceID string, shard int) string {
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), ":")
	instanceID = sanitizeJournalPart(instanceID)
	return prefix + ":{" + instanceID + "-" + strconv.Itoa(shard) + "}:events"
}

func (journal *FarmWriteJournal) groupName() string { return FarmWriteJournalProjectorGroup }

func (journal *FarmWriteJournal) consumerName(shard int) string {
	return journal.consumerID + "-" + strconv.Itoa(shard)
}

func (journal *FarmWriteJournal) sleepRetry(duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-journal.ctx.Done():
	case <-timer.C:
	}
}

func (journal *FarmWriteJournal) WaitIdle(ctx context.Context) error {
	if journal == nil || !journal.started.Load() {
		return errors.New("store: write journal is not running")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		idle, err := journal.isIdle(ctx)
		if err != nil {
			return err
		}
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("store: wait for write journal drain: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// WriteJournalBacklog is a point-in-time view of records waiting for MySQL
// projection. Pending records were delivered but not acknowledged; lag records
// have not yet been delivered to a projector.
type WriteJournalBacklog struct {
	Pending int64
	Lag     int64
	Streams int
}

func (backlog WriteJournalBacklog) Total() int64 {
	if backlog.Pending < 0 {
		backlog.Pending = 0
	}
	if backlog.Lag < 0 {
		backlog.Lag = 0
	}
	if backlog.Pending > math.MaxInt64-backlog.Lag {
		return math.MaxInt64
	}
	return backlog.Pending + backlog.Lag
}

// WriteBacklog returns the exact local Farm journal pressure used by the
// foreground admission controller. It intentionally lives with the journal so
// Gateway never needs Redis Stream or MySQL projection knowledge.
func (journal *FarmWriteJournal) WriteBacklog(ctx context.Context) (WriteJournalBacklog, error) {
	if journal == nil || journal.lookupRDB == nil || !journal.started.Load() || journal.closed.Load() {
		return WriteJournalBacklog{}, errors.New("store: write journal is not running")
	}
	pipe := journal.lookupRDB.Pipeline()
	commands := make([]*redis.XInfoGroupsCmd, journal.config.Shards)
	for shard := range journal.config.Shards {
		commands[shard] = pipe.XInfoGroups(ctx, journal.streamKey(shard))
	}
	_, _ = pipe.Exec(ctx)

	var backlog WriteJournalBacklog
	for shard, command := range commands {
		groups, err := command.Result()
		if err != nil {
			return WriteJournalBacklog{}, fmt.Errorf("store: read write backlog shard %d: %w", shard, err)
		}
		found := false
		for _, group := range groups {
			if group.Name != journal.groupName() {
				continue
			}
			found = true
			lag := group.Lag
			if lag < 0 {
				length, lengthErr := journal.lookupRDB.XLen(ctx, journal.streamKey(shard)).Result()
				if lengthErr != nil {
					return WriteJournalBacklog{}, fmt.Errorf("store: read unknown write backlog shard %d: %w", shard, lengthErr)
				}
				lag = max(int64(0), length-group.Pending)
			}
			backlog.Pending += max(int64(0), group.Pending)
			backlog.Lag += max(int64(0), lag)
			backlog.Streams++
			break
		}
		if !found {
			return WriteJournalBacklog{}, fmt.Errorf("store: write journal group missing on shard %d", shard)
		}
	}
	journal.projectionBacklog.Store(backlog.Total())
	journal.adjustProjectionLimit()
	return backlog, nil
}

func (journal *FarmWriteJournal) isIdle(ctx context.Context) (bool, error) {
	for shard := 0; shard < journal.config.Shards; shard++ {
		groups, err := journal.rdb.XInfoGroups(ctx, journal.streamKey(shard)).Result()
		if err != nil {
			return false, err
		}
		found := false
		for _, group := range groups {
			if group.Name != journal.groupName() {
				continue
			}
			found = true
			if group.Pending != 0 || group.Lag > 0 {
				return false, nil
			}
			if group.Lag < 0 {
				length, lengthErr := journal.rdb.XLen(ctx, journal.streamKey(shard)).Result()
				if lengthErr != nil || length != 0 {
					return false, lengthErr
				}
			}
		}
		if !found {
			return false, errors.New("store: write journal consumer group missing")
		}
	}
	return true, nil
}

func (journal *FarmWriteJournal) Shutdown(ctx context.Context) error {
	if journal == nil || !journal.started.Load() || !journal.closed.CompareAndSwap(false, true) {
		return nil
	}
	drainErr := journal.WaitIdle(ctx)
	journal.cancel()
	done := make(chan struct{})
	go func() {
		journal.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		if drainErr == nil {
			drainErr = ctx.Err()
		}
	}
	return drainErr
}

func (journal *FarmWriteJournal) observeAppend(started time.Time, count int, err error) {
	if journal.metrics != nil {
		journal.metrics.ObserveWriteJournalAppend(time.Since(started), count, err)
	}
}

func (journal *FarmWriteJournal) observeProjection(started time.Time, count int, err error) {
	if journal.metrics != nil {
		journal.metrics.ObserveWriteJournalProjection(time.Since(started), count, err)
	}
}

func (journal *FarmWriteJournal) observeProjectionError() {
	if journal.metrics != nil {
		journal.metrics.ObserveWriteJournalProjectionError()
	}
}

var (
	_ FarmStore        = (*journalFarmStore)(nil)
	_ CodexRewardStore = (*FarmWriteJournal)(nil)
	_ OutboxStore      = (*journalOutboxStore)(nil)
)
