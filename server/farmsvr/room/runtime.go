package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/domain/farm"
	"farm/server/shared/outbox"
	"farm/server/shared/telemetry"
)

const (
	// DefaultIdleTTL 是 Actor 无请求后卸载前的等待时长。
	DefaultIdleTTL = 10 * time.Minute

	// DefaultFlushInterval 是脏聚合的最长落盘延迟，也就是进程被强杀时的数据丢失
	// 上界。架构 5.3 节 C 档写回：用这个窗口合并同一玩家的连续改动，换掉「每个
	// 动作一次 MySQL 事务」。只在空闲时落盘是不够的——在线玩家的 Actor 永不空闲。
	DefaultFlushInterval = 1 * time.Second

	// DefaultCallTimeout 限制 Do 等待 Actor 接收请求的时长。
	// 只覆盖「尚未被接收」的阶段，所以超时返回时一定没有产生任何副作用。
	DefaultCallTimeout = 5 * time.Second

	// DefaultIOTimeout 限制单次加载或落盘的时长，让 Actor 串行区不会被慢存储卡死。
	DefaultIOTimeout = 3 * time.Second

	drainRetryMinBackoff = 10 * time.Millisecond
	drainRetryMaxBackoff = 250 * time.Millisecond
	coldLoadBatchWindow  = 500 * time.Microsecond
	coldLoadBatchMax     = 128
)

var (
	// ErrDraining 表示运行时正在疏散，不再接收新请求。
	// 调用方应按协议 ERR_REDIRECT 让客户端重连到新实例。
	ErrDraining = errors.New("actor: runtime is draining")

	// ErrBusy 表示 Actor 在 callTimeout 内没能接收这个请求。
	// 请求未被接收，因此没有任何副作用，调用方可安全重试。
	ErrBusy = errors.New("actor: actor is busy")

	// ErrCapacity means the process reached its configured resident-Actor
	// safety ceiling. Refusing a new cold load is preferable to letting the
	// cgroup OOM killer terminate every resident farm on the instance.
	ErrCapacity = errors.New("actor: resident capacity reached")

	requestResultPool = sync.Pool{New: func() any {
		return make(chan error, 1)
	}}
	pairActorRequestPool = sync.Pool{New: func() any {
		return &pairActorRequest{
			ready:    make(chan pairActorReady, 1),
			proceed:  make(chan error, 1),
			prepared: make(chan pairActorPrepared, 1),
			complete: make(chan error, 1),
			done:     make(chan struct{}, 1),
		}
	}}
)

// FarmStore 是 Actor 所需的最小持久化边界。
// 具体的 store.FarmStore 实现满足本接口，无需让 Actor 依赖账号存储方法。
type FarmStore interface {
	LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error)
	SaveFarm(ctx context.Context, aggregate *farm.Aggregate) error
}

type incrementalFarmStore interface {
	SupportsIncrementalFarmCommits() bool
}

type batchFarmStore interface {
	LoadFarms(context.Context, []uint64) (map[uint64]*farm.Aggregate, error)
}

type coldLoadResult struct {
	aggregate *farm.Aggregate
	err       error
}

type coldLoadRequest struct {
	uid    uint64
	result chan coldLoadResult
}

type coldLoadBatch struct {
	requests []coldLoadRequest
	flush    sync.Once
}

type commitAck struct {
	generation uint64
	err        error
}

// Runtime 管理本进程驻留的玩家 Actor。
type Runtime struct {
	store         FarmStore
	committers    []*Committer
	pairCommitter *pairCommitter
	idleTTL       time.Duration
	flushInterval time.Duration
	callTimeout   time.Duration
	ioTimeout     time.Duration
	maxResident   int
	hazardSalt    uint64
	metrics       *telemetry.Metrics

	mu          sync.Mutex
	actors      map[uint64]*residentActor
	draining    bool
	drainCtx    context.Context
	drainFailed atomic.Bool
	wg          sync.WaitGroup

	loadMu    sync.Mutex
	loadBatch *coldLoadBatch
}

