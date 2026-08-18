package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"
)

const (
	defaultCommitterWindow = 2 * time.Millisecond
	// One millisecond is still negligible relative to the public latency SLO,
	// but packs substantially more one-UID mutations into each Redis Lua append
	// on a one-core Farm. The old 250us window emitted thousands of tiny appends
	// per second and starved the background projector that admission protects.
	defaultForegroundWindow    = time.Millisecond
	defaultCommitterMaxBatch   = 512
	defaultCommitterQueueCap   = 16384
	defaultCommitterMinBackoff = 10 * time.Millisecond
	defaultCommitterMaxBackoff = 100 * time.Millisecond
)

// BatchFarmStore 是 Committer 所需的批量落盘边界。
type BatchFarmStore interface {
	SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error
	CommitFarms(ctx context.Context, commits []outbox.FarmCommit) error
}

type foregroundPressureStore interface{ AdjustForegroundPressure(delta int) }

// CommitResult 回报一次组提交的结果。
type CommitResult struct {
	Generation uint64
	Err        error
}

// CommitterConfig 配置组提交窗口与背压。
type CommitterConfig struct {
	Window time.Duration
	// ForegroundWindow is the maximum batching delay for a request waiting on
	// the durable acknowledgement. Background write-behind keeps Window.
	ForegroundWindow time.Duration
	MaxBatch         int
	QueueCap         int
	IOTimeout        time.Duration
	MinBackoff       time.Duration
	MaxBackoff       time.Duration
}

func defaultCommitterConfig() CommitterConfig {
	return CommitterConfig{
		Window:           defaultCommitterWindow,
		ForegroundWindow: defaultForegroundWindow,
		MaxBatch:         defaultCommitterMaxBatch,
		QueueCap:         defaultCommitterQueueCap,
		IOTimeout:        DefaultIOTimeout,
		MinBackoff:       defaultCommitterMinBackoff,
		MaxBackoff:       defaultCommitterMaxBackoff,
	}
}

type commitWaiter struct {
	generation uint64
	result     chan CommitResult
}

type commitItem struct {
	uid        uint64
	generation uint64
	snapshot   *farm.Aggregate
	mutation   *farmv1.FarmWriteMutation
	outbox     []outbox.Event
	tasks      []outbox.TaskAdvance
	codex      []outbox.CodexReward
	plan       outbox.PersistPlan
	durable    bool
	waiters    []commitWaiter
}

var commitItemPool = sync.Pool{New: func() any { return &commitItem{} }}

func acquireCommitItem() *commitItem {
	return commitItemPool.Get().(*commitItem)
}

func releaseCommitItem(item *commitItem) {
	if item == nil {
		return
	}
	item.uid = 0
	item.generation = 0
	item.snapshot = nil
	item.mutation = nil
	clear(item.outbox)
	item.outbox = item.outbox[:0]
	clear(item.tasks)
	item.tasks = item.tasks[:0]
	clear(item.codex)
	item.codex = item.codex[:0]
	item.plan = outbox.PersistPlan{}
	item.durable = false
	clear(item.waiters)
	item.waiters = item.waiters[:0]
	commitItemPool.Put(item)
}

// Committer 在单 goroutine 内聚合不同 uid 的 immutable 快照并批量落盘。
type Committer struct {
	store   BatchFarmStore
	cfg     CommitterConfig
	metrics *telemetry.Metrics

	queue    chan *commitItem
	shutdown chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	backoff time.Duration
}

// NewCommitter 启动组提交 worker。Shutdown 后不得再 Enqueue。
func NewCommitter(store BatchFarmStore, cfg CommitterConfig) *Committer {
	if cfg.Window <= 0 {
		cfg.Window = defaultCommitterWindow
	}
	if cfg.ForegroundWindow <= 0 || cfg.ForegroundWindow > cfg.Window {
		cfg.ForegroundWindow = min(defaultForegroundWindow, cfg.Window)
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultCommitterMaxBatch
	}
	if cfg.QueueCap <= 0 {
		cfg.QueueCap = defaultCommitterQueueCap
	}
	if cfg.IOTimeout <= 0 {
		cfg.IOTimeout = DefaultIOTimeout
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultCommitterMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultCommitterMaxBackoff
	}
	c := &Committer{
		store:    store,
		cfg:      cfg,
		queue:    make(chan *commitItem, cfg.QueueCap),
		shutdown: make(chan struct{}),
		done:     make(chan struct{}),
		backoff:  cfg.MinBackoff,
	}
	go c.run()
	return c
}

