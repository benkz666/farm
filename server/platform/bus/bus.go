// Package bus 提供跨农场事件总线的抽象接口与可注入实现。
//
// 期 4 多 Farm 拓扑（规格 2026-07-27-phase4-social-loop.md §3）：
//   - 跨农场动作一律走 EventBus：CrossAction → 主人裁决 → CrossResult
//   - 独立服务进程使用 Kafka；单元测试可注入 MemoryBus
//   - 禁止 Actor 同步等待另一 Actor，故 Publish 不应在调用者栈上阻塞业务
//
// 本包只定义接口与 Topics 常量，具体实现见 memory.go / kafka.go。
package bus

import "context"

// EventBus 是跨农场事件总线的抽象接口。
//
// 语义：
//   - Publish 将 (topic, key, payload) 投递给该 topic 的所有订阅者；
//     key 用于分区路由（Kafka 按 key 哈希到 partition，便于同主人事件有序）。
//   - Subscribe 注册一个 handler；handler 返回 error 视为该条投递失败。
//   - Close 释放底层资源；关闭后 Publish/Subscribe 返回 ErrBusClosed。
//
// 实现可同步（MemoryBus）或异步（KafkaBus），但调用方不应假设投递时序，
// 跨 Actor 协作必须依赖 CrossResult 回环而非 Publish 的同步返回。
type EventBus interface {
	Publish(ctx context.Context, topic string, key string, payload []byte) error
	Subscribe(ctx context.Context, topic string, handler func(key string, payload []byte) error) error
	Close() error
}

// Topics 常量对齐规格 §3 跨农场总线通道。
// 可按环境加前缀（如 dev.）以隔离命名空间，但本期默认裸名。
const (
	TopicCrossAction = "cross.action" // 访客 → 主人：CrossAction 请求
	TopicCrossResult = "cross.result" // 主人 → 访客：CrossResult 回环
)