// IsResident reports whether uid currently has an in-memory actor. Scheduled
// time advancement uses it to avoid loading farms whose players are offline.
func (r *Runtime) IsResident(uid uint64) bool {
	if r == nil || uid == 0 {
		return false
	}
	r.mu.Lock()
	resident := r.actors[uid]
	r.mu.Unlock()
	return resident != nil
}

type residentActor struct {
	mailbox  chan request
	done     chan struct{}
	drain    chan struct{}
	waiters  atomic.Int32
	stopOnce sync.Once
}

type request struct {
	fn     func(*FarmActor) error
	result chan error
	pair   *pairActorRequest
}

// pairActorRequest temporarily lends one Actor to DoPairDurable. The Actor
// run loop remains blocked until the coordinator has built and committed both
// UID mutations, so ordinary mailbox serialization is preserved.
type pairActorRequest struct {
	ready    chan pairActorReady
	proceed  chan error
	prepared chan pairActorPrepared
	complete chan error
	done     chan struct{}
}

type pairActorReady struct {
	actor *FarmActor
	err   error
}

type pairActorPrepared struct {
	commit      outbox.FarmCommit
	dirty       bool
	callbackErr error
}

// NewRuntime 创建 Actor 运行时。idleTTL 非正时使用 DefaultIdleTTL。
func NewRuntime(store FarmStore, idleTTL time.Duration) *Runtime {
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	r := &Runtime{
		store:         store,
		idleTTL:       idleTTL,
		flushInterval: DefaultFlushInterval,
		callTimeout:   DefaultCallTimeout,
		ioTimeout:     DefaultIOTimeout,
		actors:        make(map[uint64]*residentActor),
	}
	r.committers = []*Committer{NewCommitter(asBatchStore(store), defaultCommitterConfig())}
	r.pairCommitter = newPairCommitter(asBatchStore(store), defaultPairCommitterConfig(r.ioTimeout))
	return r
}

// SetCommitter 注入组提交器，供测试使用。必须在首次 Do 之前调用。
func (r *Runtime) SetCommitter(c *Committer) {
	if r == nil || len(r.committers) == 1 && r.committers[0] == c {
		return
	}
	for _, committer := range r.committers {
		if committer != nil {
			// 该方法约定在首次 Do 前调用，此时旧 committer 没有业务队列，
			// 可以同步关闭，避免测试注入时泄漏构造函数启动的 goroutine。
			_ = committer.Shutdown(context.Background())
		}
	}
	r.committers = []*Committer{c}
}

// SetCommitterShards replaces the single global durable queue with independent
// UID-affine queues. Per-UID ordering is preserved while Redis pipeline waits
// on one shard no longer stall unrelated actors. Call before the first Do.
func (r *Runtime) SetCommitterShards(shards int) {
	if r == nil {
		return
	}
	if shards <= 0 {
		shards = 1
	}
	for _, committer := range r.committers {
		if committer != nil {
			_ = committer.Shutdown(context.Background())
		}
	}
	r.committers = make([]*Committer, shards)
	for shard := range r.committers {
		r.committers[shard] = NewCommitter(asBatchStore(r.store), defaultCommitterConfig())
		if r.metrics != nil {
			r.committers[shard].SetMetrics(r.metrics)
		}
	}
}

// SetFlushInterval 覆盖写回间隔，供测试与压测调参使用。非正值恢复默认。
// 必须在首次 Do 之前调用：运行中的 Actor 会并发读这个字段。
func (r *Runtime) SetFlushInterval(interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	r.flushInterval = interval
}

// SetTimeouts 覆盖调用与存储 IO 超时，供测试使用。非正值恢复默认。
// 必须在首次 Do 之前调用：运行中的 Actor 会并发读这两个字段。
func (r *Runtime) SetTimeouts(call, io time.Duration) {
	if r == nil {
		return
	}
	if call <= 0 {
		call = DefaultCallTimeout
	}
	if io <= 0 {
		io = DefaultIOTimeout
	}
	r.callTimeout, r.ioTimeout = call, io
	if r.pairCommitter != nil {
		r.pairCommitter.config.IOTimeout = io
	}
}

