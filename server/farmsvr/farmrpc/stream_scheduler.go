package farmrpc

import (
	"context"
	"runtime"
	"sync"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/telemetry"
)

const (
	// A Gateway owns one long-lived Farm stream. Fixed process-wide widths made
	// that single stream a hidden vertical-scaling ceiling: raising Farm from
	// three to six CPUs did not create any additional execution slots. Keep the
	// lane density proportional to GOMAXPROCS so a vertically scaled Farm can use
	// the CPU/DB capacity assigned to it without requiring more Gateway streams.
	streamNormalConcurrencyPerCPU   = 128
	streamBarrierConcurrencyPerCPU  = 64
	streamMinimumNormalConcurrency  = 64
	streamMinimumBarrierConcurrency = 32
	streamQueueMultiplier           = 64
)

type streamSchedulerSizing struct {
	normalConcurrency  int
	barrierConcurrency int
	normalCapacity     int
	barrierCapacity    int
}

func currentStreamSchedulerSizing() streamSchedulerSizing {
	return streamSchedulerSizingFor(runtime.GOMAXPROCS(0))
}

func streamSchedulerSizingFor(processors int) streamSchedulerSizing {
	processors = max(processors, 1)
	normal := max(streamMinimumNormalConcurrency, processors*streamNormalConcurrencyPerCPU)
	barrier := max(streamMinimumBarrierConcurrency, processors*streamBarrierConcurrencyPerCPU)
	return streamSchedulerSizing{
		normalConcurrency:  normal,
		barrierConcurrency: barrier,
		normalCapacity:     normal * streamQueueMultiplier,
		barrierCapacity:    barrier * streamQueueMultiplier,
	}
}

type farmStreamLane uint8

const (
	farmStreamNormal farmStreamLane = iota
	farmStreamBarrier
)

func (lane farmStreamLane) label() string {
	if lane == farmStreamBarrier {
		return "barrier"
	}
	return "normal"
}

type scheduledStreamRequest struct {
	request  *farmv1.StreamExecuteRequest
	lane     farmStreamLane
	queuedAt time.Time
}

type streamUIDQueue struct {
	items []scheduledStreamRequest
}

// streamRequestScheduler preserves arrival order for every UID while allowing
// unrelated UIDs to execute independently. Each UID owns at most one runner;
// runners acquire either the normal or barrier semaphore for each command.
// This is deliberately per gRPC stream because request arrival order is owned
// by that stream. Runtime.Do remains the cross-stream serialization boundary.
type streamRequestScheduler struct {
	ctx       context.Context
	server    *CommandServer
	completed chan<- *farmv1.StreamExecuteResponse
	done      <-chan struct{}
	metrics   *telemetry.Metrics

	permits [2]chan struct{}
	limits  [2]int

	mu      sync.Mutex
	queues  map[uint64]*streamUIDQueue
	pending [2]int
	wg      sync.WaitGroup
}

func newStreamRequestScheduler(
	ctx context.Context,
	server *CommandServer,
	completed chan<- *farmv1.StreamExecuteResponse,
	done <-chan struct{},
) *streamRequestScheduler {
	return newStreamRequestSchedulerWithSizing(
		ctx, server, completed, done, currentStreamSchedulerSizing(),
	)
}

func newStreamRequestSchedulerWithSizing(
	ctx context.Context,
	server *CommandServer,
	completed chan<- *farmv1.StreamExecuteResponse,
	done <-chan struct{},
	sizing streamSchedulerSizing,
) *streamRequestScheduler {
	return &streamRequestScheduler{
		ctx: ctx, server: server, completed: completed, done: done, metrics: server.metrics,
		permits: [2]chan struct{}{
			make(chan struct{}, sizing.normalConcurrency),
			make(chan struct{}, sizing.barrierConcurrency),
		},
		limits: [2]int{sizing.normalCapacity, sizing.barrierCapacity},
		queues: make(map[uint64]*streamUIDQueue),
	}
}

// Submit never lets a saturated barrier lane backpressure the stream receiver:
// only that request is rejected. Normal commands in the same mixed batch can
// still enter their independent lane. A rejected claim has no side effect and
// remains safe for the client to retry under the existing idempotent DB claim.
func (scheduler *streamRequestScheduler) Submit(request *farmv1.StreamExecuteRequest) bool {
	select {
	case <-scheduler.done:
		return false
	case <-scheduler.ctx.Done():
		return false
	default:
	}

	lane := streamLaneForRequest(request)
	uid := streamRouteUID(request)
	item := scheduledStreamRequest{request: request, lane: lane, queuedAt: time.Now()}

	scheduler.mu.Lock()
	if scheduler.pending[lane] >= scheduler.limits[lane] {
		scheduler.mu.Unlock()
		if scheduler.metrics != nil {
			scheduler.metrics.ObserveFarmStreamRejected(lane.label())
		}
		return scheduler.deliver(scheduler.server.rejectedBatchRequest(request))
	}
	scheduler.pending[lane]++
	queue := scheduler.queues[uid]
	start := queue == nil
	if start {
		queue = &streamUIDQueue{}
		scheduler.queues[uid] = queue
		scheduler.wg.Add(1)
	}
	queue.items = append(queue.items, item)
	scheduler.mu.Unlock()

	if scheduler.metrics != nil {
		scheduler.metrics.ObserveFarmStreamQueued(lane.label())
	}
	if start {
		if scheduler.metrics != nil {
			scheduler.metrics.AddFarmStreamActiveSequencer(1)
		}
		go scheduler.runUID(uid)
	}
	return true
}