// SetMetrics 注入指标采集，best-effort。
func (c *Committer) SetMetrics(m *telemetry.Metrics) {
	if c != nil {
		c.metrics = m
	}
}

// Enqueue 提交一份 immutable 快照与可选 outbox 事件。
func (c *Committer) Enqueue(uid, generation uint64, snapshot *farm.Aggregate, outboxEvents []outbox.Event, durable bool) (<-chan CommitResult, error) {
	return c.EnqueuePlan(uid, generation, snapshot, outboxEvents, outbox.PersistPlan{Mode: outbox.PersistFull}, durable)
}

// EnqueuePlan preserves the ordinary per-UID ordering while carrying the
// smallest safe persistence plan for the immutable snapshot.
func (c *Committer) EnqueuePlan(uid, generation uint64, snapshot *farm.Aggregate, outboxEvents []outbox.Event, plan outbox.PersistPlan, durable bool) (<-chan CommitResult, error) {
	return c.EnqueueMutationPlan(uid, generation, snapshot, outboxEvents, nil, nil, plan, durable)
}

// EnqueueMutationPlan carries every durable side effect produced by one Actor
// callback in the same group-commit record and Redis acknowledgement.
func (c *Committer) EnqueueMutationPlan(
	uid, generation uint64,
	snapshot *farm.Aggregate,
	outboxEvents []outbox.Event,
	tasks []outbox.TaskAdvance,
	codex []outbox.CodexReward,
	plan outbox.PersistPlan,
	durable bool,
) (<-chan CommitResult, error) {
	return c.enqueueMutationPlan(uid, generation, snapshot, nil, outboxEvents, tasks, codex, plan, durable)
}

// EnqueueIncrementalPlan accepts a detached Protobuf mutation. Production
// journal stores use this path so the Actor never clones a complete farm.
func (c *Committer) EnqueueIncrementalPlan(
	uid, generation uint64,
	mutation *farmv1.FarmWriteMutation,
	plan outbox.PersistPlan,
	durable bool,
) (<-chan CommitResult, error) {
	return c.enqueueMutationPlan(uid, generation, nil, mutation, nil, nil, nil, plan, durable)
}

func (c *Committer) enqueueMutationPlan(
	uid, generation uint64,
	snapshot *farm.Aggregate,
	mutation *farmv1.FarmWriteMutation,
	outboxEvents []outbox.Event,
	tasks []outbox.TaskAdvance,
	codex []outbox.CodexReward,
	plan outbox.PersistPlan,
	durable bool,
) (<-chan CommitResult, error) {
	if c == nil {
		return nil, errors.New("actor: committer is nil")
	}
	if snapshot == nil && mutation == nil || uid == 0 || generation == 0 {
		return nil, errors.New("actor: invalid commit enqueue")
	}
	result := make(chan CommitResult, 1)
	item := acquireCommitItem()
	item.uid = uid
	item.generation = generation
	item.snapshot = snapshot
	item.mutation = mutation
	item.outbox = append(item.outbox[:0], outboxEvents...)
	item.tasks = append(item.tasks[:0], tasks...)
	item.codex = append(item.codex[:0], codex...)
	item.plan = plan
	item.durable = durable
	item.waiters = append(item.waiters[:0], commitWaiter{generation: generation, result: result})

	select {
	case <-c.done:
		releaseCommitItem(item)
		return nil, errors.New("actor: committer stopped")
	default:
	}

	if durable {
		c.adjustPressure(1)
		select {
		case c.queue <- item:
			return result, nil
		case <-c.done:
			c.adjustPressure(-1)
			releaseCommitItem(item)
			return nil, errors.New("actor: committer stopped")
		}
	}

	c.adjustPressure(1)
	select {
	case c.queue <- item:
		return result, nil
	case <-c.done:
		c.adjustPressure(-1)
		releaseCommitItem(item)
		return nil, errors.New("actor: committer stopped")
	default:
		c.adjustPressure(-1)
		releaseCommitItem(item)
		return nil, errors.New("actor: committer queue full")
	}
}

