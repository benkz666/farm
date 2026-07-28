package actor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/internal/farm"
	"farm/server/internal/obs"
)

const (
	// DefaultIdleTTL 是 Actor 无请求后卸载前的等待时长。
	DefaultIdleTTL = 10 * time.Minute

	// DefaultFlushInterval 是脏聚合的最长落盘延迟，也就是进程被强杀时的数据丢失
	// 上界。架构 5.3 节 C 档写回：用这个窗口合并同一玩家的连续改动，换掉「每个
	// 动作一次 MySQL 事务」。只在空闲时落盘是不够的——在线玩家的 Actor 永不空闲。
	DefaultFlushInterval = 30 * time.Second

	// DefaultCallTimeout 限制 Do 等待 Actor 接收请求的时长。
	// 只覆盖「尚未被接收」的阶段，所以超时返回时一定没有产生任何副作用。
	DefaultCallTimeout = 5 * time.Second

	// DefaultIOTimeout 限制单次加载或落盘的时长，让 Actor 串行区不会被慢存储卡死。
	DefaultIOTimeout = 3 * time.Second
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

// Runtime 管理本进程驻留的玩家 Actor。
type Runtime struct {
	store         FarmStore
	idleTTL       time.Duration
	flushInterval time.Duration
	callTimeout   time.Duration
	ioTimeout     time.Duration
	hazardSalt    uint64
	metrics       *obs.Metrics

	mu       sync.Mutex
	actors   map[uint64]*residentActor
	draining bool
	wg       sync.WaitGroup
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
	return &Runtime{
		store:         store,
		idleTTL:       idleTTL,
		flushInterval: DefaultFlushInterval,
		callTimeout:   DefaultCallTimeout,
		ioTimeout:     DefaultIOTimeout,
		actors:        make(map[uint64]*residentActor),
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
func (r *Runtime) SetMetrics(m *obs.Metrics) {
	if r == nil {
		return
	}
	r.metrics = m
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
			// 请求已被 Actor 取走，必然会有应答，这里不再设超时：否则会出现
			// 「调用方按失败处理、Actor 却已提交」的副作用。邮箱不带缓冲，加上
			// Actor 内部所有 IO 都有超时，保证这个等待是有界的。
			return <-req.result
		case <-resident.done:
			resident.waiters.Add(-1)
			// Actor 正在退出；重新查表后投递给下一代。
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
// 退出，并等待它们结束或 ctx 超时。
//
// 缺了这一步，一次正常发布就会丢掉上次写回之后的全部改动。
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	r.draining = true
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
		return nil
	case <-ctx.Done():
		return fmt.Errorf("actor: shutdown incomplete: %w", ctx.Err())
	}
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
	dirty := false

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
				// 回调 panic 后内存聚合可能不一致：卸载而不落盘，下次 Do 重新加载。
				req.result <- err
				return
			}
			// 保守地把每次回调都记为脏：惰性推进、跨农场结算的金币变动都不体现在
			// FarmSeq 上，靠比对字段判断脏位并不可靠，而多写一次远比少写一次安全。
			dirty = true
			if actor.syncFlush {
				actor.syncFlush = false
				if flushErr := r.flush(actor.Aggregate); flushErr != nil {
					// 调用方只会看到 ERR_INTERNAL，落盘失败的真实原因必须留在日志里。
					obs.L().Error("actor flush failed",
						"component", "actor",
						"op", "sync_flush",
						"err", flushErr.Error(),
					)
					// A 档语义：没落盘就不算成功，把存储错误交回调用方。
					if err == nil {
						err = flushErr
					}
				} else {
					dirty = false
				}
			}
			req.result <- err
			resetTimer(idle, r.idleTTL)

		case <-flush.C:
			if !dirty {
				continue
			}
			if err := r.flush(actor.Aggregate); err != nil {
				obs.L().Error("actor flush failed",
					"component", "actor",
					"op", "write_behind",
					"err", err.Error(),
				)
				continue
			}
			dirty = false

		case <-idle.C:
			if !dirty {
				return
			}
			if err := r.flush(actor.Aggregate); err != nil {
				// 落盘失败时保留内存权威并重试，避免卸载尚未落盘的数据。
				obs.L().Error("actor flush failed",
					"component", "actor",
					"op", "idle_flush",
					"err", err.Error(),
				)
				resetTimer(idle, r.idleTTL)
				continue
			}
			return

		case <-resident.drain:
			if dirty {
				if err := r.flush(actor.Aggregate); err != nil {
					// 疏散阶段落盘失败就是真丢数据，必须留下可追查的记录。
					obs.L().Error("actor flush failed",
						"component", "actor",
						"op", "drain_flush",
						"err", err.Error(),
					)
				}
			}
			return
		}
	}
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
	// 每次加载覆盖：盐是进程配置，不是玩家存档字段。
	aggregate.HazardSalt = r.hazardSalt
	return aggregate, nil
}

func (r *Runtime) flush(aggregate *farm.Aggregate) error {
	if aggregate == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.ioTimeout)
	defer cancel()

	start := time.Now()
	err := r.store.SaveFarm(ctx, aggregate)
	if m := r.metrics; m != nil {
		m.ObserveActorSave(time.Since(start), err)
	}
	if err != nil {
		return fmt.Errorf("actor: save farm %d: %w", aggregate.UID, err)
	}
	return nil
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
