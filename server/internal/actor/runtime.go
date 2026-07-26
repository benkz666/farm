package actor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"farm/server/internal/farm"
)

const DefaultIdleTTL = 10 * time.Minute

// FarmStore 是 Actor 所需的最小持久化边界。
// 具体的 store.FarmStore 实现满足本接口，无需让 Actor 依赖账号存储方法。
type FarmStore interface {
	LoadFarm(ctx context.Context, uid uint64) (*farm.Aggregate, error)
	SaveFarm(ctx context.Context, aggregate *farm.Aggregate) error
}

// Runtime 管理本进程驻留的玩家 Actor。
type Runtime struct {
	store   FarmStore
	idleTTL time.Duration

	mu     sync.Mutex
	actors map[uint64]*residentActor
}

type residentActor struct {
	mailbox chan request
	done    chan struct{}
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
		store:   store,
		idleTTL: idleTTL,
		actors:  make(map[uint64]*residentActor),
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

	for {
		resident := r.getOrStartActor(uid)
		select {
		case resident.mailbox <- req:
			return <-req.result
		case <-resident.done:
			// Actor 正在完成 flush；重新查表后投递给下一代 Actor。
		}
	}
}

func (r *Runtime) getOrStartActor(uid uint64) *residentActor {
	r.mu.Lock()
	defer r.mu.Unlock()

	resident := r.actors[uid]
	if resident == nil {
		resident = &residentActor{
			mailbox: make(chan request),
			done:    make(chan struct{}),
		}
		r.actors[uid] = resident
		go r.run(uid, resident)
	}
	return resident
}

func (r *Runtime) run(uid uint64, resident *residentActor) {
	defer func() {
		r.mu.Lock()
		if r.actors[uid] == resident {
			delete(r.actors, uid)
		}
		r.mu.Unlock()
		close(resident.done)
	}()

	timer := time.NewTimer(r.idleTTL)
	defer timer.Stop()

	var actor FarmActor
	for {
		select {
		case req := <-resident.mailbox:
			if actor.Aggregate == nil {
				aggregate, err := r.store.LoadFarm(context.Background(), uid)
				if err != nil {
					req.result <- fmt.Errorf("actor: load farm %d: %w", uid, err)
					return
				}
				if aggregate == nil {
					req.result <- fmt.Errorf("actor: load farm %d: empty aggregate", uid)
					return
				}
				actor.Aggregate = aggregate
			}

			req.result <- req.fn(&actor)
			resetTimer(timer, r.idleTTL)

		case <-timer.C:
			if actor.Aggregate == nil {
				return
			}
			if err := r.store.SaveFarm(context.Background(), actor.Aggregate); err != nil {
				// 持久化失败时保留内存权威并重试，避免将尚未落盘的数据直接卸载。
				resetTimer(timer, r.idleTTL)
				continue
			}
			return
		}
	}
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
