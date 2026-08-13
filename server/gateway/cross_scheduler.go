package gateway

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	crossWorkerCount = 128
	crossQueueSize   = 32768
	crossWheelTick   = 25 * time.Millisecond
	crossWheelSlots  = 256
)

var gatewayCrossScheduler = newCrossScheduler()

// crossScheduler bounds the amount of execution state retained by bursty
// cross-farm traffic. Requests share a fixed worker set and one hashed timeout
// wheel instead of allocating one goroutine and one runtime Timer each.
type crossScheduler struct {
	work   chan func()
	wheel  [crossWheelSlots]crossTimerBucket
	cursor atomic.Uint64
}

type crossTimerBucket struct {
	mu      sync.Mutex
	entries []crossTimerEntry
}

type crossTimerEntry struct {
	gateway  *Gateway
	reqID    uint64
	deadline time.Time
}

func newCrossScheduler() *crossScheduler {
	scheduler := &crossScheduler{work: make(chan func(), crossQueueSize)}
	for range crossWorkerCount {
		go scheduler.runWorker()
	}
	go scheduler.runWheel()
	return scheduler
}

func (scheduler *crossScheduler) submit(task func()) bool {
	if scheduler == nil || task == nil {
		return false
	}
	select {
	case scheduler.work <- task:
		return true
	default:
		return false
	}
}

func (scheduler *crossScheduler) runWorker() {
	for task := range scheduler.work {
		func() {
			defer func() { _ = recover() }()
			task()
		}()
	}
}

func (scheduler *crossScheduler) scheduleTimeout(gateway *Gateway, reqID uint64, deadline time.Time) {
	if scheduler == nil || gateway == nil || reqID == 0 {
		return
	}
	remaining := time.Until(deadline)
	ticks := uint64(1)
	if remaining > crossWheelTick {
		ticks = uint64((remaining + crossWheelTick - 1) / crossWheelTick)
	}
	slot := (scheduler.cursor.Load() + ticks) % crossWheelSlots
	bucket := &scheduler.wheel[slot]
	bucket.mu.Lock()
	bucket.entries = append(bucket.entries, crossTimerEntry{gateway: gateway, reqID: reqID, deadline: deadline})
	bucket.mu.Unlock()
}

func (scheduler *crossScheduler) runWheel() {
	ticker := time.NewTicker(crossWheelTick)
	defer ticker.Stop()
	for now := range ticker.C {
		slot := scheduler.cursor.Add(1) % crossWheelSlots
		bucket := &scheduler.wheel[slot]
		bucket.mu.Lock()
		entries := bucket.entries
		bucket.entries = nil
		bucket.mu.Unlock()
		for _, entry := range entries {
			if now.Before(entry.deadline) {
				scheduler.scheduleTimeout(entry.gateway, entry.reqID, entry.deadline)
				continue
			}
			entry.gateway.timeoutCrossAction(entry.reqID)
		}
	}
}
