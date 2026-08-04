package room

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/domain/farm"
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
)

var (
	// ErrDraining 表示运行时正在疏散，不再接收新请求。
	// 调用方应按协议 ERR_REDIRECT 让客户端重连到新实例。
	ErrDraining = errors.New("actor: runtime is draining")

	// ErrBusy 表示 Actor 在 callTimeout 内没能接收这个请求。
	// 请求未被接收，因此没有任何副作用，调用方可安全重试。
	ErrBusy = errors.New("actor: actor is busy")
)

// FarmStore 是 Actor 所需的最小持久化边界。
// 具体的 store.FarmStore 实现满足本接口，无需让 Actor 依赖账号存储方法。
type FarmStore interface {
	LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error)
	SaveFarm(ctx context.Context, aggregate *farm.Aggregate) error
}

type commitAck struct {
	generation uint64
	err        error
}

// Runtime 管理本进程驻留的玩家 Actor。
type Runtime struct {
	store         FarmStore
	committer     *Committer
	idleTTL       time.Duration
	flushInterval time.Duration
	callTimeout   time.Duration
	ioTimeout     time.Duration
	hazardSalt    uint64
	metrics       *telemetry.Metrics

	mu          sync.Mutex
	actors      map[uint64]*residentActor
	draining    bool
	drainCtx    context.Context
	drainFailed atomic.Bool
	wg          sync.WaitGroup
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
	r.committer = NewCommitter(asBatchStore(store), defaultCommitterConfig())
	return r
}

// SetCommitter 注入组提交器，供测试使用。必须在首次 Do 之前调用。
func (r *Runtime) SetCommitter(c *Committer) {
	if r == nil || r.committer == c {
		return
	}
	if r.committer != nil {
		// 该方法约定在首次 Do 前调用，此时旧 committer 没有业务队列，
		// 可以同步关闭，避免测试注入时泄漏构造函数启动的 goroutine。
		_ = r.committer.Shutdown(context.Background())
	}
	r.committer = c
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
	if r.committer != nil {
		r.committer.SetMetrics(m)
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

	req := request{
		fn:     fn,
		result: make(chan error, 1),
	}
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
		case resident.mailbox <- req:
			resident.waiters.Add(-1)
			return <-req.result
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

	if r.committer != nil {
		if err := r.committer.Shutdown(ctx); err != nil {
			return err
		}
	}
	return nil
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
	commitAcks := make(chan commitAck, 16)

	for {
		select {
		case req := <-resident.mailbox:
			if actor.Aggregate == nil {
				aggregate, err := r.load(uid)
				if err != nil {
					req.result <- err
					return
				}
				actor.Aggregate = aggregate
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
			needSyncFlush := actor.syncFlush
			if needSyncFlush {
				actor.syncFlush = false
				r.finishRequestAfterDurable(uid, &actor, generation, err, req.result, commitAcks)
			} else {
				req.result <- err
			}
			resetTimer(idle, r.idleTTL)

		case <-flush.C:
			if committedGen >= generation {
				continue
			}
			r.enqueueWriteBehind(uid, &actor, generation, commitAcks)

		case ack := <-commitAcks:
			if ack.err != nil {
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
			actor.ackOutbox(committedGen)
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
				actor.ackOutbox(committedGen)
			}
			return
		}
	}
}

func (r *Runtime) enqueueSave(uid uint64, actor *FarmActor, generation uint64, durable bool) (<-chan CommitResult, error) {
	if r.committer == nil {
		return nil, errors.New("actor: committer is nil")
	}
	if actor == nil || actor.Aggregate == nil {
		return nil, errors.New("actor: aggregate is nil")
	}
	snapshot := actor.Aggregate.Clone()
	return r.committer.Enqueue(uid, generation, snapshot, actor.pendingOutboxEvents(), durable)
}

func (r *Runtime) enqueueWriteBehind(uid uint64, actor *FarmActor, generation uint64, commitAcks chan commitAck) {
	resultCh, err := r.enqueueSave(uid, actor, generation, false)
	if err != nil {
		telemetry.L().Error("actor enqueue write-behind failed",
			"component", "actor",
			"op", "write_behind_enqueue",
			"uid", uid,
			"err", err.Error(),
		)
		return
	}
	go func(gen uint64) {
		res := <-resultCh
		select {
		case commitAcks <- commitAck{generation: res.Generation, err: res.Err}:
		default:
		}
	}(generation)
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
) {
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
		return
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
}

func (r *Runtime) load(uid uint64) (*farm.Aggregate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.ioTimeout)
	defer cancel()

	start := time.Now()
	aggregate, err := r.store.LoadFarm(ctx, uid)
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
