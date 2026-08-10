package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// StealHintStore 维护「农场是否有可偷成熟作物」的弱一致摘要（Redis）。
type StealHintStore interface {
	SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error
	GetStealHints(ctx context.Context, uids []uint64) (map[uint64]bool, error)
}

const (
	stealHintFlushWindow = 500 * time.Microsecond
	stealHintBatchSize   = 512
	stealHintIOTimeout   = time.Second
	stealHintRetryDelay  = 10 * time.Millisecond
)

// AsyncStealHintStore keeps the weak-consistent FriendList hint out of the
// authoritative Farm action latency. Updates for the same UID are coalesced
// and flushed in a Redis pipeline; a newer value always wins over a failed
// older batch retry.
//
// The hint is deliberately advisory. Farm state, FarmDelta and SyncFarm remain
// authoritative, so a saturated hint writer must never block Water/Harvest.
type AsyncStealHintStore struct {
	base StealHintStore

	mu      sync.Mutex
	pending map[uint64]bool
	closed  bool
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// NewAsyncStealHintStore starts one coalescing writer. Call Shutdown during
// service drain so the latest advisory values get a best-effort final flush.
func NewAsyncStealHintStore(base StealHintStore) *AsyncStealHintStore {
	writer := &AsyncStealHintStore{
		base:    base,
		pending: make(map[uint64]bool),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go writer.run()
	return writer
}

// SetStealHint records only the newest value and returns without Redis I/O.
func (writer *AsyncStealHintStore) SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error {
	if writer == nil || writer.base == nil {
		return errors.New("store: async steal hint writer unavailable")
	}
	if uid == 0 {
		return errors.New("store: invalid steal hint uid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	writer.mu.Lock()
	if writer.closed {
		writer.mu.Unlock()
		return errors.New("store: async steal hint writer stopped")
	}
	writer.pending[uid] = hasStealable
	writer.mu.Unlock()
	select {
	case writer.wake <- struct{}{}:
	default:
	}
	return nil
}

func (writer *AsyncStealHintStore) GetStealHints(ctx context.Context, uids []uint64) (map[uint64]bool, error) {
	if writer == nil || writer.base == nil {
		return make(map[uint64]bool), nil
	}
	return writer.base.GetStealHints(ctx, uids)
}

// Shutdown rejects new updates, drains queued hints and waits for the worker.
func (writer *AsyncStealHintStore) Shutdown(ctx context.Context) error {
	if writer == nil {
		return nil
	}
	writer.once.Do(func() {
		writer.mu.Lock()
		writer.closed = true
		writer.mu.Unlock()
		close(writer.stop)
	})
	select {
	case <-writer.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("store: async steal hint shutdown: %w", ctx.Err())
	}
}

func (writer *AsyncStealHintStore) run() {
	defer close(writer.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		select {
		case <-writer.wake:
			resetStealHintTimer(timer, stealHintFlushWindow)
			select {
			case <-timer.C:
			case <-writer.stop:
				writer.flushAll()
				return
			}
			writer.flushAvailable()
		case <-writer.stop:
			writer.flushAll()
			return
		}
	}
}

func (writer *AsyncStealHintStore) flushAvailable() {
	for {
		batch := writer.takeBatch(stealHintBatchSize)
		if len(batch) == 0 {
			return
		}
		if writer.flush(batch) != nil {
			writer.requeue(batch)
			time.AfterFunc(stealHintRetryDelay, func() {
				select {
				case writer.wake <- struct{}{}:
				default:
				}
			})
			return
		}
	}
}

func (writer *AsyncStealHintStore) flushAll() {
	writer.mu.Lock()
	batch := writer.pending
	writer.pending = make(map[uint64]bool)
	writer.mu.Unlock()
	if len(batch) > 0 {
		// Shutdown is best effort because these are advisory values. Flush all
		// remaining keys in one pipeline so drain is bounded by one I/O timeout.
		_ = writer.flush(batch)
	}
}

func (writer *AsyncStealHintStore) takeBatch(limit int) map[uint64]bool {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.pending) == 0 {
		return nil
	}
	limit = min(limit, len(writer.pending))
	batch := make(map[uint64]bool, limit)
	for uid, value := range writer.pending {
		batch[uid] = value
		delete(writer.pending, uid)
		if len(batch) == limit {
			break
		}
	}
	return batch
}

func (writer *AsyncStealHintStore) requeue(batch map[uint64]bool) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return
	}
	for uid, value := range batch {
		if _, newer := writer.pending[uid]; !newer {
			writer.pending[uid] = value
		}
	}
}

func (writer *AsyncStealHintStore) flush(batch map[uint64]bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), stealHintIOTimeout)
	defer cancel()
	if batched, ok := writer.base.(interface {
		SetStealHints(context.Context, map[uint64]bool) error
	}); ok {
		return batched.SetStealHints(ctx, batch)
	}
	for uid, value := range batch {
		if err := writer.base.SetStealHint(ctx, uid, value); err != nil {
			return err
		}
	}
	return nil
}

func resetStealHintTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func stealHintKey(uid uint64) string {
	return "steal_hint:" + strconv.FormatUint(uid, 10)
}

// SetStealHint 写入或清除可偷摘要。hasStealable=false 时删除键，避免 FriendList 读到陈旧 true。
func (s *Store) SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("store: steal hint redis unavailable")
	}
	key := stealHintKey(uid)
	if !hasStealable {
		if err := s.rdb.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("store: delete steal hint: %w", err)
		}
		return nil
	}
	if err := s.rdb.Set(ctx, key, "1", 0).Err(); err != nil {
		return fmt.Errorf("store: set steal hint: %w", err)
	}
	return nil
}

// SetStealHints writes a coalesced hint batch in one Redis round trip.
func (s *Store) SetStealHints(ctx context.Context, hints map[uint64]bool) error {
	if s == nil || s.rdb == nil {
		return fmt.Errorf("store: steal hint redis unavailable")
	}
	if len(hints) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	for uid, hasStealable := range hints {
		if uid == 0 {
			continue
		}
		key := stealHintKey(uid)
		if hasStealable {
			pipe.Set(ctx, key, "1", 0)
		} else {
			pipe.Del(ctx, key)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("store: write steal hint batch: %w", err)
	}
	return nil
}

// GetStealHints 批量读取可偷摘要；缺失键视为 false，不出现在返回 map 中亦可（调用方按 false 处理）。
func (s *Store) GetStealHints(ctx context.Context, uids []uint64) (map[uint64]bool, error) {
	out := make(map[uint64]bool, len(uids))
	if s == nil || s.rdb == nil || len(uids) == 0 {
		return out, nil
	}
	keys := make([]string, len(uids))
	for i, uid := range uids {
		keys[i] = stealHintKey(uid)
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("store: mget steal hints: %w", err)
	}
	for i, val := range vals {
		if val == nil {
			continue
		}
		switch v := val.(type) {
		case string:
			if v == "1" || v == "true" {
				out[uids[i]] = true
			}
		}
	}
	return out, nil
}
