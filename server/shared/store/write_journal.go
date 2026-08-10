package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	defaultWriteJournalBatch  = 512
)

const projectionForegroundHold = 50 * time.Millisecond

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

type journalOutboxAck struct {
	EventID string `json:"event_id"`
}

var appendWriteJournalScript = redis.NewScript(`
local stream_id = redis.call('XADD', KEYS[1], '*',
  'event_id', ARGV[1], 'kind', ARGV[2], 'uid', ARGV[3],
  'farm_seq', ARGV[4], 'body', ARGV[5])
if ARGV[6] == '1' then
  redis.call('HSET', KEYS[2], 'event_id', ARGV[1], 'body', ARGV[5])
  redis.call('PEXPIRE', KEYS[2], ARGV[7])
end
return stream_id
`)

var deleteLatestJournalScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'event_id') == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// FarmWriteJournal is both the low-latency Redis Streams append boundary and
// the owner of ordered MySQL projector workers.
type FarmWriteJournal struct {
	base      *Store
	rdb       *redis.Client // projector/read-side pool
	appendRDB *redis.Client // foreground durable-append pool
	config    FarmWriteJournalConfig
	metrics   *telemetry.Metrics

	started atomic.Bool
	closed  atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	observerMu      sync.RWMutex
	taskObserver    func(uint64, Task)
	mailObserver    func(uint64)
	consumerID      string
	barrierMu       sync.Mutex
	barriers        map[string]chan struct{}
	projectLimiter  *adaptiveProjectionLimiter
	appendInFlight  atomic.Int32
	foregroundQueue atomic.Int64
	lastForeground  atomic.Int64
}

// OpenFarmWriteJournal opens a dedicated Redis client. redisAddr may point at
// the shared Redis in development, but production can isolate the durable log
// from cache eviction and session traffic.
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
	projectClient := redis.NewClient(&redis.Options{Addr: redisAddr, PoolSize: 32, MinIdleConns: 2})
	if err := projectClient.Ping(ctx).Err(); err != nil {
		_ = projectClient.Close()
		return nil, nil, fmt.Errorf("store: ping write journal Redis: %w", err)
	}
	appendClient := redis.NewClient(&redis.Options{Addr: redisAddr, PoolSize: 64, MinIdleConns: 8})
	if err := appendClient.Ping(ctx).Err(); err != nil {
		_ = appendClient.Close()
		_ = projectClient.Close()
		return nil, nil, fmt.Errorf("store: ping write journal append Redis: %w", err)
	}
	journal := NewFarmWriteJournal(base, projectClient, config)
	journal.appendRDB = appendClient
	closeClients := func() error {
		return errors.Join(appendClient.Close(), projectClient.Close())
	}
	return journal, closeClients, nil
}

func NewFarmWriteJournal(base *Store, client *redis.Client, config FarmWriteJournalConfig) *FarmWriteJournal {
	config = normalizeFarmWriteJournalConfig(config)
	return &FarmWriteJournal{
		base: base, rdb: client, appendRDB: client, config: config,
		// Keep rolling instances distinct. A replacement only takes over an old
		// worker's pending message after ClaimIdle instead of racing the active
		// projector and applying the same batch concurrently.
		consumerID: config.InstanceID + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
			strconv.FormatUint(journalEventCounter.Add(1), 36),
		barriers:       make(map[string]chan struct{}),
		projectLimiter: newAdaptiveProjectionLimiter(config.Projectors),
	}
}

func (journal *FarmWriteJournal) SetMetrics(metrics *telemetry.Metrics) {
	journal.metrics = metrics
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
	return &JournalDirectStore{Store: base, journal: journal}
}