// SetMaxResident installs a hard admission ceiling for cold Actor loads.
// A non-positive value disables the ceiling. Configure it before the first Do.
func (r *Runtime) SetMaxResident(maxResident int) {
	if r == nil {
		return
	}
	if maxResident < 0 {
		maxResident = 0
	}
	r.maxResident = maxResident
}

// SetHazardSalt 注入草/虫确定性哈希盐（由 FARM_HAZARD_SECRET 派生）。
// 必须在首次 Do 之前调用；加载聚合时写入 Aggregate.HazardSalt（不落盘）。
func (r *Runtime) SetHazardSalt(salt uint64) {
	if r == nil {
		return
	}
	r.hazardSalt = salt
}

// SetMetrics 注入 Actor 指标采集；可在任意时刻调用，观测为 best-effort。
func (r *Runtime) SetMetrics(m *telemetry.Metrics) {
	if r == nil {
		return
	}
	r.metrics = m
	for _, committer := range r.committers {
		if committer != nil {
			committer.SetMetrics(m)
		}
	}
	if r.pairCommitter != nil {
		r.pairCommitter.SetMetrics(m)
	}
}

// Do 将 fn 投递到 uid 对应的 Actor 串行区执行。
// 首次访问会加载聚合；Actor 空闲超过 idleTTL 时会持久化并从运行时卸载。
func (r *Runtime) Do(uid uint64, fn func(*FarmActor) error) error {
	if r == nil {
		return errors.New("actor: runtime is nil")
	}
	if r.store == nil {
		return errors.New("actor: farm store is nil")
	}
	if fn == nil {
		return errors.New("actor: callback is nil")
	}

	resultCh := requestResultPool.Get().(chan error)
	// A pooled channel is returned only after its sole response was received;
	// drain defensively so future refactors cannot leak a stale result.
	select {
	case <-resultCh:
	default:
	}
	defer requestResultPool.Put(resultCh)
	req := request{fn: fn, result: resultCh}
	deadline := time.NewTimer(r.callTimeout)
	defer deadline.Stop()

	for {
		resident, err := r.getOrStartActor(uid)
		if err != nil {
			if errors.Is(err, ErrCapacity) {
				if m := r.metrics; m != nil {
					m.ActorDoBusy.Inc()
				}
			}
			return err
		}
		depth := int(resident.waiters.Add(1))
		if m := r.metrics; m != nil {
			m.ObserveMailboxDepth(depth)
		}
		select {
		case resident.mailbox <- req:
			resident.waiters.Add(-1)
			return <-resultCh
		case <-resident.done:
			resident.waiters.Add(-1)
		case <-deadline.C:
			resident.waiters.Add(-1)
			if m := r.metrics; m != nil {
				m.ActorDoBusy.Inc()
			}
			return ErrBusy
		}
	}
}

