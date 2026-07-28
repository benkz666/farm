// Package actor 提供按玩家 uid 串行执行农场操作的进程内运行时。
package actor

import "farm/server/internal/farm"

// FarmActor 持有单个玩家在内存中的农场聚合。
//
// Aggregate 只能在 Runtime.Do 的回调中访问；Runtime 保证同一 uid 的回调不会并发执行。
type FarmActor struct {
	Aggregate *farm.Aggregate
	Deltas    farm.DeltaRing

	// results 缓存跨农场动作的幂等结果。它挂在 Actor 上而不是某个全局表里，
	// 这样内存随 Actor 卸载一起回收，不会随在线过的玩家数无限增长。
	results resultCache
	// syncFlush 由 RequireFlush 置位，Runtime 在回调返回后据此同步落盘。
	syncFlush bool
}

// RequireFlush 要求 Runtime 在本次回调返回前把聚合同步写入存储，写失败则整次
// 调用报错。
//
// 用于「玩家已经付出代价」的 A 档操作，例如购买与扩地（架构 5.3 节）：这类改动
// 不能落在写回窗口里，否则进程被强杀后金币回来了、买到的东西没了。
func (a *FarmActor) RequireFlush() {
	if a == nil {
		return
	}
	a.syncFlush = true
}

// CachedResult 读取先前缓存的跨农场结果，用于识别重复投递的同一请求。
func (a *FarmActor) CachedResult(reqID uint64) (any, bool) {
	if a == nil {
		return nil, false
	}
	return a.results.get(reqID)
}

// CacheResult 记录一次跨农场请求的处理结果，供重复投递时直接复用。
func (a *FarmActor) CacheResult(reqID uint64, result any) {
	if a == nil {
		return
	}
	a.results.put(reqID, result)
}

// resultCacheCapacity 是每个 Actor 保留的幂等结果条数。
//
// 只需覆盖事件总线的重投窗口：同一 (owner, req_id) 在短时间内可能被投递多次，
// 但不会在几十条其他请求之后再回来。
const resultCacheCapacity = 64

// resultCache 是固定容量的先进先出缓存。容量很小，用切片顺序扫描比维护链表更简单。
type resultCache struct {
	order   []uint64
	entries map[uint64]any
}

func (c *resultCache) get(reqID uint64) (any, bool) {
	if c == nil || c.entries == nil {
		return nil, false
	}
	result, ok := c.entries[reqID]
	return result, ok
}

func (c *resultCache) put(reqID uint64, result any) {
	if c == nil {
		return
	}
	if c.entries == nil {
		c.entries = make(map[uint64]any, resultCacheCapacity)
		c.order = make([]uint64, 0, resultCacheCapacity)
	}
	if _, exists := c.entries[reqID]; exists {
		c.entries[reqID] = result
		return
	}
	if len(c.order) >= resultCacheCapacity {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, reqID)
	c.entries[reqID] = result
}
