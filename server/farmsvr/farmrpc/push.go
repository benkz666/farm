package farmrpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

const (
	// Bound concurrent Gateway callbacks so one Publish cannot spawn unbounded
	// goroutines when a room spans many Gateways.
	maxParallelGatewayPush  = 16
	deltaPushMaxAttempts    = 3
	deltaPushRetryBaseDelay = 50 * time.Millisecond

	taskNotifyCoalesceWindow  = 75 * time.Millisecond
	maxPendingTaskNotifies    = 4096
	defaultDeltaQueueShards   = 16
	defaultDeltaQueuePerShard = 1024
)

// DeltaPushRequest is the legacy single-connection Farm→Gateway callback.
// Prefer PushBatch for new fan-out paths.
type DeltaPushRequest struct {
	ConnectionID uint64         `json:"connection_id"`
	Delta        farm.FarmDelta `json:"delta"`
}

// PushBatch is the internal Gateway push unit from protocol.md §2.5. Delta is
// the production typed payload; Envelope remains as an in-process test seam.
type PushBatch struct {
	ConnIDs  []uint64            `json:"conn_ids"`
	Delta    *publicv3.FarmDelta `json:"-"`
	Envelope []byte              `json:"envelope,omitempty"`
}

// PlayerDeltaPushRequest is sent to the Gateway that owns a player's WebSocket.
// UID is repeated so the Gateway can reject a stale or forged connection ID.
type PlayerDeltaPushRequest struct {
	ConnectionID uint64           `json:"connection_id"`
	UID          uint64           `json:"uid"`
	Delta        farm.PlayerDelta `json:"delta"`
}

// TaskNotifyPushRequest is sent to the Gateway that owns a player's WebSocket.
// UID is repeated so the Gateway can reject a stale or forged connection ID.
type TaskNotifyPushRequest struct {
	ConnectionID uint64     `json:"connection_id"`
	UID          uint64     `json:"uid"`
	Task         store.Task `json:"task"`
}

// MailNotifyPushRequest is a refresh hint for the Gateway owning one player
// session. Kind is advisory only; clients reload MailList for authority.
type MailNotifyPushRequest struct {
	ConnectionID uint64 `json:"connection_id"`
	UID          uint64 `json:"uid"`
	Kind         string `json:"kind"`
}

// SessionKickPushRequest asks the Gateway owning an old connection to notify
// and close it after a newer login replaces the player's online lease.
type SessionKickPushRequest struct {
	ConnectionID uint64       `json:"connection_id"`
	UID          uint64       `json:"uid"`
	Reason       errcode.Code `json:"reason"`
}

// DeltaPublisher fans a FarmDelta out after an authoritative mutation commits.
type DeltaPublisher interface {
	Publish(ctx context.Context, delta farm.FarmDelta, originator presence.ConnRef) error
}

type queuedDelta struct {
	delta      farm.FarmDelta
	originator presence.ConnRef
}

// AsyncDeltaPublisher moves registry lookup, encoding and Gateway callbacks
// behind a bounded UID-sharded queue. One shard owns a UID, preserving FarmSeq
// order without creating one goroutine per action. A full queue drops this
// best-effort hint; SyncFarm remains the authoritative gap recovery path.
type AsyncDeltaPublisher struct {
	inner   DeltaPublisher
	mu      sync.RWMutex
	queues  []chan queuedDelta
	closed  bool
	wg      sync.WaitGroup
	dropped atomic.Uint64
}

func NewAsyncDeltaPublisher(inner DeltaPublisher, shards, perShard int) *AsyncDeltaPublisher {
	if shards <= 0 {
		shards = defaultDeltaQueueShards
	}
	if perShard <= 0 {
		perShard = defaultDeltaQueuePerShard
	}
	publisher := &AsyncDeltaPublisher{
		inner:  inner,
		queues: make([]chan queuedDelta, shards),
	}
	for index := range publisher.queues {
		queue := make(chan queuedDelta, perShard)
		publisher.queues[index] = queue
		publisher.wg.Add(1)
		go publisher.run(queue)
	}
	return publisher
}

func (*AsyncDeltaPublisher) publishesAsynchronously() {}

func (publisher *AsyncDeltaPublisher) Publish(_ context.Context, delta farm.FarmDelta, originator presence.ConnRef) error {
	if publisher == nil || publisher.inner == nil || delta.OwnerUID == 0 || len(publisher.queues) == 0 {
		return fmt.Errorf("farmrpc: async Delta publisher is not configured")
	}
	// FarmDelta values become immutable once handed to a publisher; retaining
	// the slice avoids copying the same plot projection a third time.
	if delta.GuardDog != nil {
		guardDog := *delta.GuardDog
		delta.GuardDog = &guardDog
	}
	job := queuedDelta{delta: delta, originator: originator}
	queue := publisher.queues[delta.OwnerUID%uint64(len(publisher.queues))]
	publisher.mu.RLock()
	defer publisher.mu.RUnlock()
	if publisher.closed {
		return fmt.Errorf("farmrpc: async Delta publisher stopped")
	}
	select {
	case queue <- job:
	default:
		dropped := publisher.dropped.Add(1)
		if dropped == 1 || dropped%1024 == 0 {
			telemetry.L().Warn("farmrpc Delta queue full; recovery delegated to SyncFarm",
				"component", "farmrpc", "op", "queue_delta", "dropped", dropped)
		}
	}
	return nil
}