// DoPairDurable executes one mutation while two UID-affine Actors are held in
// deterministic order, then appends both final mutations through one
// BatchFarmStore call. It is intended for colocated cross-farm commands where
// visitor reservation, owner adjudication and visitor settlement form one
// logical transaction in the reliable Redis journal.
//
// The callback must not call Runtime.Do/DoPairDurable recursively. Both Actor
// mailboxes are exclusively owned until the shared durable append completes.
func (r *Runtime) DoPairDurable(
	firstUID, secondUID uint64,
	fn func(first, second *FarmActor) error,
) error {
	if r == nil {
		return errors.New("actor: runtime is nil")
	}
	if r.store == nil {
		return errors.New("actor: farm store is nil")
	}
	if firstUID == 0 || secondUID == 0 || firstUID == secondUID {
		return errors.New("actor: invalid actor pair")
	}
	if fn == nil {
		return errors.New("actor: pair callback is nil")
	}

	lowUID, highUID := firstUID, secondUID
	if lowUID > highUID {
		lowUID, highUID = highUID, lowUID
	}
	low := newPairActorRequest()
	defer recyclePairActorRequest(low)
	if err := r.sendPairActorRequest(lowUID, low); err != nil {
		return err
	}
	lowReady := <-low.ready
	if lowReady.err != nil {
		<-low.done
		return lowReady.err
	}

	high := newPairActorRequest()
	defer recyclePairActorRequest(high)
	if err := r.sendPairActorRequest(highUID, high); err != nil {
		releasePairActor(low, err)
		return err
	}
	highReady := <-high.ready
	if highReady.err != nil {
		<-high.done
		releasePairActor(low, highReady.err)
		return highReady.err
	}

	firstActor, secondActor := lowReady.actor, highReady.actor
	if firstUID > secondUID {
		firstActor, secondActor = secondActor, firstActor
	}
	callbackErr := invokePairCallback(fn, firstActor, secondActor)
	low.proceed <- callbackErr
	high.proceed <- callbackErr
	lowPrepared := <-low.prepared
	highPrepared := <-high.prepared

	commitErr := callbackErr
	if commitErr == nil {
		commitErr = lowPrepared.callbackErr
		if commitErr == nil {
			commitErr = highPrepared.callbackErr
		}
	}
	if commitErr == nil {
		var commitBuffer [2]outbox.FarmCommit
		commits := commitBuffer[:0]
		if lowPrepared.dirty {
			commits = append(commits, lowPrepared.commit)
		}
		if highPrepared.dirty {
			commits = append(commits, highPrepared.commit)
		}
		if len(commits) > 0 {
			commitErr = r.commitPair(commits)
		}
	}
	low.complete <- commitErr
	high.complete <- commitErr
	<-low.done
	<-high.done
	if commitErr != nil {
		return fmt.Errorf("actor: commit pair %d/%d: %w", firstUID, secondUID, commitErr)
	}
	return nil
}

func newPairActorRequest() *pairActorRequest {
	return pairActorRequestPool.Get().(*pairActorRequest)
}

func recyclePairActorRequest(pair *pairActorRequest) {
	if pair != nil {
		pairActorRequestPool.Put(pair)
	}
}

func (r *Runtime) sendPairActorRequest(uid uint64, pair *pairActorRequest) error {
	deadline := time.NewTimer(r.callTimeout)
	defer deadline.Stop()
	for {
		resident, err := r.getOrStartActor(uid)
		if err != nil {
			return err
		}
		depth := int(resident.waiters.Add(1))
		if m := r.metrics; m != nil {
			m.ObserveMailboxDepth(depth)
		}
		select {
		case resident.mailbox <- request{pair: pair}:
			resident.waiters.Add(-1)
			return nil
		case <-resident.done:
			resident.waiters.Add(-1)
		case <-deadline.C:
			resident.waiters.Add(-1)
			if m := r.metrics; m != nil {
				m.ActorDoBusy.Inc()
			}
			return ErrBusy
		}
	}
}

func releasePairActor(pair *pairActorRequest, err error) {
	if pair == nil {
		return
	}
	pair.proceed <- err
	<-pair.prepared
	pair.complete <- err
	<-pair.done
}

func invokePairCallback(fn func(*FarmActor, *FarmActor) error, first, second *FarmActor) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("actor: pair callback panic: %v", recovered)
		}
	}()
	return fn(first, second)
}

func (r *Runtime) commitPair(commits []outbox.FarmCommit) error {
	if r.pairCommitter == nil {
		return errors.New("actor: pair committer is nil")
	}
	if pressure, ok := asBatchStore(r.store).(foregroundPressureStore); ok {
		pressure.AdjustForegroundPressure(1)
		defer pressure.AdjustForegroundPressure(-1)
	}
	return r.pairCommitter.Commit(commits)
}