// Shutdown 排空队列并停止 worker。
func (c *Committer) Shutdown(ctx context.Context) error {
	if c == nil {
		return nil
	}
	select {
	case <-c.done:
		return nil
	default:
	}
	c.stopOnce.Do(func() { close(c.shutdown) })
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("actor: committer shutdown incomplete: %w", ctx.Err())
	}
}

func (c *Committer) run() {
	defer close(c.done)

	for {
		batch, draining := c.collectBatch()
		dequeued := 0
		for _, entry := range batch {
			dequeued += len(entry.waiters)
		}
		if len(batch) == 0 && draining {
			return
		}
		if len(batch) == 0 {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.IOTimeout)
		start := time.Now()
		commits := make([]outbox.FarmCommit, 0, len(batch))
		logicalRequests := 0
		for _, entry := range batch {
			logicalRequests += len(entry.waiters)
			commits = append(commits, outbox.FarmCommit{
				Snapshot:     entry.snapshot,
				Mutation:     entry.mutation,
				Outbox:       entry.outbox,
				TaskAdvances: entry.tasks,
				CodexRewards: entry.codex,
				Plan:         entry.plan,
			})
		}
		if m := c.metrics; m != nil {
			m.ObserveCommitBatch(len(batch), logicalRequests)
		}
		err := c.store.CommitFarms(ctx, commits)
		cancel()
		// Keep dequeued requests counted as foreground pressure until their
		// durable Redis append finishes. Dropping them before CommitFarms made
		// the adaptive projector limiter see an empty queue exactly while the
		// foreground batch was competing with MySQL projection for one CPU.
		c.adjustPressure(-dequeued)
		if m := c.metrics; m != nil {
			m.ObserveActorSave(time.Since(start), err)
		}

		if err != nil {
			telemetry.L().Error("committer batch save failed",
				"component", "committer",
				"batch_size", len(batch),
				"err", err.Error(),
			)
			c.notifyBatch(batch, err)
			c.sleepBackoff()
			continue
		}
		c.backoff = c.cfg.MinBackoff
		c.notifyBatch(batch, nil)
	}
}

func (c *Committer) adjustPressure(delta int) {
	if delta == 0 {
		return
	}
	if store, ok := c.store.(foregroundPressureStore); ok {
		store.AdjustForegroundPressure(delta)
	}
}