func (publisher *AsyncDeltaPublisher) HasActiveFarm(ctx context.Context, uid uint64) (bool, error) {
	checker, ok := publisher.inner.(ActiveFarmChecker)
	if !ok {
		return false, nil
	}
	return checker.HasActiveFarm(ctx, uid)
}

func (publisher *AsyncDeltaPublisher) Shutdown(ctx context.Context) error {
	if publisher == nil {
		return nil
	}
	publisher.mu.Lock()
	if !publisher.closed {
		publisher.closed = true
		for _, queue := range publisher.queues {
			close(queue)
		}
	}
	publisher.mu.Unlock()
	done := make(chan struct{})
	go func() {
		publisher.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("farmrpc: drain Delta publisher: %w", ctx.Err())
	}
}

func (publisher *AsyncDeltaPublisher) run(queue <-chan queuedDelta) {
	defer publisher.wg.Done()
	for job := range queue {
		if err := publisher.inner.Publish(context.Background(), job.delta, job.originator); err != nil {
			telemetry.L().Error("farmrpc Delta publish failed",
				"component", "farmrpc", "op", "publish_delta", "err", err.Error())
		}
	}
}

// ActiveFarmChecker reports whether a farm still has a live room subscriber.
// The scheduler uses it after an Actor was evicted so offline farms are not
// loaded only to produce a push nobody can receive.
type ActiveFarmChecker interface {
	HasActiveFarm(ctx context.Context, uid uint64) (bool, error)
}

// DeltaBatchPusher delivers one PushBatch to a single Gateway.
type DeltaBatchPusher interface {
	PushBatch(ctx context.Context, gatewayID string, batch PushBatch) error
}

// FarmDeltaEncoder builds the typed FarmDelta payload once per publish.
type FarmDeltaEncoder interface {
	EncodeFarmDelta(delta farm.FarmDelta) ([]byte, error)
}

// PlayerDeltaPublisher fans personal-state changes to every active connection
// for one player, regardless of which farm room that player is viewing.
type PlayerDeltaPublisher interface {
	PublishPlayerDelta(ctx context.Context, uid uint64, delta farm.PlayerDelta) error
}

// TaskNotifyPublisher fans task progress updates to every active connection
// for one player, regardless of which farm room that player is viewing.
type TaskNotifyPublisher interface {
	PublishTaskNotify(ctx context.Context, uid uint64, task store.Task) error
}

// MailNotifyPublisher fans an advisory mail-state refresh signal to every
// active connection for a player.
type MailNotifyPublisher interface {
	PublishMailNotify(ctx context.Context, uid uint64, kind string) error
}

// PlayerDeltaPusher delivers one PlayerDelta to the Gateway that owns a
// connection.
type PlayerDeltaPusher interface {
	PushPlayerDelta(ctx context.Context, ref presence.ConnRef, uid uint64, delta farm.PlayerDelta) error
}

// TaskNotifyPusher delivers one TaskNotify to the Gateway that owns a connection.
type TaskNotifyPusher interface {
	PushTaskNotify(ctx context.Context, ref presence.ConnRef, uid uint64, task store.Task) error
}

// MailNotifyPusher delivers one MailNotify to the Gateway that owns a session.
type MailNotifyPusher interface {
	PushMailNotify(ctx context.Context, ref presence.ConnRef, uid uint64, kind string) error
}

// SessionKickPusher closes an evicted player connection on its owning Gateway.
type SessionKickPusher interface {
	PushSessionKick(ctx context.Context, ref presence.ConnRef, uid uint64, reason errcode.Code) error
}

// FanoutPublisher resolves room subscribers from the shared connection registry,
// encodes the Envelope once, and pushes one batch per Gateway.
type FanoutPublisher struct {
	registry *presence.Registry
	pusher   DeltaBatchPusher
	encoder  FarmDeltaEncoder
	metrics  *telemetry.Metrics
}

// NewFanoutPublisher constructs the Farm-side cross-Gateway Delta broadcaster.
func NewFanoutPublisher(registry *presence.Registry, pusher DeltaBatchPusher) *FanoutPublisher {
	return &FanoutPublisher{registry: registry, pusher: pusher}
}

