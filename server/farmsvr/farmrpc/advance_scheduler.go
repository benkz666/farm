package farmrpc

import (
	"container/heap"
	"sync"
	"time"
)

const (
	advanceWorkerLimit = 32
	advanceRetryDelay  = 2 * time.Second
)

type scheduledAdvance struct {
	uid uint64
	due int64
}

type advanceHeap []scheduledAdvance

func (items advanceHeap) Len() int           { return len(items) }
func (items advanceHeap) Less(i, j int) bool { return items[i].due < items[j].due }
func (items advanceHeap) Swap(i, j int)      { items[i], items[j] = items[j], items[i] }
func (items *advanceHeap) Push(value any)    { *items = append(*items, value.(scheduledAdvance)) }
func (items *advanceHeap) Pop() any {
	old := *items
	value := old[len(old)-1]
	*items = old[:len(old)-1]
	return value
}

// farmAdvanceScheduler is a process-local timing heap for active farms.
// Changed deadlines leave one stale heap entry that is discarded lazily. An
// unchanged deadline is not pushed again, which keeps repeated reads from
// growing the heap while retaining O(log n) mutation cost.
type farmAdvanceScheduler struct {
	now      func() int64
	advance  func(uint64)
	mu       sync.Mutex
	items    advanceHeap
	deadline map[uint64]int64
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	workers  chan struct{}
	workerWG sync.WaitGroup
}

func newFarmAdvanceScheduler(now func() int64, advance func(uint64)) *farmAdvanceScheduler {
	scheduler := &farmAdvanceScheduler{
		now:      now,
		advance:  advance,
		deadline: make(map[uint64]int64),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		workers:  make(chan struct{}, advanceWorkerLimit),
	}
	heap.Init(&scheduler.items)
	go scheduler.run()
	return scheduler
}

func (scheduler *farmAdvanceScheduler) Schedule(uid uint64, due int64) {
	if scheduler == nil || uid == 0 {
		return
	}
	scheduler.mu.Lock()
	if due <= 0 {
		delete(scheduler.deadline, uid)
	} else {
		if current, ok := scheduler.deadline[uid]; ok && current == due {
			scheduler.mu.Unlock()
			return
		}
		scheduler.deadline[uid] = due
		heap.Push(&scheduler.items, scheduledAdvance{uid: uid, due: due})
	}
	scheduler.mu.Unlock()
	select {
	case scheduler.wake <- struct{}{}:
	default:
	}
}

func (scheduler *farmAdvanceScheduler) Close() {
	if scheduler == nil {
		return
	}
	scheduler.once.Do(func() { close(scheduler.stop) })
	<-scheduler.done
	scheduler.workerWG.Wait()
}

func (scheduler *farmAdvanceScheduler) run() {
	defer close(scheduler.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		due, ok := scheduler.next()
		if !ok {
			select {
			case <-scheduler.wake:
				continue
			case <-scheduler.stop:
				return
			}
		}
		delayMillis := due.due - scheduler.now()
		if delayMillis > 0 {
			resetAdvanceTimer(timer, time.Duration(delayMillis)*time.Millisecond)
			select {
			case <-timer.C:
			case <-scheduler.wake:
				continue
			case <-scheduler.stop:
				return
			}
		}
		if !scheduler.claim(due) {
			continue
		}
		select {
		case scheduler.workers <- struct{}{}:
			scheduler.workerWG.Add(1)
			go func(uid uint64) {
				defer func() {
					<-scheduler.workers
					scheduler.workerWG.Done()
				}()
				scheduler.advance(uid)
			}(due.uid)
		case <-scheduler.stop:
			return
		}
	}
}

func (scheduler *farmAdvanceScheduler) next() (scheduledAdvance, bool) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	for scheduler.items.Len() > 0 {
		candidate := scheduler.items[0]
		if scheduler.deadline[candidate.uid] != candidate.due {
			heap.Pop(&scheduler.items)
			continue
		}
		return candidate, true
	}
	return scheduledAdvance{}, false
}

func (scheduler *farmAdvanceScheduler) claim(candidate scheduledAdvance) bool {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if scheduler.items.Len() == 0 || scheduler.deadline[candidate.uid] != candidate.due {
		return false
	}
	heap.Pop(&scheduler.items)
	delete(scheduler.deadline, candidate.uid)
	return true
}

func resetAdvanceTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}