// Shutdown 疏散全部驻留 Actor：拒绝新请求，让每个 Actor 处理完手上的请求后落盘
// 退出，并等待它们结束或 ctx 超时，最后排空组提交器。
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	r.draining = true
	r.drainCtx = ctx
	residents := make([]*residentActor, 0, len(r.actors))
	for _, resident := range r.actors {
		residents = append(residents, resident)
	}
	r.mu.Unlock()

	for _, resident := range residents {
		resident.stop()
	}

	finished := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		if r.drainFailed.Load() {
			return fmt.Errorf("actor: shutdown incomplete: %w", ctx.Err())
		}
	case <-ctx.Done():
		return fmt.Errorf("actor: shutdown incomplete: %w", ctx.Err())
	}

	var shutdownErrors []error
	for _, committer := range r.committers {
		if committer != nil {
			if err := committer.Shutdown(ctx); err != nil {
				shutdownErrors = append(shutdownErrors, err)
			}
		}
	}
	if r.pairCommitter != nil {
		if err := r.pairCommitter.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	return errors.Join(shutdownErrors...)
}

func (a *residentActor) stop() {
	a.stopOnce.Do(func() { close(a.drain) })
}

func (r *Runtime) getOrStartActor(uid uint64) (*residentActor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.draining {
		return nil, ErrDraining
	}
	resident := r.actors[uid]
	if resident == nil {
		if r.maxResident > 0 && len(r.actors) >= r.maxResident {
			return nil, ErrCapacity
		}
		resident = &residentActor{
			mailbox: make(chan request),
			done:    make(chan struct{}),
			drain:   make(chan struct{}),
		}
		r.actors[uid] = resident
		r.wg.Add(1)
		if m := r.metrics; m != nil {
			m.ActorResident.Inc()
		}
		go r.run(uid, resident)
	}
	return resident, nil
}

