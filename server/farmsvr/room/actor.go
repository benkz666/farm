// Package room 提供按玩家 uid 串行执行农场操作的进程内运行时。
package room

import (
	"farm/server/domain/farm"
	"farm/server/shared/outbox"
)

// FarmActor 持有单个玩家在内存中的农场聚合。
//
// Aggregate 只能在 Runtime.Do 的回调中访问；Runtime 保证同一 uid 的回调不会并发执行。
type FarmActor struct {
	Aggregate *farm.Aggregate
	Deltas    farm.DeltaRing

	// results 是跨农场回执的进程内热缓存；跨 Actor 卸载的幂等性由 Aggregate
	// 持久化的 CrossReceipts 保证，这里仅避免短时间重投反复查聚合。
	results resultCache
	// syncFlush 由 RequireFlush 置位，Runtime 在回调返回后据此同步落盘。
	syncFlush bool
	// dirty 表示当前回调是否真实改动了需持久化的聚合状态。
	dirty bool
	// outboxEvents 记录尚未 durable ack 的跨农场结果等事件。
	outboxEvents []stampedOutboxEvent
	outboxTail   int
}

// MarkDirty 标记当前回调已真实改动聚合；纯读路径不得调用。
func (a *FarmActor) MarkDirty() {
	if a == nil {
		return
	}
	a.dirty = true
}

func (a *FarmActor) consumeDirty() bool {
	if a == nil {
		return false
	}
	dirty := a.dirty
	a.dirty = false
	return dirty
}

// RequireFlush 要求 Runtime 在本次回调返回前把聚合同步写入存储，写失败则整次
// 调用报错。
func (a *FarmActor) RequireFlush() {
	if a == nil {
		return
	}
	a.syncFlush = true
	a.MarkDirty()
}

type stampedOutboxEvent struct {
	generation uint64
	event      outbox.Event
}

// RecordOutbox queues a durable outbox event for the current callback generation.
func (a *FarmActor) RecordOutbox(event outbox.Event) {
	if a == nil || event.EventID == "" {
		return
	}
	a.outboxEvents = append(a.outboxEvents, stampedOutboxEvent{event: event})
	a.MarkDirty()
}

func (a *FarmActor) stampOutboxGeneration(generation uint64) {
	for i := a.outboxTail; i < len(a.outboxEvents); i++ {
		a.outboxEvents[i].generation = generation
	}
	a.outboxTail = len(a.outboxEvents)
}

func (a *FarmActor) pendingOutboxEvents() []outbox.Event {
	if a == nil || len(a.outboxEvents) == 0 {
		return nil
	}
	events := make([]outbox.Event, 0, len(a.outboxEvents))
	for _, stamped := range a.outboxEvents {
		if stamped.generation == 0 {
			continue
		}
		events = append(events, stamped.event)
	}
	return events
}

func (a *FarmActor) ackOutbox(committedGen uint64) {
	if a == nil || committedGen == 0 || len(a.outboxEvents) == 0 {
		return
	}
	kept := a.outboxEvents[:0]
	for _, stamped := range a.outboxEvents {
		if stamped.generation > committedGen {
			kept = append(kept, stamped)
		}
	}
	a.outboxEvents = kept
	a.outboxTail = len(a.outboxEvents)
}

// CachedResult 读取先前缓存的跨农场结果，用于快速识别重复投递的同一请求。
func (a *FarmActor) CachedResult(reqID uint64) (any, bool) {
	if a == nil {
		return nil, false
	}
	return a.results.get(reqID)
}

// CacheResult 记录一次跨农场请求的处理结果，供热路径上的重复投递直接复用。
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
