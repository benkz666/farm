package room

import (
	"context"
	"errors"
	"sync"
	"time"

	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"
)

const (
	defaultPairCommitWindow   = 500 * time.Microsecond
	defaultPairCommitMaxBatch = 128
	defaultPairCommitQueueCap = 8192
)

type pairCommitterConfig struct {
	Window    time.Duration
	MaxBatch  int
	QueueCap  int
	IOTimeout time.Duration
}

func defaultPairCommitterConfig(ioTimeout time.Duration) pairCommitterConfig {
	return pairCommitterConfig{
		Window:    defaultPairCommitWindow,
		MaxBatch:  defaultPairCommitMaxBatch,
		QueueCap:  defaultPairCommitQueueCap,
		IOTimeout: ioTimeout,
	}
}

type pairCommitRequest struct {
	commits [2]outbox.FarmCommit
	count   int
	result  chan error
}

var pairCommitRequestPool = sync.Pool{New: func() any {
	return &pairCommitRequest{result: make(chan error, 1)}
}}

func acquirePairCommitRequest(commits []outbox.FarmCommit) *pairCommitRequest {
	request := pairCommitRequestPool.Get().(*pairCommitRequest)
	request.count = copy(request.commits[:], commits)
	return request
}

func releasePairCommitRequest(request *pairCommitRequest) {
	if request == nil {
		return
	}
	clear(request.commits[:])
	request.count = 0
	pairCommitRequestPool.Put(request)
}

// pairCommitter 把多个互不重叠的双 Actor 提交合并成一次存储调用。
// 每个请求仍在可靠日志确认后才返回，只减少 Redis 往返和 Lua 调用次数。
type pairCommitter struct {
	store   BatchFarmStore
	config  pairCommitterConfig
	metrics *telemetry.Metrics

	queue    chan *pairCommitRequest
	shutdown chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	acceptMu sync.RWMutex
	stopped  bool
}

func newPairCommitter(store BatchFarmStore, config pairCommitterConfig) *pairCommitter {
	if config.Window <= 0 {
		config.Window = defaultPairCommitWindow
	}
	if config.MaxBatch <= 0 {
		config.MaxBatch = defaultPairCommitMaxBatch
	}
	if config.QueueCap <= 0 {
		config.QueueCap = defaultPairCommitQueueCap
	}
	if config.IOTimeout <= 0 {
		config.IOTimeout = DefaultIOTimeout
	}
	committer := &pairCommitter{
		store: store, config: config,
		queue:    make(chan *pairCommitRequest, config.QueueCap),
		shutdown: make(chan struct{}), done: make(chan struct{}),
	}
	go committer.run()
	return committer
}

func (committer *pairCommitter) SetMetrics(metrics *telemetry.Metrics) {
	if committer != nil {
		committer.metrics = metrics
	}
}

func (committer *pairCommitter) Commit(commits []outbox.FarmCommit) error {
	if committer == nil || committer.store == nil {
		return errors.New("actor: pair committer is nil")
	}
	if len(commits) == 0 {
		return nil
	}
	if len(commits) > 2 {
		return errors.New("actor: pair commit contains more than two farms")
	}
	request := acquirePairCommitRequest(commits)

	committer.acceptMu.RLock()
	if committer.stopped {
		committer.acceptMu.RUnlock()
		releasePairCommitRequest(request)
		return errors.New("actor: pair committer stopped")
	}
	committer.queue <- request
	committer.acceptMu.RUnlock()

	err := <-request.result
	releasePairCommitRequest(request)
	return err
}

func (committer *pairCommitter) Shutdown(ctx context.Context) error {
	if committer == nil {
		return nil
	}
	committer.stopOnce.Do(func() {
		committer.acceptMu.Lock()
		committer.stopped = true
		close(committer.shutdown)
		committer.acceptMu.Unlock()
	})
	select {
	case <-committer.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (committer *pairCommitter) run() {
	defer close(committer.done)
	batchBuffer := make([]*pairCommitRequest, 0, committer.config.MaxBatch)
	commitBuffer := make([]outbox.FarmCommit, 0, committer.config.MaxBatch*2)
	for {
		batch, draining := committer.collectBatch(batchBuffer[:0])
		if len(batch) == 0 {
			if draining {
				return
			}
			continue
		}

		commitCount := 0
		for _, request := range batch {
			commitCount += request.count
		}
		commits := commitBuffer[:0]
		for _, request := range batch {
			commits = append(commits, request.commits[:request.count]...)
		}

		ctx, cancel := context.WithTimeout(context.Background(), committer.config.IOTimeout)
		started := time.Now()
		if committer.metrics != nil {
			committer.metrics.ObserveCommitBatch(commitCount, len(batch))
		}
		err := committer.store.CommitFarms(ctx, commits)
		cancel()
		if committer.metrics != nil {
			committer.metrics.ObserveActorSave(time.Since(started), err)
		}
		for _, request := range batch {
			request.result <- err
		}
		clear(commits)
		commitBuffer = commits[:0]
		clear(batch)
		batchBuffer = batch[:0]
		if draining {
			return
		}
	}
}

func (committer *pairCommitter) collectBatch(batch []*pairCommitRequest) ([]*pairCommitRequest, bool) {
	drain := func() {
		for len(batch) < committer.config.MaxBatch {
			select {
			case request := <-committer.queue:
				batch = append(batch, request)
			default:
				return
			}
		}
	}

	select {
	case request := <-committer.queue:
		batch = append(batch, request)
	case <-committer.shutdown:
		drain()
		return batch, true
	}
	drain()
	if len(batch) >= committer.config.MaxBatch {
		return batch, false
	}

	timer := time.NewTimer(committer.config.Window)
	defer timer.Stop()
	for {
		select {
		case request := <-committer.queue:
			batch = append(batch, request)
			if len(batch) >= committer.config.MaxBatch {
				return batch, false
			}
			// 深队列本身已经形成批量，不再额外等待完整窗口。
			if len(committer.queue) >= max(1, committer.config.MaxBatch/4) {
				drain()
				return batch, false
			}
		case <-timer.C:
			return batch, false
		case <-committer.shutdown:
			drain()
			return batch, true
		}
	}
}
