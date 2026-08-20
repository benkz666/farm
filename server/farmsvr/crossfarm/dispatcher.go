package crossfarm

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"farm/server/shared/errcode"
	"farm/server/shared/outbox"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

const (
	dispatcherPollInterval = 250 * time.Millisecond
	dispatcherBatchSize    = 64
	// 投递保持串行。一行要等对端 Farm 处理完 DeliverCrossResult（实测 12.5ms：
	// 写 journal 加改 actor 状态），串行把吞吐锁在 80 行/秒，而静态双分片下跨
	// 农场事件以约 400 行/秒产生，outbox 会持续积压、访客延迟收到跨农场结果。
	//
	// 但并行化解决不了：投递的真实成本在对端 Farm 的处理和它背后那个共享
	// MySQL，不在 dispatcher 自己。1.8U 双分片下实测 16 路让对端 journal 积压
	// 9.2 万、98% 业务请求失败；8 路让 MySQL 节流从 0 涨到 18 秒、连接池耗尽、
	// 投影批量报 context deadline exceeded，P99 从 1.2 秒恶化到 6.8 秒。
	// 提高投递并发只是把同一份压力更快地推给已经饱和的下游。
	//
	// 真正的解法是减少跨分片流量本身（按好友关系聚类分片）或让数据层跟着水平
	// 扩展；在那之前，积压换稳定是更划算的取舍——业务请求并不等待这条投递路径。
	dispatcherMinBackoff = 25 * time.Millisecond
	dispatcherMaxBackoff   = 5 * time.Second
	dispatcherMaxAttempts  = 100
	dispatcherOpTimeout    = 5 * time.Second
	// 已投递的行留 24 小时对可靠性毫无贡献，却让表无界增长：2 倍水平扩展一轮
	// 压测就攒下 61.7 万行。代价不只是空间——published_at 是 idx_outbox_publish
	// 的首列，把它从 NULL 改成时间戳会让索引条目搬家，表越大随机写越多。
	// 保留期只需覆盖 journal 投影的重放窗口（默认 5 分钟），让重放期内的
	// 重复 INSERT 仍命中 ON DUPLICATE KEY 而不是重新投递一遍。
	outboxRetention       = 10 * time.Minute
	outboxCleanupInterval = 30 * time.Second
)

// OutboxDispatcher delivers durable cross results to visitor farms.
type OutboxDispatcher struct {
	store       store.OutboxStore
	client      *GRPCClient
	players     PlayerDeltaPublisher
	now         func() int64
	wakeup      chan struct{}
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	lastCleanup int64
}

// NewOutboxDispatcher starts the background outbox fan-out worker.
func NewOutboxDispatcher(
	outboxStore store.OutboxStore,
	client *GRPCClient,
	now func() int64,
	players ...PlayerDeltaPublisher,
) *OutboxDispatcher {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	d := &OutboxDispatcher{
		store:  outboxStore,
		client: client,
		now:    now,
		wakeup: make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if len(players) > 0 {
		d.players = players[0]
	}
	go d.run()
	return d
}

// Shutdown stops the dispatcher gracefully.
func (d *OutboxDispatcher) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	select {
	case <-d.done:
		return nil
	default:
	}
	d.stopOnce.Do(func() { close(d.stop) })
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *OutboxDispatcher) run() {
	defer close(d.done)
	ticker := time.NewTicker(dispatcherPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
		case <-d.wakeup:
		}
		d.pollOnce()
		d.maybeCleanupPublished()
	}
}

