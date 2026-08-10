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
	dispatcherMinBackoff   = 25 * time.Millisecond
	dispatcherMaxBackoff   = 5 * time.Second
	dispatcherMaxAttempts  = 100
	dispatcherOpTimeout    = 5 * time.Second
	outboxRetention        = 24 * time.Hour
	outboxCleanupInterval  = time.Hour
)

// OutboxDispatcher delivers durable cross results to visitor farms.
type OutboxDispatcher struct {
	store       store.OutboxStore
	client      *GRPCClient
	now         func() int64
	wakeup      chan struct{}
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	lastCleanup int64
}

// NewOutboxDispatcher starts the background outbox fan-out worker.
func NewOutboxDispatcher(outboxStore store.OutboxStore, client *GRPCClient, now func() int64) *OutboxDispatcher {
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
	for _, row := range rows {
		d.deliverRow(row)
	}
}

func (d *OutboxDispatcher) deliverRow(row store.OutboxRow) {
	ctx, cancel := context.WithTimeout(context.Background(), dispatcherOpTimeout)
	defer cancel()
	switch row.Kind {
	case outbox.KindCrossResult:
		result, err := outbox.DecodeCrossResult(row.Payload)
		if err != nil {
			_ = d.store.MarkOutboxPublished(ctx, row.EventID)
			return
		}
		domain, ok := resultFromProto(result)
		if !ok {
			_ = d.store.MarkOutboxPublished(ctx, row.EventID)
			return
		}
		_, _, code, err := d.client.DeliverCrossResult(ctx, domain)
		if err != nil || code == errcode.Internal {
			d.scheduleRetry(row)
			return
		}
	default:
		_ = d.store.MarkOutboxPublished(ctx, row.EventID)
		return
	}
	if err := d.store.MarkOutboxPublished(ctx, row.EventID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		telemetry.L().Error("outbox mark published failed",
			"component", "outbox",
			"event_id", row.EventID,
			"err", err.Error(),
		)
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