func (scheduler *streamRequestScheduler) Wait() {
	scheduler.wg.Wait()
}

func (scheduler *streamRequestScheduler) runUID(uid uint64) {
	defer scheduler.wg.Done()
	defer scheduler.abandonUID(uid)
	defer func() {
		if scheduler.metrics != nil {
			scheduler.metrics.AddFarmStreamActiveSequencer(-1)
		}
	}()

	for {
		item, ok := scheduler.next(uid)
		if !ok {
			return
		}
		permit := scheduler.permits[item.lane]
		select {
		case permit <- struct{}{}:
			if scheduler.metrics != nil {
				scheduler.metrics.ObserveFarmStreamStarted(item.lane.label(), time.Since(item.queuedAt))
			}
			response := scheduler.server.executeBatchRequest(scheduler.ctx, item.request)
			if scheduler.metrics != nil {
				scheduler.metrics.ObserveFarmStreamFinished(item.lane.label())
			}
			<-permit
			scheduler.finish(item.lane)
			if !scheduler.deliver(response) {
				return
			}
		case <-scheduler.done:
			scheduler.drop(item.lane)
			return
		case <-scheduler.ctx.Done():
			scheduler.drop(item.lane)
			return
		}
	}
}

func (scheduler *streamRequestScheduler) abandonUID(uid uint64) {
	scheduler.mu.Lock()
	queue := scheduler.queues[uid]
	delete(scheduler.queues, uid)
	if queue != nil {
		for _, item := range queue.items {
			if scheduler.pending[item.lane] > 0 {
				scheduler.pending[item.lane]--
			}
		}
	}
	scheduler.mu.Unlock()
	if scheduler.metrics != nil && queue != nil {
		for _, item := range queue.items {
			scheduler.metrics.ObserveFarmStreamDropped(item.lane.label(), false)
		}
	}
}

func (scheduler *streamRequestScheduler) next(uid uint64) (scheduledStreamRequest, bool) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	queue := scheduler.queues[uid]
	if queue == nil || len(queue.items) == 0 {
		delete(scheduler.queues, uid)
		return scheduledStreamRequest{}, false
	}
	item := queue.items[0]
	queue.items[0] = scheduledStreamRequest{}
	queue.items = queue.items[1:]
	return item, true
}

func (scheduler *streamRequestScheduler) finish(lane farmStreamLane) {
	scheduler.mu.Lock()
	if scheduler.pending[lane] > 0 {
		scheduler.pending[lane]--
	}
	scheduler.mu.Unlock()
}

func (scheduler *streamRequestScheduler) drop(lane farmStreamLane) {
	scheduler.finish(lane)
	if scheduler.metrics != nil {
		scheduler.metrics.ObserveFarmStreamDropped(lane.label(), false)
	}
}

func (scheduler *streamRequestScheduler) deliver(response *farmv1.StreamExecuteResponse) bool {
	select {
	case scheduler.completed <- response:
		return true
	case <-scheduler.done:
		return false
	case <-scheduler.ctx.Done():
		return false
	}
}

func streamRouteUID(request *farmv1.StreamExecuteRequest) uint64 {
	if request == nil {
		return 0
	}
	if request.Request != nil && request.Request.RouteUid != 0 {
		return request.Request.RouteUid
	}
	if request.FastSyncUid != 0 {
		return request.FastSyncUid
	}
	// Invalid requests do not own an Actor ordering domain. RequestId keeps a
	// malformed burst from serializing behind the synthetic UID zero.
	return request.RequestId
}

func streamLaneForRequest(request *farmv1.StreamExecuteRequest) farmStreamLane {
	if request == nil || request.Request == nil || request.Request.Envelope == nil {
		return farmStreamNormal
	}
	switch request.Request.Envelope.Cmd {
	case 602, 608, 614:
		return farmStreamBarrier
	default:
		return farmStreamNormal
	}
}

func (server *CommandServer) rejectedBatchRequest(
	request *farmv1.StreamExecuteRequest,
) *farmv1.StreamExecuteResponse {
	response := &farmv1.StreamExecuteResponse{RequestId: request.GetRequestId()}
	if request != nil && request.FastSyncUid != 0 {
		response.Response = &farmv1.ClientCommandResponse{
			Envelope: errorEnvelope(204, request.FastSyncClientSeq, errcode.RateLimited),
		}
		return response
	}
	response.Response = errorClientResponse(request.GetRequest(), errcode.RateLimited)
	return response
}