// SetMetrics attaches FarmDelta broadcast collectors.
func (p *FanoutPublisher) SetMetrics(m *telemetry.Metrics) {
	if p == nil {
		return
	}
	p.metrics = m
}

// HasActiveFarm checks the same leased room index used by Publish.
func (p *FanoutPublisher) HasActiveFarm(ctx context.Context, uid uint64) (bool, error) {
	if p == nil || p.registry == nil || uid == 0 {
		return false, nil
	}
	refs, err := p.registry.LookupSubscribers(ctx, uid)
	return len(refs) > 0, err
}

// Publish attempts all current room subscribers except the connection that
// initiated the command. The originator already receives the authoritative
// response patch, so excluding it prevents duplicate FarmDelta delivery.
func (p *FanoutPublisher) Publish(ctx context.Context, delta farm.FarmDelta, originator presence.ConnRef) error {
	if p == nil || p.registry == nil || p.pusher == nil {
		return fmt.Errorf("farmrpc: Delta publisher is not configured")
	}
	refs, err := p.registry.LookupSubscribers(ctx, delta.OwnerUID)
	if err != nil {
		return err
	}

	groups := make(map[string][]uint64)
	for _, ref := range refs {
		if ref == originator {
			continue
		}
		groups[ref.GatewayID] = append(groups[ref.GatewayID], ref.ConnID)
	}
	if len(groups) == 0 {
		return nil
	}

	encodeStart := time.Now()
	typedDelta := clientwire.FarmDeltaToProto(delta)
	var envelope []byte
	if p.encoder != nil {
		envelope, err = p.encoder.EncodeFarmDelta(delta)
	}
	encodeDur := time.Since(encodeStart)
	if err != nil {
		return err
	}

	type job struct {
		gatewayID string
		connIDs   []uint64
	}
	jobs := make([]job, 0, len(groups))
	targetCount := 0
	for gatewayID, connIDs := range groups {
		jobs = append(jobs, job{gatewayID: gatewayID, connIDs: connIDs})
		targetCount += len(connIDs)
	}

	workers := len(jobs)
	if workers > maxParallelGatewayPush {
		workers = maxParallelGatewayPush
	}
	jobCh := make(chan job)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}
	pushStart := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobCh {
				if ctx.Err() != nil {
					recordErr(ctx.Err())
					continue
				}
				err := p.pusher.PushBatch(ctx, item.gatewayID, PushBatch{
					ConnIDs:  item.connIDs,
					Delta:    typedDelta,
					Envelope: envelope,
				})
				recordErr(err)
			}
		}()
	}
sendLoop:
	for _, item := range jobs {
		select {
		case <-ctx.Done():
			recordErr(ctx.Err())
			break sendLoop
		case jobCh <- item:
		}
	}
	close(jobCh)
	wg.Wait()
	if m := p.metrics; m != nil {
		// 每个 Gateway 一条 PushBatch；跨 N 个 Gateway 记 N，而不是一次 Publish 记 1。
		m.ObserveDeltaBroadcast(len(jobs), targetCount, encodeDur, time.Since(pushStart))
	}
	return firstErr
}

// PlayerFanoutPublisher resolves every active connection of a player and
// forwards personal state to its owning Gateway.
// PlayerDelta still uses per-connection pushes; batching is left for a follow-up
// because the target uid is not part of the PlayerDelta payload / PushBatch shape.
type PlayerFanoutPublisher struct {
	registry *presence.Registry
	pusher   PlayerDeltaPusher
}

// NewPlayerFanoutPublisher constructs the Farm-side PlayerDelta broadcaster.
func NewPlayerFanoutPublisher(registry *presence.Registry, pusher PlayerDeltaPusher) *PlayerFanoutPublisher {
	return &PlayerFanoutPublisher{registry: registry, pusher: pusher}
}