type journalFarmStore struct {
	FarmStore
	journal *FarmWriteJournal
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
	if pending, err := store.journal.hasLatestFarm(ctx, uid); err != nil {
		return nil, err
	} else if pending {
		// The hot Actor owns the latest state. A cold reload first places a
		// per-UID barrier behind any unprojected mutations, then loads MySQL.
		// This replaces the former full-snapshot JSON stored in Redis.
		if err := store.journal.WaitUIDProjected(ctx, uid); err != nil {
			return nil, err
		}
	}
	return store.FarmStore.LoadFarm(ctx, uid)
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
	journal *FarmWriteJournal
}

func (store *JournalDirectStore) ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (TaskReward, error) {
	barrierCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	if err := store.journal.WaitUIDProjected(barrierCtx, uid); err != nil {
		return TaskReward{}, err
	}
	return store.Store.ClaimTask(ctx, uid, dayKey, taskID)
}

func (store *JournalDirectStore) ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (TaskReward, error) {
	barrierCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	if err := store.journal.WaitUIDProjected(barrierCtx, uid); err != nil {
		return TaskReward{}, err
	}
	return store.Store.ClaimDailyLogin(ctx, uid, dayKey)
}

func (store *JournalDirectStore) ClaimMail(ctx context.Context, uid, mailID uint64) (Mail, error) {
	barrierCtx, cancel := store.journal.directWriteContext(ctx)
	defer cancel()
	if err := store.journal.WaitUIDProjected(barrierCtx, uid); err != nil {
		return Mail{}, err
	}
	return store.Store.ClaimMail(ctx, uid, mailID)
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
	pipe := connection.Pipeline()
	for _, item := range encoded {
		uid, _ := strconv.ParseUint(item.record.UID, 10, 64)
		shard := journal.shard(uid)
		// Pipelines cannot observe NOSCRIPT until Exec, so queue EVAL directly
		// instead of Script.Run's optimistic EVALSHA fallback.
		appendWriteJournalScript.Eval(ctx, pipe, []string{
			journal.streamKey(shard), journal.latestKey(shard, uid),
		}, item.record.EventID, item.record.Kind, item.record.UID, item.record.FarmSeq,
			item.body, "1", journal.config.LatestTTL.Milliseconds())
	}
	_, err := pipe.Exec(ctx)
	journal.observeAppend(started, len(encoded), err)
	if err != nil {
		return fmt.Errorf("store: append farm write journal: %w", err)
	}
	return journal.waitForReplicas(ctx, connection)
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
	if err := journal.appendRecord(ctx, uid, record, false); err != nil {
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
	if err := journal.appendRecord(ctx, uid, record, false); err != nil {
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
	return journal.appendRecord(ctx, ownerUID, record, false)
}

func (journal *FarmWriteJournal) WaitUIDProjected(ctx context.Context, uid uint64) error {
	if uid == 0 {
		return errors.New("store: invalid write journal barrier UID")
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
	if err := journal.appendRecord(ctx, uid, record, false); err != nil {
		remove()
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		remove()
		return fmt.Errorf("store: wait for UID %d projection: %w", uid, ctx.Err())
	case <-journal.ctx.Done():
		remove()
		return errors.New("store: write journal stopped while waiting for projection")
	}
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

func (journal *FarmWriteJournal) appendRecord(ctx context.Context, uid uint64, record writeJournalRecord, latest bool) error {
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
		journal.streamKey(shard), journal.latestKey(shard, uid),
	}, record.EventID, record.Kind, record.UID, record.FarmSeq, body,
		boolJournalArg(latest), journal.config.LatestTTL.Milliseconds()).Result()
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
	if journal == nil || journal.rdb == nil || uid == 0 {
		return false, nil
	}
	exists, err := journal.rdb.Exists(ctx, journal.latestKey(journal.shard(uid), uid)).Result()
	if err != nil {
		return false, fmt.Errorf("store: check latest journal farm: %w", err)
	}
	return exists > 0, nil
}

func (journal *FarmWriteJournal) runProjector(shard int) {
	defer journal.wg.Done()
	retry := journal.config.RetryMin
	for {
		if journal.ctx.Err() != nil {
			return
		}
		messages, err := journal.claimPending(shard)
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
		journal.adjustProjectionLimit()
		if !journal.projectLimiter.Acquire(journal.ctx) {
			return
		}
		if err := journal.processMessages(shard, messages); err != nil {
			journal.projectLimiter.Release()
			journal.observeProjectionError()
			telemetry.L().Error("write journal projection failed",
				"component", "write_journal", "shard", shard, "err", err.Error())
			journal.sleepRetry(retry)
			retry = min(retry*2, journal.config.RetryMax)
			continue
		}
		journal.projectLimiter.Release()
		retry = journal.config.RetryMin
	}
}

func (journal *FarmWriteJournal) adjustProjectionLimit() {
	if journal == nil || journal.projectLimiter == nil {
		return
	}
	limit := journal.config.Projectors
	inFlight := journal.appendInFlight.Load()
	queue := journal.foregroundQueue.Load()
	now := time.Now()
	if inFlight > 0 || queue > 0 {
		// On a one-core Farm, even two concurrent MySQL projectors can steal a
		// material share of the CPU needed to acknowledge durable foreground
		// appends. Keep one projector making progress while gameplay is active.
		journal.lastForeground.Store(now.UnixNano())
		limit = 1
	} else if last := journal.lastForeground.Load(); last > 0 && now.Sub(time.Unix(0, last)) < projectionForegroundHold {
		// Group commits create very short gaps between batches. Holding the
		// reduced limit avoids oscillating 1→4→1 and starting expensive MySQL
		// work just before the next foreground batch arrives.
		limit = 1
	}
	journal.projectLimiter.SetLimit(limit)
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

func (limiter *adaptiveProjectionLimiter) SetLimit(limit int) {
	limiter.mu.Lock()
	limit = max(limit, 1)
	if limiter.limit != limit {
		limiter.limit = limit
		close(limiter.wake)
		limiter.wake = make(chan struct{})
	}
	limiter.mu.Unlock()
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

func (journal *FarmWriteJournal) processMessages(shard int, messages []redis.XMessage) error {
	for index := 0; index < len(messages); {
		record, err := recordFromXMessage(messages[index])
		if err != nil {
			return err
		}
		kind := record.Kind
		end := index + 1
		for end < len(messages) {
			next, nextErr := recordFromXMessage(messages[end])
			if nextErr != nil || next.Kind != kind {
				break
			}
			end++
		}
		group := messages[index:end]
		started := time.Now()
		switch kind {
		case writeJournalFarmCommit:
			err = journal.materializeFarmGroup(shard, group)
		case writeJournalTaskAdvance:
			err = journal.materializeTaskGroup(group)
		case writeJournalCodexReward:
			err = journal.materializeCodexGroup(group)
		case writeJournalOutboxAck:
			err = journal.materializeOutboxAckGroup(group)
		case writeJournalBarrier:
			err = journal.materializeBarrierGroup(group)
		default:
			err = fmt.Errorf("store: unsupported write journal kind %q", kind)
		}
		journal.observeProjection(started, len(group), err)
		if err != nil {
			return err
		}
		ids := make([]string, len(group))
		for offset := range group {
			ids[offset] = group[offset].ID
		}
		if err := journal.rdb.XAck(journal.ctx, journal.streamKey(shard), journal.groupName(), ids...).Err(); err != nil {
			return fmt.Errorf("store: ack write journal: %w", err)
		}
		// The stream is a recovery log, not long-term analytics storage. Once
		// MySQL has committed and the group has acknowledged, deletion is safe.
		_ = journal.rdb.XDel(journal.ctx, journal.streamKey(shard), ids...).Err()
		index = end
	}
	return nil
}

func (journal *FarmWriteJournal) materializeFarmGroup(shard int, messages []redis.XMessage) error {
	records := make([]writeJournalRecord, 0, len(messages))
	for _, message := range messages {
		record, err := recordFromXMessage(message)
		if err == nil && record.Commit != nil && record.Commit.Mutation == nil && record.Commit.Snapshot != nil {
			record.Commit.Mutation, err = outbox.NewFarmWriteMutation(
				record.Commit.Snapshot, outbox.PersistPlan{Mode: outbox.PersistFull}, nil, nil, nil,
				record.Commit.Outbox, record.Commit.TaskAdvances, record.Commit.CodexRewards,
			)
		}
		if err != nil || record.Commit == nil || record.Commit.Mutation == nil {
			return fmt.Errorf("store: decode farm journal record: %w", err)
		}
		records = append(records, record)
	}
	commits, latestIDs := coalesceJournalFarmCommits(records)
	ctx, cancel := context.WithTimeout(journal.ctx, journal.config.IOTimeout)
	defer cancel()
	if err := journal.base.MaterializeFarmCommits(ctx, commits); err != nil {
		return err
	}
	if err := journal.materializeBundledSideEffects(records, messages); err != nil {
		return err
	}
	uids := make([]uint64, 0, len(commits))
	for _, commit := range commits {
		if commit.Mutation != nil {
			uids = append(uids, commit.Mutation.Uid)
		}
	}
	if err := journal.base.invalidateFarmCacheUIDs(ctx, uids); err != nil {
		telemetry.L().Error("write journal projected cache update failed",
			"component", "write_journal", "count", len(uids), "err", err.Error())
		return fmt.Errorf("store: invalidate projected farm caches: %w", err)
	}
	pipe := journal.rdb.Pipeline()
	for uid, eventID := range latestIDs {
		deleteLatestJournalScript.Eval(ctx, pipe,
			[]string{journal.latestKey(shard, uid)}, eventID)
	}
	_, _ = pipe.Exec(ctx)
	return nil
}

func (journal *FarmWriteJournal) materializeBundledSideEffects(records []writeJournalRecord, messages []redis.XMessage) error {
	tasks := make([]journalTaskProjection, 0)
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
	}
	if len(tasks) > 0 {
		ctx, cancel := context.WithTimeout(journal.ctx, journal.config.IOTimeout)
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
			ctx, cancel := context.WithTimeout(journal.ctx, journal.config.IOTimeout)
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
		incoming := outbox.FarmCommit{Mutation: proto.Clone(record.Commit.Mutation).(*farmv1.FarmWriteMutation)}
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

func (journal *FarmWriteJournal) materializeTaskGroup(messages []redis.XMessage) error {
	events := make([]journalTaskProjection, 0, len(messages))
	for _, message := range messages {
		record, err := recordFromXMessage(message)
		if err != nil || record.Task == nil {
			return fmt.Errorf("store: decode task journal record: %w", err)
		}
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
	ctx, cancel := context.WithTimeout(journal.ctx, journal.config.IOTimeout)
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

	results := make(map[journalTaskKey]TaskAdvanceResult, len(order))
	appliedKeys := make([]journalTaskKey, 0, len(order))
	for _, key := range order {
		definition, selected := dailyTaskDefinitionByID(dailyTaskDefinitionsFor(key.uid, key.dayKey), key.taskID)
		if !selected {
			continue
		}
		currentProgress := definition.initialProgress
		var claimed bool
		var previousMS, previousSequence uint64
		exists := true
		queryErr := tx.QueryRowContext(ctx, `
			SELECT progress, claimed_at IS NOT NULL, journal_stream_ms, journal_stream_seq
			FROM player_task
			WHERE uid = ? AND logic_day = ? AND task_id = ?
			FOR UPDATE`, key.uid, key.dayKey, key.taskID,
		).Scan(&currentProgress, &claimed, &previousMS, &previousSequence)
		if errors.Is(queryErr, sql.ErrNoRows) {
			exists = false
		} else if queryErr != nil {
			return nil, fmt.Errorf("store: lock projected task %d: %w", key.taskID, queryErr)
		}

		amount := uint32(0)
		latestMS, latestSequence := previousMS, previousSequence
		for _, event := range byKey[key] {
			if !journalStreamAfter(event.streamMS, event.streamSeq, previousMS, previousSequence) {
				continue
			}
			amount += event.amount
			if journalStreamAfter(event.streamMS, event.streamSeq, latestMS, latestSequence) {
				latestMS, latestSequence = event.streamMS, event.streamSeq
			}
		}
		if amount == 0 {
			continue
		}
		newProgress := currentProgress
		if !claimed {
			newProgress = min(definition.target, currentProgress+amount)
		}
		var execErr error
		if exists {
			_, execErr = tx.ExecContext(ctx, `
				UPDATE player_task
				SET progress = ?, target = ?, reward_coin = ?,
					journal_stream_ms = ?, journal_stream_seq = ?
				WHERE uid = ? AND logic_day = ? AND task_id = ?`,
				newProgress, definition.target, definition.rewardCoin,
				latestMS, latestSequence, key.uid, key.dayKey, key.taskID,
			)
		} else {
			_, execErr = tx.ExecContext(ctx, `
				INSERT INTO player_task (
					uid, logic_day, task_id, progress, target, reward_coin,
					journal_stream_ms, journal_stream_seq
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				key.uid, key.dayKey, key.taskID, newProgress, definition.target,
				definition.rewardCoin, latestMS, latestSequence,
			)
		}
		if execErr != nil {
			return nil, fmt.Errorf("store: materialize projected task %d: %w", key.taskID, execErr)
		}
		appliedKeys = append(appliedKeys, key)
		if newProgress != currentProgress {
			results[key] = TaskAdvanceResult{
				Task: Task{
					ID: key.taskID, DayKey: key.dayKey, Title: definition.title,
					Progress: newProgress, Target: definition.target,
					RewardCoin: definition.rewardCoin, Claimed: claimed, Kind: definition.kind,
				},
				Changed: true, JustCompleted: currentProgress < definition.target && newProgress == definition.target,
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit task journal projection: %w", err)
	}
	for _, key := range appliedKeys {
		s.invalidateTaskCache(taskReadKey{uid: key.uid, dayKey: key.dayKey})
	}
	return results, nil
}

func (journal *FarmWriteJournal) materializeCodexGroup(messages []redis.XMessage) error {
	for _, message := range messages {
		record, err := recordFromXMessage(message)
		if err != nil || record.Codex == nil {
			return fmt.Errorf("store: decode codex journal record: %w", err)
		}
		uid, err := strconv.ParseUint(record.UID, 10, 64)
		if err != nil || uid == 0 {
			return errors.New("store: invalid codex journal UID")
		}
		ctx, cancel := context.WithTimeout(journal.ctx, journal.config.IOTimeout)
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

func (journal *FarmWriteJournal) materializeOutboxAckGroup(messages []redis.XMessage) error {
	for _, message := range messages {
		record, err := recordFromXMessage(message)
		if err != nil || record.Ack == nil || record.Ack.EventID == "" {
			return fmt.Errorf("store: decode outbox ack journal record: %w", err)
		}
		ctx, cancel := context.WithTimeout(journal.ctx, journal.config.IOTimeout)
		err = journal.base.MarkOutboxPublished(ctx, record.Ack.EventID)
		cancel()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (journal *FarmWriteJournal) materializeBarrierGroup(messages []redis.XMessage) error {
	journal.barrierMu.Lock()
	defer journal.barrierMu.Unlock()
	for _, message := range messages {
		record, err := recordFromXMessage(message)
		if err != nil {
			return err
		}
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
	return journal.config.Prefix + ":{" + journal.streamTag(shard) + "}:events"
}

func (journal *FarmWriteJournal) latestKey(shard int, uid uint64) string {
	return journal.config.Prefix + ":{" + journal.streamTag(shard) + "}:latest:" + strconv.FormatUint(uid, 10)
}

func (journal *FarmWriteJournal) groupName() string { return "mysql-projector" }

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
