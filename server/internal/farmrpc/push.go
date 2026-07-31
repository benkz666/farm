package farmrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
	"farm/server/internal/wireenv"
)

const (
	deltaPushBatchPath  = "/internal/v1/push/farm-delta-batch"
	playerDeltaPushPath = "/internal/v1/push/player-delta"
	taskNotifyPushPath  = "/internal/v1/push/task-notify"
	mailNotifyPushPath  = "/internal/v1/push/mail-notify"
	sessionKickPushPath = "/internal/v1/push/session-kick"

	// Bound concurrent Gateway callbacks so one Publish cannot spawn unbounded
	// goroutines when a room spans many Gateways.
	maxParallelGatewayPush = 16

	taskNotifyCoalesceWindow = 75 * time.Millisecond
	maxPendingTaskNotifies   = 4096
)

// DeltaPushRequest is the legacy single-connection Farm→Gateway callback.
// Prefer PushBatch for new fan-out paths.
type DeltaPushRequest struct {
	ConnectionID uint64         `json:"connection_id"`
	Delta        farm.FarmDelta `json:"delta"`
}

// PushBatch is the internal Gateway push unit from protocol.md §2.5:
// pre-encoded Envelope bytes delivered to many local connections at once.
type PushBatch struct {
	ConnIDs  []uint64 `json:"conn_ids"`
	Envelope []byte   `json:"envelope"`
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
	ConnectionID uint64      `json:"connection_id"`
	UID          uint64      `json:"uid"`
	Reason       pkgerr.Code `json:"reason"`
}

// DeltaPublisher fans a FarmDelta out after an authoritative mutation commits.
type DeltaPublisher interface {
	Publish(ctx context.Context, delta farm.FarmDelta, originator connreg.ConnRef) error
}

// DeltaBatchPusher delivers one PushBatch to a single Gateway.
type DeltaBatchPusher interface {
	PushBatch(ctx context.Context, gatewayID string, batch PushBatch) error
}

// FarmDeltaEncoder builds the client-visible FarmDelta Envelope once per publish.
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
	PushPlayerDelta(ctx context.Context, ref connreg.ConnRef, uid uint64, delta farm.PlayerDelta) error
}

// TaskNotifyPusher delivers one TaskNotify to the Gateway that owns a connection.
type TaskNotifyPusher interface {
	PushTaskNotify(ctx context.Context, ref connreg.ConnRef, uid uint64, task store.Task) error
}

// MailNotifyPusher delivers one MailNotify to the Gateway that owns a session.
type MailNotifyPusher interface {
	PushMailNotify(ctx context.Context, ref connreg.ConnRef, uid uint64, kind string) error
}

// SessionKickPusher closes an evicted player connection on its owning Gateway.
type SessionKickPusher interface {
	PushSessionKick(ctx context.Context, ref connreg.ConnRef, uid uint64, reason pkgerr.Code) error
}

// FanoutPublisher resolves room subscribers from the shared connection registry,
// encodes the Envelope once, and pushes one batch per Gateway.
type FanoutPublisher struct {
	registry *connreg.Registry
	pusher   DeltaBatchPusher
	encoder  FarmDeltaEncoder
	metrics  *obs.Metrics
}

// NewFanoutPublisher constructs the Farm-side cross-Gateway Delta broadcaster.
func NewFanoutPublisher(registry *connreg.Registry, pusher DeltaBatchPusher) *FanoutPublisher {
	return &FanoutPublisher{registry: registry, pusher: pusher}
}

// SetMetrics attaches FarmDelta broadcast collectors.
func (p *FanoutPublisher) SetMetrics(m *obs.Metrics) {
	if p == nil {
		return
	}
	p.metrics = m
}