func (r *Runtime) run(uid uint64, resident *residentActor) {
	defer func() {
		r.mu.Lock()
		if r.actors[uid] == resident {
			delete(r.actors, uid)
		}
		r.mu.Unlock()
		if m := r.metrics; m != nil {
			m.ActorResident.Dec()
		}
		close(resident.done)
		r.wg.Done()
	}()

	idle := time.NewTimer(r.idleTTL)
	defer idle.Stop()
	flush := time.NewTicker(r.flushInterval)
	defer flush.Stop()

	var actor FarmActor
	var generation uint64
	var committedGen uint64
	var enqueuedGen uint64
	commitAcks := make(chan commitAck, 16)

	for {
		select {
		case req := <-resident.mailbox:
			if actor.Aggregate == nil {
				aggregate, err := r.load(uid)
				if err != nil {
					if req.pair != nil {
						req.pair.ready <- pairActorReady{err: err}
						req.pair.done <- struct{}{}
					} else {
						req.result <- err
					}
					return
				}
				actor.Aggregate = aggregate
			}
			if req.pair != nil {
				if err := r.ensurePairBaseline(uid, &actor, generation, &committedGen, &enqueuedGen, commitAcks); err != nil {
					req.pair.ready <- pairActorReady{err: err}
					req.pair.done <- struct{}{}
					resetTimer(idle, r.idleTTL)
					continue
				}
				req.pair.ready <- pairActorReady{actor: &actor}
				callbackErr := <-req.pair.proceed
				dirty := actor.consumeDirty()
				actor.syncFlush = false
				prepared := pairActorPrepared{dirty: dirty, callbackErr: callbackErr}
				if dirty {
					generation++
					actor.stampOutboxGeneration(generation)
					actor.stampSideEffectGeneration(generation)
					actor.stampPersistGeneration(generation)
					prepared.commit, prepared.callbackErr = r.buildPairCommit(&actor, prepared.callbackErr)
				}
				req.pair.prepared <- prepared
				commitErr := <-req.pair.complete
				if dirty && commitErr == nil {
					committedGen = generation
					enqueuedGen = generation
					actor.ackOutbox(committedGen)
					actor.ackSideEffects(committedGen)
					actor.ackPersistPlan(committedGen)
				} else if dirty {
					// The final mutation remains stamped and the ordinary flush ticker
					// will retry it; do not claim this generation was enqueued.
					enqueuedGen = committedGen
				}
				req.pair.done <- struct{}{}
				resetTimer(idle, r.idleTTL)
				continue
			}

			err, panicked := invokeCallback(req.fn, &actor)
			if panicked {
				req.result <- err
				return
			}
			if !actor.consumeDirty() {
				actor.syncFlush = false
				req.result <- err
				resetTimer(idle, r.idleTTL)
				continue
			}
			generation++
			actor.stampOutboxGeneration(generation)
			actor.stampSideEffectGeneration(generation)
			actor.stampPersistGeneration(generation)
			needSyncFlush := actor.syncFlush
			if needSyncFlush {
				actor.syncFlush = false
				if r.finishRequestAfterDurable(uid, &actor, generation, err, req.result, commitAcks) {
					enqueuedGen = generation
				}
			} else {
				req.result <- err
			}
			resetTimer(idle, r.idleTTL)

		case <-flush.C:
			if committedGen >= generation {
				continue
			}
			if enqueuedGen < generation && r.enqueueWriteBehind(uid, &actor, generation, commitAcks) {
				enqueuedGen = generation
			}

		case ack := <-commitAcks:
			if ack.err != nil {
				enqueuedGen = committedGen
				telemetry.L().Error("actor flush failed",
					"component", "actor",
					"op", "write_behind",
					"uid", uid,
					"err", ack.err.Error(),
				)
				continue
			}
			if ack.generation > committedGen {
				committedGen = ack.generation
				actor.ackOutbox(committedGen)
				actor.ackSideEffects(committedGen)
				actor.ackPersistPlan(committedGen)
			}

		case <-idle.C:
			if committedGen >= generation {
				return
			}
			if err := r.enqueueDurableAndWait(uid, &actor, generation); err != nil {
				telemetry.L().Error("actor flush failed",
					"component", "actor",
					"op", "idle_flush",
					"uid", uid,
					"err", err.Error(),
				)
				resetTimer(idle, r.idleTTL)
				continue
			}
			committedGen = generation
			enqueuedGen = generation
			actor.ackOutbox(committedGen)
			actor.ackSideEffects(committedGen)
			actor.ackPersistPlan(committedGen)
			return

		case <-resident.drain:
			drainCtx := r.drainCtx
			if drainCtx == nil {
				drainCtx = context.Background()
			}
			backoff := drainRetryMinBackoff
			for committedGen < generation {
				if err := r.enqueueDurableAndWait(uid, &actor, generation); err != nil {
					telemetry.L().Error("actor flush failed",
						"component", "actor",
						"op", "drain_flush",
						"uid", uid,
						"err", err.Error(),
					)
					timer := time.NewTimer(backoff)
					select {
					case <-timer.C:
						if backoff < drainRetryMaxBackoff {
							backoff *= 2
							if backoff > drainRetryMaxBackoff {
								backoff = drainRetryMaxBackoff
							}
						}
						continue
					case <-drainCtx.Done():
						r.drainFailed.Store(true)
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						return
					}
				}
				committedGen = generation
				enqueuedGen = generation
				actor.ackOutbox(committedGen)
				actor.ackSideEffects(committedGen)
				actor.ackPersistPlan(committedGen)
			}
			return
		}
	}
}

func (r *Runtime) enqueueSave(uid uint64, actor *FarmActor, generation uint64, durable bool) (<-chan CommitResult, error) {
	if len(r.committers) == 0 {
		return nil, errors.New("actor: committer is nil")
	}
	if actor == nil || actor.Aggregate == nil {
		return nil, errors.New("actor: aggregate is nil")
	}
	committer := r.committers[uid%uint64(len(r.committers))]
	if committer == nil {
		return nil, errors.New("actor: committer shard is nil")
	}
	if incremental, ok := r.store.(incrementalFarmStore); ok && incremental.SupportsIncrementalFarmCommits() {
		mutation, err := actor.pendingWriteMutation()
		if err != nil {
			return nil, err
		}
		return committer.EnqueueIncrementalPlan(uid, generation, mutation, actor.pendingPersistPlan(), durable)
	}
	snapshot := actor.Aggregate.Clone()
	return committer.EnqueueMutationPlan(uid, generation, snapshot,
		actor.pendingOutboxEvents(), actor.pendingTaskAdvances(), actor.pendingCodexRewards(),
		actor.pendingPersistPlan(), durable)
}

