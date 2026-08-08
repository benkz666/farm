package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"
)

const (
	defaultCommitterWindow     = 2 * time.Millisecond
	defaultCommitterMaxBatch   = 128
	defaultCommitterQueueCap   = 4096
	defaultCommitterMinBackoff = 10 * time.Millisecond
	defaultCommitterMaxBackoff = 100 * time.Millisecond
)

// BatchFarmStore 是 Committer 所需的批量落盘边界。
type BatchFarmStore interface {
	SaveFarms(ctx context.Context, snapshots []*farm.Aggregate) error
	CommitFarms(ctx context.Context, commits []outbox.FarmCommit) error
}

// CommitResult 回报一次组提交的结果。
type CommitResult struct {
	Generation uint64
	Err        error
}

// CommitterConfig 配置组提交窗口与背压。
type CommitterConfig struct {
	Window     time.Duration
	MaxBatch   int
	QueueCap   int
	IOTimeout  time.Duration
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func defaultCommitterConfig() CommitterConfig {
	return CommitterConfig{
		Window:     defaultCommitterWindow,
		MaxBatch:   defaultCommitterMaxBatch,
		QueueCap:   defaultCommitterQueueCap,
		IOTimeout:  DefaultIOTimeout,
		MinBackoff: defaultCommitterMinBackoff,
		MaxBackoff: defaultCommitterMaxBackoff,
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
	outbox     []outbox.Event
	plan       outbox.PersistPlan
	waiters    []commitWaiter
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
	if c == nil {
		return nil, errors.New("actor: committer is nil")
	}
	if snapshot == nil || uid == 0 || generation == 0 {
		return nil, errors.New("actor: invalid commit enqueue")
	}
	result := make(chan CommitResult, 1)
	item := &commitItem{
		uid:        uid,
		generation: generation,
		snapshot:   snapshot,
		outbox:     append([]outbox.Event(nil), outboxEvents...),
		plan:       plan,
		waiters: []commitWaiter{{
			generation: generation,
			result:     result,
		}},
	}

	select {
	case <-c.done:
		return nil, errors.New("actor: committer stopped")
	default:
	}

	if durable {
		select {
		case c.queue <- item:
			return result, nil
		case <-c.done:
			return nil, errors.New("actor: committer stopped")
		}
	}

	select {
	case c.queue <- item:
		return result, nil
	case <-c.done:
		return nil, errors.New("actor: committer stopped")
	default:
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
				Snapshot: entry.snapshot,
				Outbox:   entry.outbox,
				Plan:     entry.plan,
			})
		}
		if m := c.metrics; m != nil {
			m.ObserveCommitBatch(len(batch), logicalRequests)
		}
		err := c.store.CommitFarms(ctx, commits)
		cancel()
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

func (c *Committer) collectBatch() (map[uint64]*commitItem, bool) {
	batch := make(map[uint64]*commitItem)
	merge := func(item *commitItem) {
		entry := batch[item.uid]
		if entry == nil {
			batch[item.uid] = item
			return
		}
		entry.waiters = append(entry.waiters, item.waiters...)
		if item.generation > entry.generation {
			entry.generation = item.generation
			entry.snapshot = item.snapshot
			entry.plan = item.plan
		}
		entry.outbox = mergeOutboxEvents(entry.outbox, item.outbox)
	}
	drainQueued := func() {
		for len(batch) < c.cfg.MaxBatch {
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
	if len(batch) >= c.cfg.MaxBatch {
		return batch, false
	}

	timer := time.NewTimer(c.cfg.Window)
	defer timer.Stop()

	for {
		select {
		case item := <-c.queue:
			merge(item)
			if len(batch) >= c.cfg.MaxBatch {
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
