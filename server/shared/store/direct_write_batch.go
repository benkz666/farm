package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"
)

const (
	directWriteBatchWindow = 25 * time.Millisecond
	directWriteBatchMax    = 64
	directWriteQueueSize   = 4096
	directWriteTimeout     = 5 * time.Second
)

type directWriteResult struct {
	result sql.Result
	err    error
}

type directWriteRequest struct {
	uid   uint64
	query string
	args  []any
	done  chan directWriteResult
}

type sqlContextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type directUIDExecer struct {
	batcher *directWriteBatcher
	uid     uint64
}

func (exec directUIDExecer) ExecContext(
	ctx context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	return exec.batcher.ExecContext(ctx, exec.uid, query, args...)
}

// directWriteBatcher groups independent task/mail mutations into one MySQL
// transaction. These foreground operations previously generated one durable
// fsync each and shared the same disk with the asynchronous Farm projector;
// batching preserves response-after-COMMIT semantics while amortizing fsync.
type directWriteBatcher struct {
	ctx      context.Context
	db       *sql.DB
	requests chan directWriteRequest
	once     sync.Once
}

func newDirectWriteBatcher(ctx context.Context, db *sql.DB) *directWriteBatcher {
	if ctx == nil {
		ctx = context.Background()
	}
	return &directWriteBatcher{
		ctx: ctx, db: db, requests: make(chan directWriteRequest, directWriteQueueSize),
	}
}

func (batcher *directWriteBatcher) ExecContext(
	ctx context.Context,
	uid uint64,
	query string,
	args ...any,
) (sql.Result, error) {
	if batcher == nil || batcher.db == nil {
		return nil, errors.New("store: direct write batcher is unavailable")
	}
	batcher.once.Do(func() { go batcher.run() })
	request := directWriteRequest{
		uid: uid, query: query, args: append([]any(nil), args...), done: make(chan directWriteResult, 1),
	}
	select {
	case batcher.requests <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-batcher.ctx.Done():
		return nil, batcher.ctx.Err()
	}
	// Once accepted, wait for the transaction outcome even if the caller's
	// context is cancelled; returning an ambiguous result could make a client
	// retry a write that has actually committed.
	select {
	case result := <-request.done:
		return result.result, result.err
	case <-batcher.ctx.Done():
		return nil, batcher.ctx.Err()
	}
}

func (batcher *directWriteBatcher) run() {
	for {
		select {
		case first := <-batcher.requests:
			batch := []directWriteRequest{first}
			timer := time.NewTimer(directWriteBatchWindow)
		collect:
			for len(batch) < directWriteBatchMax {
				select {
				case request := <-batcher.requests:
					batch = append(batch, request)
				case <-timer.C:
					break collect
				case <-batcher.ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					batcher.fail(batch, batcher.ctx.Err())
					batcher.drain(batcher.ctx.Err())
					return
				}
			}
			sort.SliceStable(batch, func(left, right int) bool { return batch[left].uid < batch[right].uid })
			batcher.execute(batch)
		case <-batcher.ctx.Done():
			batcher.drain(batcher.ctx.Err())
			return
		}
	}
}

func (batcher *directWriteBatcher) execute(batch []directWriteRequest) {
	ctx, cancel := context.WithTimeout(batcher.ctx, directWriteTimeout)
	defer cancel()
	tx, err := batcher.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		batcher.fail(batch, err)
		return
	}
	results := make([]sql.Result, len(batch))
	for index := range batch {
		results[index], err = tx.ExecContext(ctx, batch[index].query, batch[index].args...)
		if err != nil {
			_ = tx.Rollback()
			batcher.fail(batch, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		batcher.fail(batch, err)
		return
	}
	for index := range batch {
		batch[index].done <- directWriteResult{result: results[index]}
	}
}

func (batcher *directWriteBatcher) fail(batch []directWriteRequest, err error) {
	for index := range batch {
		batch[index].done <- directWriteResult{err: err}
	}
}

func (batcher *directWriteBatcher) drain(err error) {
	for {
		select {
		case request := <-batcher.requests:
			request.done <- directWriteResult{err: err}
		default:
			return
		}
	}
}