func (d *OutboxDispatcher) pollOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), dispatcherOpTimeout)
	defer cancel()
	rows, err := d.store.ClaimDueOutbox(ctx, dispatcherBatchSize, d.now())
	if err != nil {
		telemetry.L().Error("outbox claim failed", "component", "outbox", "err", err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}
	// 批量确认把一批的 N 次 DELETE 收成一条语句。store 未实现该扩展时回退到
	// 逐行确认，此时 deliverRow 自己收尾。
	batcher, _ := d.store.(store.OutboxBatchPublisher)
	delivered := make([]string, 0, len(rows))
	for _, row := range rows {
		eventID, ok := d.deliverRow(row, batcher != nil)
		if !ok {
			continue
		}
		delivered = append(delivered, eventID)
	}
	if batcher == nil || len(delivered) == 0 {
		return
	}
	ackCtx, ackCancel := context.WithTimeout(context.Background(), dispatcherOpTimeout)
	defer ackCancel()
	if err := batcher.MarkOutboxPublishedBatch(ackCtx, delivered); err != nil {
		telemetry.L().Error("outbox batch ack failed",
			"component", "outbox", "count", len(delivered), "err", err.Error())
	}
}

// deliverRow 投递一行，返回待确认的事件 ID。deferAck 为真时确认交给调用方批量
// 执行，返回值即待确认 ID；为假时本函数自己确认并返回 false。
func (d *OutboxDispatcher) deliverRow(row store.OutboxRow, deferAck bool) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatcherOpTimeout)
	defer cancel()
	ack := func() (string, bool) {
		if deferAck {
			return row.EventID, true
		}
		if err := d.store.MarkOutboxPublished(ctx, row.EventID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			telemetry.L().Error("outbox mark published failed",
				"component", "outbox",
				"event_id", row.EventID,
				"err", err.Error(),
			)
		}
		return "", false
	}
	switch row.Kind {
	case outbox.KindCrossResult:
		result, err := outbox.DecodeCrossResult(row.Payload)
		if err != nil {
			return ack()
		}
		domain, ok := resultFromProto(result)
		if !ok {
			return ack()
		}
		_, playerDelta, code, err := d.client.DeliverCrossResult(ctx, domain)
		if err != nil || code == errcode.Internal {
			d.scheduleRetry(row)
			return "", false
		}
		// The direct path returns this delta to Gateway. The dispatcher is the
		// only fallback consumer, so it owns the one best-effort push here.
		if playerDelta != nil && d.players != nil {
			_ = d.players.PublishPlayerDelta(context.Background(), domain.VisitorUID, *playerDelta)
		}
		return ack()
	default:
		return ack()
	}
}

func (d *OutboxDispatcher) scheduleRetry(row store.OutboxRow) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatcherOpTimeout)
	defer cancel()
	attempts := row.Attempts + 1
	if attempts >= dispatcherMaxAttempts {
		if err := d.store.MarkOutboxDeadLetter(ctx, row.EventID, attempts); err != nil {
			telemetry.L().Error("outbox dead letter failed",
				"component", "outbox",
				"event_id", row.EventID,
				"attempts", attempts,
				"err", err.Error(),
			)
			return
		}
		telemetry.L().Error("outbox moved to dead letter",
			"component", "outbox",
			"event_id", row.EventID,
			"attempts", attempts,
		)
		return
	}
	backoff := dispatcherRetryBackoff(attempts)
	next := d.now() + backoff.Milliseconds()
	_ = d.store.MarkOutboxRetry(ctx, row.EventID, attempts, next)
}

func dispatcherRetryBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return dispatcherMinBackoff
	}
	// Saturate before shifting: a Duration left shift at attempts>=63 wraps
	// negative and would make the row immediately due in a tight retry loop.
	const maxSafeShift = 7
	if attempts > maxSafeShift {
		return dispatcherMaxBackoff
	}
	backoff := dispatcherMinBackoff << attempts
	if backoff > dispatcherMaxBackoff {
		return dispatcherMaxBackoff
	}
	return backoff
}

func (d *OutboxDispatcher) maybeCleanupPublished() {
	now := d.now()
	if d.lastCleanup != 0 && now-d.lastCleanup < outboxCleanupInterval.Milliseconds() {
		return
	}
	d.lastCleanup = now
	ctx, cancel := context.WithTimeout(context.Background(), dispatcherOpTimeout)
	defer cancel()
	before := now - outboxRetention.Milliseconds()
	if _, err := d.store.DeletePublishedOutboxBefore(ctx, before); err != nil {
		telemetry.L().Error("outbox cleanup failed", "component", "outbox", "err", err.Error())
	}
}
