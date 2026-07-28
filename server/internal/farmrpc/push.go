package farmrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
	"farm/server/internal/obs"
	"farm/server/internal/wireenv"
)

const (
	deltaPushBatchPath  = "/internal/v1/push/farm-delta-batch"
	playerDeltaPushPath = "/internal/v1/push/player-delta"

	// Bound concurrent Gateway callbacks so one Publish cannot spawn unbounded
	// goroutines when a room spans many Gateways.
	maxParallelGatewayPush = 16
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

// PlayerDeltaPusher delivers one PlayerDelta to the Gateway that owns a
// connection.
type PlayerDeltaPusher interface {
	PushPlayerDelta(ctx context.Context, ref connreg.ConnRef, uid uint64, delta farm.PlayerDelta) error
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