func (c *Committer) collectBatch() (map[uint64]*commitItem, bool) {
	batch := make(map[uint64]*commitItem)
	logicalCount := 0
	hasForeground := false
	merge := func(item *commitItem) {
		logicalCount++
		hasForeground = hasForeground || item.durable
		entry := batch[item.uid]
		if entry == nil {
			batch[item.uid] = item
			return
		}
		entry.waiters = append(entry.waiters, item.waiters...)
		if item.generation >= entry.generation {
			entry.generation = item.generation
			entry.snapshot = item.snapshot
			entry.mutation = item.mutation
			entry.plan = item.plan
			// Actor submissions contain the complete, still-unacknowledged side
			// effect set. The newer generation therefore supersedes the older
			// copy; appending would count the same task advancement twice.
			entry.tasks = item.tasks
			entry.codex = item.codex
			// Ownership moved to the retained batch entry; do not clear these
			// backing arrays when recycling the merged shell below.
			item.tasks = nil
			item.codex = nil
		}
		entry.outbox = mergeOutboxEvents(entry.outbox, item.outbox)
		entry.durable = entry.durable || item.durable
		// Every retained field above was copied or detached into entry.
		releaseCommitItem(item)
	}
	drainQueued := func() {
		for logicalCount < c.cfg.MaxBatch {
			select {
			case item := <-c.queue:
				merge(item)
			default:
				return
			}
		}
	}

	// 没有待提交项时必须阻塞等待。若在这里启动 2ms timer，空闲进程会每秒
	// 无意义唤醒约 500 次。
	select {
	case item := <-c.queue:
		merge(item)
	case <-c.shutdown:
		drainQueued()
		return batch, true
	}
	// Consume work that was already waiting before opening a batching timer.
	// Under pressure this normally fills a useful batch without adding latency.
	drainQueued()
	if logicalCount >= c.cfg.MaxBatch {
		return batch, false
	}

	waitWindow := c.cfg.Window
	if hasForeground {
		waitWindow = c.cfg.ForegroundWindow
	}
	timerStarted := time.Now()
	timer := time.NewTimer(waitWindow)
	defer timer.Stop()

	for {
		select {
		case item := <-c.queue:
			itemDurable := item.durable
			merge(item)
			if itemDurable && waitWindow > c.cfg.ForegroundWindow {
				waitWindow = c.cfg.ForegroundWindow
				remaining := waitWindow - time.Since(timerStarted)
				if remaining <= 0 {
					return batch, false
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(remaining)
			}
			if logicalCount >= c.cfg.MaxBatch {
				return batch, false
			}
			// A deep queue already supplies batching; drain it now instead of
			// spending the full time window while foreground callers wait.
			if len(c.queue) >= max(1, c.cfg.MaxBatch/4) {
				drainQueued()
				return batch, false
			}

		case <-timer.C:
			return batch, false

		case <-c.shutdown:
			drainQueued()
			return batch, true
		}
	}
}

func (c *Committer) notifyBatch(batch map[uint64]*commitItem, err error) {
	for _, entry := range batch {
		committedGen := entry.generation
		for _, waiter := range entry.waiters {
			if err != nil {
				waiter.result <- CommitResult{Generation: waiter.generation, Err: err}
				continue
			}
			if waiter.generation <= committedGen {
				waiter.result <- CommitResult{Generation: committedGen, Err: nil}
			}
		}
		releaseCommitItem(entry)
	}
}

func (c *Committer) sleepBackoff() {
	time.Sleep(c.backoff)
	next := c.backoff * 2
	if next > c.cfg.MaxBackoff {
		next = c.cfg.MaxBackoff
	}
	c.backoff = next
}

// saveFarmsAdapter 让仅有 SaveFarm 的测试 fake 仍走 Committer 路径。
type saveFarmsAdapter struct {
	store FarmStore
}

func (a *saveFarmsAdapter) SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error {
	for _, snap := range snapshots {
		if err := a.store.SaveFarm(ctx, snap); err != nil {
			return err
		}
	}
	return nil
}

func (a *saveFarmsAdapter) CommitFarms(ctx context.Context, commits []outbox.FarmCommit) error {
	snapshots := make([]*farm.Aggregate, 0, len(commits))
	for _, commit := range commits {
		snapshots = append(snapshots, commit.Snapshot)
	}
	return a.SaveFarms(ctx, snapshots)
}

func mergeOutboxEvents(existing, incoming []outbox.Event) []outbox.Event {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]outbox.Event, 0, len(existing)+len(incoming))
	for _, event := range existing {
		if event.EventID == "" {
			continue
		}
		if _, ok := seen[event.EventID]; ok {
			continue
		}
		seen[event.EventID] = struct{}{}
		merged = append(merged, event)
	}
	for _, event := range incoming {
		if event.EventID == "" {
			continue
		}
		if _, ok := seen[event.EventID]; ok {
			continue
		}
		seen[event.EventID] = struct{}{}
		merged = append(merged, event)
	}
	return merged
}

func asBatchStore(store FarmStore) BatchFarmStore {
	if bs, ok := store.(BatchFarmStore); ok {
		return bs
	}
	return &saveFarmsAdapter{store: store}
}