func (r *Runtime) enqueueWriteBehind(uid uint64, actor *FarmActor, generation uint64, commitAcks chan commitAck) bool {
	resultCh, err := r.enqueueSave(uid, actor, generation, false)
	if err != nil {
		telemetry.L().Error("actor enqueue write-behind failed",
			"component", "actor",
			"op", "write_behind_enqueue",
			"uid", uid,
			"err", err.Error(),
		)
		return false
	}
	go func(gen uint64) {
		res := <-resultCh
		select {
		case commitAcks <- commitAck{generation: res.Generation, err: res.Err}:
		default:
		}
	}(generation)
	return true
}

func (r *Runtime) enqueueDurableAndWait(uid uint64, actor *FarmActor, generation uint64) error {
	resultCh, err := r.enqueueSave(uid, actor, generation, true)
	if err != nil {
		return err
	}
	res := <-resultCh
	if res.Err != nil {
		return fmt.Errorf("actor: save farm %d: %w", uid, res.Err)
	}
	return nil
}

func (r *Runtime) finishRequestAfterDurable(
	uid uint64,
	actor *FarmActor,
	generation uint64,
	callbackErr error,
	result chan error,
	commitAcks chan commitAck,
) bool {
	resultCh, enqueueErr := r.enqueueSave(uid, actor, generation, true)
	if enqueueErr != nil {
		telemetry.L().Error("actor flush failed",
			"component", "actor",
			"op", "sync_flush_enqueue",
			"uid", uid,
			"err", enqueueErr.Error(),
		)
		if callbackErr == nil {
			callbackErr = enqueueErr
		}
		result <- callbackErr
		return false
	}
	go func(cbErr error, gen uint64) {
		res := <-resultCh
		if res.Err != nil {
			telemetry.L().Error("actor flush failed",
				"component", "actor",
				"op", "sync_flush",
				"uid", uid,
				"err", res.Err.Error(),
			)
			if cbErr == nil {
				cbErr = fmt.Errorf("actor: save farm %d: %w", uid, res.Err)
			}
		}
		result <- cbErr
		select {
		case commitAcks <- commitAck{generation: res.Generation, err: res.Err}:
		default:
		}
	}(callbackErr, generation)
	return true
}

func (r *Runtime) ensurePairBaseline(
	uid uint64,
	actor *FarmActor,
	generation uint64,
	committedGen, enqueuedGen *uint64,
	commitAcks chan commitAck,
) error {
	for *committedGen < generation {
		if *enqueuedGen < generation {
			*enqueuedGen = generation
			if err := r.enqueueDurableAndWait(uid, actor, generation); err != nil {
				*enqueuedGen = *committedGen
				return err
			}
			*committedGen = generation
			actor.ackOutbox(generation)
			actor.ackSideEffects(generation)
			actor.ackPersistPlan(generation)
			return nil
		}
		wait := time.NewTimer(r.ioTimeout)
		var ack commitAck
		select {
		case ack = <-commitAcks:
			if !wait.Stop() {
				select {
				case <-wait.C:
				default:
				}
			}
		case <-wait.C:
			*enqueuedGen = *committedGen
			return fmt.Errorf("actor: wait prior farm %d generation %d: %w", uid, generation, context.DeadlineExceeded)
		}
		if ack.err != nil {
			*enqueuedGen = *committedGen
			return fmt.Errorf("actor: wait prior farm %d generation %d: %w", uid, ack.generation, ack.err)
		}
		if ack.generation > *committedGen {
			*committedGen = ack.generation
			actor.ackOutbox(*committedGen)
			actor.ackSideEffects(*committedGen)
			actor.ackPersistPlan(*committedGen)
		}
	}
	return nil
}

