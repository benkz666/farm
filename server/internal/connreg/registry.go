// Package connreg stores the Gateway currently owning each WebSocket connection.
package connreg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "farm:connreg:"

// ConnRef identifies a connection that must receive a server-side push.
type ConnRef struct {
	ConnID    uint64 `json:"conn_id"`
	GatewayID string `json:"gateway_id"`
}

// Backend is the narrow Redis hash surface used by Registry.
type Backend interface {
	Set(ctx context.Context, key, field, value string) error
	Delete(ctx context.Context, key, field string) error
	Values(ctx context.Context, key string) (map[string]string, error)
}

// Registry maps player connections and active farm-room subscriptions to the
// Gateway that owns the WebSocket. The same connection may appear in both maps.
type Registry struct {
	backend Backend
}

// New constructs a Redis-backed connection registry.
func New(client redis.UniversalClient) *Registry {
	return NewWithBackend(redisBackend{client: client})
}

// NewWithBackend constructs a registry from a narrow storage boundary. It is
// exported so transport tests can exercise registry behavior without Redis.
func NewWithBackend(backend Backend) *Registry {
	return &Registry{backend: backend}
}

// Register records a connected player's WebSocket lifecycle.
func (r *Registry) Register(ctx context.Context, uid, connID uint64, gatewayID string) error {
	return r.set(ctx, connectionKey(uid), connID, gatewayID)
}

// Unregister removes a disconnected player's WebSocket lifecycle record.
func (r *Registry) Unregister(ctx context.Context, uid, connID uint64, gatewayID string) error {
	return r.delete(ctx, connectionKey(uid), connID, gatewayID)
}

// Lookup returns every currently registered connection for uid.
func (r *Registry) Lookup(ctx context.Context, uid uint64) ([]ConnRef, error) {
	return r.lookup(ctx, connectionKey(uid))
}

// Subscribe registers a connection as viewing ownerUID's farm. Farm Delta
// fan-out uses this index rather than the player's lifecycle index.
func (r *Registry) Subscribe(ctx context.Context, ownerUID, connID uint64, gatewayID string) error {
	return r.set(ctx, roomKey(ownerUID), connID, gatewayID)
}

// Unsubscribe removes a connection from ownerUID's farm-room fan-out index.
func (r *Registry) Unsubscribe(ctx context.Context, ownerUID, connID uint64, gatewayID string) error {
	return r.delete(ctx, roomKey(ownerUID), connID, gatewayID)
}

// LookupSubscribers returns the WebSockets currently viewing ownerUID's farm.
func (r *Registry) LookupSubscribers(ctx context.Context, ownerUID uint64) ([]ConnRef, error) {
	return r.lookup(ctx, roomKey(ownerUID))
}

func (r *Registry) set(ctx context.Context, key string, connID uint64, gatewayID string) error {
	if r == nil || r.backend == nil {
		return errors.New("connreg: registry backend is nil")
	}
	if connID == 0 || strings.TrimSpace(gatewayID) == "" {
		return errors.New("connreg: connection ID and gateway ID are required")
	}
	if err := r.backend.Set(ctx, key, encodeRefField(gatewayID, connID), gatewayID); err != nil {
		return fmt.Errorf("connreg: register connection: %w", err)
	}
	return nil
}

func (r *Registry) delete(ctx context.Context, key string, connID uint64, gatewayID string) error {
	if r == nil || r.backend == nil {
		return errors.New("connreg: registry backend is nil")
	}
	if connID == 0 || strings.TrimSpace(gatewayID) == "" {
		return errors.New("connreg: connection ID and gateway ID are required")
	}
	if err := r.backend.Delete(ctx, key, encodeRefField(gatewayID, connID)); err != nil {
		return fmt.Errorf("connreg: unregister connection: %w", err)
	}
	return nil
}

func (r *Registry) lookup(ctx context.Context, key string) ([]ConnRef, error) {
	if r == nil || r.backend == nil {
		return nil, errors.New("connreg: registry backend is nil")
	}
	values, err := r.backend.Values(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("connreg: lookup connections: %w", err)
	}
	refs := make([]ConnRef, 0, len(values))
	for encodedRef, storedGatewayID := range values {
		ref, ok := decodeRefField(encodedRef)
		if !ok || ref.GatewayID != strings.TrimSpace(storedGatewayID) {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ConnID == refs[j].ConnID {
			return refs[i].GatewayID < refs[j].GatewayID
		}
		return refs[i].ConnID < refs[j].ConnID
	})
	return refs, nil
}

func encodeRefField(gatewayID string, connID uint64) string {
	return strings.TrimSpace(gatewayID) + ":" + strconv.FormatUint(connID, 10)
}

func decodeRefField(field string) (ConnRef, bool) {
	separator := strings.LastIndexByte(field, ':')
	if separator <= 0 || separator == len(field)-1 {
		return ConnRef{}, false
	}
	gatewayID := strings.TrimSpace(field[:separator])
	connID, err := strconv.ParseUint(field[separator+1:], 10, 64)
	if err != nil || connID == 0 || gatewayID == "" {
		return ConnRef{}, false
	}
	return ConnRef{ConnID: connID, GatewayID: gatewayID}, true
}

func connectionKey(uid uint64) string {
	return keyPrefix + "connection:" + strconv.FormatUint(uid, 10)
}

func roomKey(ownerUID uint64) string {
	return keyPrefix + "room:" + strconv.FormatUint(ownerUID, 10)
}

type redisBackend struct {
	client redis.UniversalClient
}

func (b redisBackend) Set(ctx context.Context, key, field, value string) error {
	if b.client == nil {
		return errors.New("redis client is nil")
	}
	return b.client.HSet(ctx, key, field, value).Err()
}

func (b redisBackend) Delete(ctx context.Context, key, field string) error {
	if b.client == nil {
		return errors.New("redis client is nil")
	}
	return b.client.HDel(ctx, key, field).Err()
}

func (b redisBackend) Values(ctx context.Context, key string) (map[string]string, error) {
	if b.client == nil {
		return nil, errors.New("redis client is nil")
	}
	return b.client.HGetAll(ctx, key).Result()
}
