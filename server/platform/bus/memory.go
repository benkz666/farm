package bus

import (
	"context"
	"errors"
	"sync"
)

// ErrBusClosed 表示总线已关闭，拒绝 Publish/Subscribe。
var ErrBusClosed = errors.New("bus: closed")

// MemoryBus 是进程内 EventBus 实现，仅供单元测试和局部组件测试使用。
//
// 投递语义：Publish 同步调用该 topic 下所有 handler，返回首个 handler 错误。
// 选同步而非异步是为了让单测确定性地断言投递结果；生产跨 Actor 协作仍走
// CrossResult 回环，不依赖 Publish 的同步返回。handler 在快照副本上调用，
// 避免持锁执行用户代码导致自死锁（如 handler 内再 Publish）。
//
// 并发安全：Publish/Subscribe/Close 可并发调用。
type MemoryBus struct {
	mu       sync.Mutex
	handlers map[string][]func(key string, payload []byte) error
	closed   bool
}

// NewMemoryBus 构造一个空的 MemoryBus。
func NewMemoryBus() *MemoryBus {
	return &MemoryBus{handlers: make(map[string][]func(key string, payload []byte) error)}
}

// Publish 同步投递消息给 topic 的所有订阅者。
func (b *MemoryBus) Publish(ctx context.Context, topic string, key string, payload []byte) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBusClosed
	}
	// 拷贝一份快照，避免持锁调用 handler。
	snapshot := append([]func(key string, payload []byte) error(nil), b.handlers[topic]...)
	b.mu.Unlock()

	for _, h := range snapshot {
		if err := h(key, payload); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe 注册 handler 到 topic。重复订阅同一 topic 会追加，每条消息广播给所有 handler。
func (b *MemoryBus) Subscribe(ctx context.Context, topic string, handler func(key string, payload []byte) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	b.handlers[topic] = append(b.handlers[topic], handler)
	return nil
}

// Close 关闭总线，释放订阅者引用。后续 Publish/Subscribe 返回 ErrBusClosed。
func (b *MemoryBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.handlers = nil
	return nil
}