func (r *Runtime) buildPairCommit(actor *FarmActor, callbackErr error) (outbox.FarmCommit, error) {
	if callbackErr != nil {
		return outbox.FarmCommit{}, callbackErr
	}
	if actor == nil || actor.Aggregate == nil {
		return outbox.FarmCommit{}, errors.New("actor: pair aggregate is nil")
	}
	if incremental, ok := r.store.(incrementalFarmStore); ok && incremental.SupportsIncrementalFarmCommits() {
		mutation, err := actor.pendingWriteMutation()
		if err != nil {
			return outbox.FarmCommit{}, err
		}
		return outbox.FarmCommit{Mutation: mutation, Plan: actor.pendingPersistPlan()}, nil
	}
	return outbox.FarmCommit{
		Snapshot:     actor.Aggregate.Clone(),
		Outbox:       actor.pendingOutboxEvents(),
		TaskAdvances: actor.pendingTaskAdvances(),
		CodexRewards: actor.pendingCodexRewards(),
		Plan:         actor.pendingPersistPlan(),
	}, nil
}

func (r *Runtime) load(uid uint64) (*farm.Aggregate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.ioTimeout)
	defer cancel()

	start := time.Now()
	aggregate, err := r.loadAggregate(ctx, uid)
	if err != nil {
		if m := r.metrics; m != nil {
			m.ObserveActorLoad(time.Since(start), err)
		}
		return nil, fmt.Errorf("actor: load farm %d: %w", uid, err)
	}
	if aggregate == nil {
		empty := fmt.Errorf("actor: load farm %d: empty aggregate", uid)
		if m := r.metrics; m != nil {
			m.ObserveActorLoad(time.Since(start), empty)
		}
		return nil, empty
	}
	if m := r.metrics; m != nil {
		m.ObserveActorLoad(time.Since(start), nil)
	}
	aggregate.HazardSalt = r.hazardSalt
	return aggregate, nil
}

func (r *Runtime) loadAggregate(ctx context.Context, uid uint64) (*farm.Aggregate, error) {
	if _, ok := r.store.(batchFarmStore); !ok {
		return r.store.LoadFarm(ctx, uid)
	}
	request := coldLoadRequest{uid: uid, result: make(chan coldLoadResult, 1)}
	r.loadMu.Lock()
	batch := r.loadBatch
	if batch == nil {
		batch = &coldLoadBatch{requests: make([]coldLoadRequest, 0, coldLoadBatchMax)}
		r.loadBatch = batch
		go r.flushColdLoadBatchAfter(batch, coldLoadBatchWindow)
	}
	batch.requests = append(batch.requests, request)
	full := len(batch.requests) >= coldLoadBatchMax
	if full && r.loadBatch == batch {
		// Detach the full batch before releasing the lock. New arrivals can form
		// the next batch instead of racing the immediate flusher past the cap.
		r.loadBatch = nil
	}
	r.loadMu.Unlock()
	if full {
		go r.flushColdLoadBatchAfter(batch, 0)
	}
	select {
	case result := <-request.result:
		return result.aggregate, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Runtime) flushColdLoadBatchAfter(batch *coldLoadBatch, delay time.Duration) {
	if delay > 0 {
		timer := time.NewTimer(delay)
		<-timer.C
	}
	batch.flush.Do(func() {
		r.loadMu.Lock()
		if r.loadBatch == batch {
			r.loadBatch = nil
		}
		requests := append([]coldLoadRequest(nil), batch.requests...)
		r.loadMu.Unlock()

		uids := make([]uint64, len(requests))
		for index, request := range requests {
			uids[index] = request.uid
		}
		loadCtx, cancel := context.WithTimeout(context.Background(), r.ioTimeout)
		loaded, err := r.store.(batchFarmStore).LoadFarms(loadCtx, uids)
		cancel()
		for _, request := range requests {
			result := coldLoadResult{aggregate: loaded[request.uid], err: err}
			if err == nil && result.aggregate == nil {
				result.err = fmt.Errorf("actor: batch load omitted farm %d", request.uid)
			}
			request.result <- result
		}
	})
}

func invokeCallback(fn func(*FarmActor) error, actor *FarmActor) (err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			err = fmt.Errorf("actor: callback panic: %v", recovered)
		}
	}()
	return fn(actor), false
}

func resetTimer(timer *time.Timer, ttl time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(ttl)
}
