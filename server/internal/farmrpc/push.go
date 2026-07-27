package farmrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"farm/server/internal/connreg"
	"farm/server/internal/farm"
)

const (
	deltaPushPath       = "/internal/v1/push/farm-delta"
	playerDeltaPushPath = "/internal/v1/push/player-delta"
)

// DeltaPushRequest is sent from a Farm to the Gateway that owns one WebSocket.
// The Gateway verifies the service token and that the connection still views
// Delta.OwnerUID before writing to the client.
type DeltaPushRequest struct {
	ConnectionID uint64         `json:"connection_id"`
	Delta        farm.FarmDelta `json:"delta"`
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
	Publish(ctx context.Context, delta farm.FarmDelta, originatorConnID uint64) error
}

// DeltaPusher delivers one Delta to the Gateway that owns a connection.
type DeltaPusher interface {
	Push(ctx context.Context, ref connreg.ConnRef, delta farm.FarmDelta) error
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

// FanoutPublisher resolves room subscribers from the shared connection registry
// and forwards each Delta to the Gateway that owns its WebSocket.
type FanoutPublisher struct {
	registry *connreg.Registry
	pusher   DeltaPusher
}

// NewFanoutPublisher constructs the Farm-side cross-Gateway Delta broadcaster.
func NewFanoutPublisher(registry *connreg.Registry, pusher DeltaPusher) *FanoutPublisher {
	return &FanoutPublisher{registry: registry, pusher: pusher}
}

// Publish attempts all current room subscribers except the connection that
// initiated the command. The originator already receives the authoritative
// response patch, so excluding it prevents duplicate FarmDelta delivery.
func (p *FanoutPublisher) Publish(ctx context.Context, delta farm.FarmDelta, originatorConnID uint64) error {
	if p == nil || p.registry == nil || p.pusher == nil {
		return fmt.Errorf("farmrpc: Delta publisher is not configured")
	}
	refs, err := p.registry.LookupSubscribers(ctx, delta.OwnerUID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ref := range refs {
		if ref.ConnID == originatorConnID {
			continue
		}
		if err := p.pusher.Push(ctx, ref, delta); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// PlayerFanoutPublisher resolves every active connection of a player and
// forwards personal state to its owning Gateway.
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

// Push sends a Delta to one Gateway-owned connection.
func (p *HTTPDeltaPusher) Push(ctx context.Context, ref connreg.ConnRef, delta farm.FarmDelta) error {
	if p == nil {
		return fmt.Errorf("farmrpc: HTTP Delta pusher is nil")
	}
	endpoint := p.endpoints[ref.GatewayID]
	if endpoint == "" {
		return fmt.Errorf("farmrpc: no Gateway endpoint configured for %q", ref.GatewayID)
	}
	body, err := json.Marshal(DeltaPushRequest{ConnectionID: ref.ConnID, Delta: delta})
	if err != nil {
		return fmt.Errorf("farmrpc: encode Delta push: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+deltaPushPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("farmrpc: build Delta push: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return fmt.Errorf("farmrpc: push Delta: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("farmrpc: Delta push returned HTTP %d", response.StatusCode)
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