// PublishPlayerDelta attempts all active player connections.
func (p *PlayerFanoutPublisher) PublishPlayerDelta(ctx context.Context, uid uint64, delta farm.PlayerDelta) error {
	if p == nil || p.registry == nil || p.pusher == nil || uid == 0 {
		return fmt.Errorf("farmrpc: PlayerDelta publisher is not configured")
	}
	refs, err := p.registry.Lookup(ctx, uid)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if err := p.pusher.PushPlayerDelta(ctx, ref, uid, delta); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// TaskFanoutPublisher resolves every active connection of a player and forwards
// task progress updates to their owning Gateways.
type TaskFanoutPublisher struct {
	registry *presence.Registry
	pusher   TaskNotifyPusher

	mu      sync.Mutex
	flushMu sync.Mutex
	pending map[taskNotifyKey]pendingTaskNotify
	timer   *time.Timer
	window  time.Duration
	dropped atomic.Uint64
}

type taskNotifyKey struct {
	uid    uint64
	dayKey int64
	taskID uint32
}

type pendingTaskNotify struct {
	uid  uint64
	task store.Task
}

// MailFanoutPublisher resolves every active connection of a player and forwards
// the MailNotify refresh hint to its owning Gateways.
type MailFanoutPublisher struct {
	registry *presence.Registry
	pusher   MailNotifyPusher
}

// NewMailFanoutPublisher constructs the cross-Gateway MailNotify broadcaster.
func NewMailFanoutPublisher(registry *presence.Registry, pusher MailNotifyPusher) *MailFanoutPublisher {
	return &MailFanoutPublisher{registry: registry, pusher: pusher}
}

// PublishMailNotify attempts every active player connection.
func (p *MailFanoutPublisher) PublishMailNotify(ctx context.Context, uid uint64, kind string) error {
	if p == nil || p.registry == nil || p.pusher == nil || uid == 0 || strings.TrimSpace(kind) == "" {
		return fmt.Errorf("farmrpc: MailNotify publisher is not configured")
	}
	refs, err := p.registry.Lookup(ctx, uid)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if err := p.pusher.PushMailNotify(ctx, ref, uid, kind); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NewTaskFanoutPublisher constructs the Farm-side TaskNotify broadcaster.
func NewTaskFanoutPublisher(registry *presence.Registry, pusher TaskNotifyPusher) *TaskFanoutPublisher {
	return newTaskFanoutPublisher(registry, pusher, taskNotifyCoalesceWindow)
}

func newTaskFanoutPublisher(registry *presence.Registry, pusher TaskNotifyPusher, window time.Duration) *TaskFanoutPublisher {
	if window <= 0 {
		window = taskNotifyCoalesceWindow
	}
	return &TaskFanoutPublisher{
		registry: registry,
		pusher:   pusher,
		pending:  make(map[taskNotifyKey]pendingTaskNotify),
		window:   window,
	}
}

// PublishTaskNotify keeps only the newest state for one uid/task within a short
// window. This absorbs bursts before connection-registry lookups and internal
// HTTP callbacks. When the bounded pending map is full, the new distinct key is
// dropped as an advisory hint instead of blocking gameplay on synchronous HTTP;
// TaskList remains the authoritative recovery path.
func (p *TaskFanoutPublisher) PublishTaskNotify(ctx context.Context, uid uint64, task store.Task) error {
	if p == nil || p.registry == nil || p.pusher == nil || uid == 0 {
		return fmt.Errorf("farmrpc: TaskNotify publisher is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := taskNotifyKey{uid: uid, dayKey: task.DayKey, taskID: task.ID}
	p.mu.Lock()
	if _, exists := p.pending[key]; !exists && len(p.pending) >= maxPendingTaskNotifies {
		p.mu.Unlock()
		dropped := p.dropped.Add(1)
		if dropped == 1 || dropped%256 == 0 {
			telemetry.L().Warn("farmrpc TaskNotify queue full; advisory hint dropped",
				"component", "farmrpc",
				"op", "queue_task_notify",
				"dropped", dropped,
			)
		}
		return nil
	}
	p.pending[key] = pendingTaskNotify{uid: uid, task: task}
	if p.timer == nil {
		p.timer = time.AfterFunc(p.window, p.flushTaskNotifies)
	}
	p.mu.Unlock()
	return nil
}

func (p *TaskFanoutPublisher) takePendingTaskNotifies() []pendingTaskNotify {
	p.mu.Lock()
	defer p.mu.Unlock()
	items := make([]pendingTaskNotify, 0, len(p.pending))
	for _, item := range p.pending {
		items = append(items, item)
	}
	p.pending = make(map[taskNotifyKey]pendingTaskNotify)
	p.timer = nil
	return items
}

func (p *TaskFanoutPublisher) flushTaskNotifies() {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	items := p.takePendingTaskNotifies()
	if len(items) == 0 {
		return
	}
	workers := len(items)
	if workers > maxParallelGatewayPush {
		workers = maxParallelGatewayPush
	}
	jobs := make(chan pendingTaskNotify)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := p.deliverTaskNotify(ctx, item.uid, item.task)
				cancel()
				if err != nil {
					telemetry.L().Error("farmrpc coalesced TaskNotify delivery failed",
						"component", "farmrpc",
						"op", "flush_task_notify",
						"uid", item.uid,
						"task_id", item.task.ID,
						"err", err.Error(),
					)
				}
			}
		}()
	}
	for _, item := range items {
		jobs <- item
	}
	close(jobs)
	wg.Wait()
}

func (p *TaskFanoutPublisher) deliverTaskNotify(ctx context.Context, uid uint64, task store.Task) error {
	refs, err := p.registry.Lookup(ctx, uid)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if err := p.pusher.PushTaskNotify(ctx, ref, uid, task); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