// Publish attempts all current room subscribers except the connection that
// initiated the command. The originator already receives the authoritative
// response patch, so excluding it prevents duplicate FarmDelta delivery.
func (p *FanoutPublisher) Publish(ctx context.Context, delta farm.FarmDelta, originator connreg.ConnRef) error {
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

	encoder := p.encoder
	if encoder == nil {
		encoder = defaultFarmDeltaEncoder{}
	}
	encodeStart := time.Now()
	envelope, err := encoder.EncodeFarmDelta(delta)
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

type defaultFarmDeltaEncoder struct{}

func (defaultFarmDeltaEncoder) EncodeFarmDelta(delta farm.FarmDelta) ([]byte, error) {
	return wireenv.EncodeFarmDelta(delta)
}

// PlayerFanoutPublisher resolves every active connection of a player and
// forwards personal state to its owning Gateway.
// PlayerDelta still uses per-connection pushes; batching is left for a follow-up
// because the target uid is not part of the PlayerDelta payload / PushBatch shape.
type PlayerFanoutPublisher struct {
	registry *connreg.Registry
	pusher   PlayerDeltaPusher
}

// NewPlayerFanoutPublisher constructs the Farm-side PlayerDelta broadcaster.
func NewPlayerFanoutPublisher(registry *connreg.Registry, pusher PlayerDeltaPusher) *PlayerFanoutPublisher {
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
	registry *connreg.Registry
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
	registry *connreg.Registry
	pusher   MailNotifyPusher
}

// NewMailFanoutPublisher constructs the cross-Gateway MailNotify broadcaster.
func NewMailFanoutPublisher(registry *connreg.Registry, pusher MailNotifyPusher) *MailFanoutPublisher {
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
func NewTaskFanoutPublisher(registry *connreg.Registry, pusher TaskNotifyPusher) *TaskFanoutPublisher {
	return newTaskFanoutPublisher(registry, pusher, taskNotifyCoalesceWindow)
}

func newTaskFanoutPublisher(registry *connreg.Registry, pusher TaskNotifyPusher, window time.Duration) *TaskFanoutPublisher {
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
			obs.L().Warn("farmrpc TaskNotify queue full; advisory hint dropped",
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
					obs.L().Error("farmrpc coalesced TaskNotify delivery failed",
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

// HTTPDeltaPusher implements Farm-to-Gateway callbacks over authenticated HTTP.
type HTTPDeltaPusher struct {
	endpoints map[string]string
	token     string
	client    *http.Client
}

// NewHTTPDeltaPusher constructs a callback client from Gateway ID to URL.
func NewHTTPDeltaPusher(endpoints map[string]string, token string) *HTTPDeltaPusher {
	copied := make(map[string]string, len(endpoints))
	for gatewayID, endpoint := range endpoints {
		copied[gatewayID] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return &HTTPDeltaPusher{
		endpoints: copied,
		token:     token,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

// PushBatch sends one pre-encoded Envelope to many connections on one Gateway.
func (p *HTTPDeltaPusher) PushBatch(ctx context.Context, gatewayID string, batch PushBatch) error {
	if p == nil {
		return fmt.Errorf("farmrpc: HTTP Delta pusher is nil")
	}
	endpoint := p.endpoints[gatewayID]
	if endpoint == "" {
		return fmt.Errorf("farmrpc: no Gateway endpoint configured for %q", gatewayID)
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("farmrpc: encode Delta push batch: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+deltaPushBatchPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("farmrpc: build Delta push batch: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("farmrpc: push Delta batch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("farmrpc: Delta push batch returned HTTP %d", response.StatusCode)
	}
	return nil
}

// HTTPPlayerDeltaPusher implements Farm-to-Gateway PlayerDelta callbacks over
// the same authenticated internal HTTP boundary as FarmDelta.
type HTTPPlayerDeltaPusher struct {
	endpoints map[string]string
	token     string
	client    *http.Client
}

// NewHTTPPlayerDeltaPusher constructs a callback client from Gateway ID to URL.
func NewHTTPPlayerDeltaPusher(endpoints map[string]string, token string) *HTTPPlayerDeltaPusher {
	copied := make(map[string]string, len(endpoints))
	for gatewayID, endpoint := range endpoints {
		copied[gatewayID] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return &HTTPPlayerDeltaPusher{
		endpoints: copied,
		token:     token,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

// PushPlayerDelta sends a personal-state update to one Gateway-owned connection.
func (p *HTTPPlayerDeltaPusher) PushPlayerDelta(ctx context.Context, ref connreg.ConnRef, uid uint64, delta farm.PlayerDelta) error {
	if p == nil {
		return fmt.Errorf("farmrpc: HTTP PlayerDelta pusher is nil")
	}
	endpoint := p.endpoints[ref.GatewayID]
	if endpoint == "" {
		return fmt.Errorf("farmrpc: no Gateway endpoint configured for %q", ref.GatewayID)
	}
	body, err := json.Marshal(PlayerDeltaPushRequest{ConnectionID: ref.ConnID, UID: uid, Delta: delta})
	if err != nil {
		return fmt.Errorf("farmrpc: encode PlayerDelta push: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+playerDeltaPushPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("farmrpc: build PlayerDelta push: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("farmrpc: push PlayerDelta: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("farmrpc: PlayerDelta push returned HTTP %d", response.StatusCode)
	}
	return nil
}

// HTTPTaskNotifyPusher implements Farm-to-Gateway TaskNotify callbacks over
// the same authenticated internal HTTP boundary as PlayerDelta.
type HTTPTaskNotifyPusher struct {
	endpoints map[string]string
	token     string
	client    *http.Client
}

// NewHTTPTaskNotifyPusher constructs a callback client from Gateway ID to URL.
func NewHTTPTaskNotifyPusher(endpoints map[string]string, token string) *HTTPTaskNotifyPusher {
	copied := make(map[string]string, len(endpoints))
	for gatewayID, endpoint := range endpoints {
		copied[gatewayID] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return &HTTPTaskNotifyPusher{
		endpoints: copied,
		token:     token,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

// PushTaskNotify sends a task progress update to one Gateway-owned connection.
func (p *HTTPTaskNotifyPusher) PushTaskNotify(ctx context.Context, ref connreg.ConnRef, uid uint64, task store.Task) error {
	if p == nil {
		return fmt.Errorf("farmrpc: HTTP TaskNotify pusher is nil")
	}
	endpoint := p.endpoints[ref.GatewayID]
	if endpoint == "" {
		return fmt.Errorf("farmrpc: no Gateway endpoint configured for %q", ref.GatewayID)
	}
	body, err := json.Marshal(TaskNotifyPushRequest{ConnectionID: ref.ConnID, UID: uid, Task: task})
	if err != nil {
		return fmt.Errorf("farmrpc: encode TaskNotify push: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+taskNotifyPushPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("farmrpc: build TaskNotify push: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("farmrpc: push TaskNotify: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("farmrpc: TaskNotify push returned HTTP %d", response.StatusCode)
	}
	return nil
}

// HTTPMailNotifyPusher implements MailNotify callbacks over the authenticated
// Gateway internal HTTP boundary.
type HTTPMailNotifyPusher struct {
	endpoints map[string]string
	token     string
	client    *http.Client
}

// NewHTTPMailNotifyPusher constructs a callback client from Gateway ID to URL.
func NewHTTPMailNotifyPusher(endpoints map[string]string, token string) *HTTPMailNotifyPusher {
	copied := make(map[string]string, len(endpoints))
	for gatewayID, endpoint := range endpoints {
		copied[gatewayID] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return &HTTPMailNotifyPusher{
		endpoints: copied,
		token:     token,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

// PushMailNotify sends an advisory MailNotify hint to one Gateway-owned session.
func (p *HTTPMailNotifyPusher) PushMailNotify(ctx context.Context, ref connreg.ConnRef, uid uint64, kind string) error {
	if p == nil {
		return fmt.Errorf("farmrpc: HTTP MailNotify pusher is nil")
	}
	endpoint := p.endpoints[ref.GatewayID]
	if endpoint == "" {
		return fmt.Errorf("farmrpc: no Gateway endpoint configured for %q", ref.GatewayID)
	}
	body, err := json.Marshal(MailNotifyPushRequest{ConnectionID: ref.ConnID, UID: uid, Kind: kind})
	if err != nil {
		return fmt.Errorf("farmrpc: encode MailNotify push: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+mailNotifyPushPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("farmrpc: build MailNotify push: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("farmrpc: push MailNotify: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("farmrpc: MailNotify push returned HTTP %d", response.StatusCode)
	}
	return nil
}

// HTTPSessionKickPusher implements cross-Gateway session replacement callbacks.
type HTTPSessionKickPusher struct {
	endpoints map[string]string
	token     string
	client    *http.Client
}

func NewHTTPSessionKickPusher(endpoints map[string]string, token string) *HTTPSessionKickPusher {
	copied := make(map[string]string, len(endpoints))
	for gatewayID, endpoint := range endpoints {
		copied[gatewayID] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return &HTTPSessionKickPusher{
		endpoints: copied,
		token:     token,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (p *HTTPSessionKickPusher) PushSessionKick(ctx context.Context, ref connreg.ConnRef, uid uint64, reason pkgerr.Code) error {
	if p == nil {
		return fmt.Errorf("farmrpc: HTTP SessionKick pusher is nil")
	}
	endpoint := p.endpoints[ref.GatewayID]
	if endpoint == "" {
		return fmt.Errorf("farmrpc: no Gateway endpoint configured for %q", ref.GatewayID)
	}
	body, err := json.Marshal(SessionKickPushRequest{
		ConnectionID: ref.ConnID,
		UID:          uid,
		Reason:       reason,
	})
	if err != nil {
		return fmt.Errorf("farmrpc: encode SessionKick push: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+sessionKickPushPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("farmrpc: build SessionKick push: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("farmrpc: push SessionKick: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("farmrpc: SessionKick push returned HTTP %d", response.StatusCode)
	}
	return nil
}
