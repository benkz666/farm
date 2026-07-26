// Package actor 提供按玩家 uid 串行执行农场操作的进程内运行时。
package actor

import "farm/server/internal/farm"

// FarmActor 持有单个玩家在内存中的农场聚合。
//
// Aggregate 只能在 Runtime.Do 的回调中访问；Runtime 保证同一 uid 的回调不会并发执行。
type FarmActor struct {
	Aggregate *farm.Aggregate
}
