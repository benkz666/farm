// Package presence stores the Gateway currently owning each WebSocket connection.
//
// Entries are per-member leases (Redis sorted set: member=gatewayID:connID,
// score=expiresAtUnixMilli). DefaultLeaseTTL is 2m so a quiet-but-alive socket
// can survive wsReadTimeout (90s). Gateway renews on Register/Subscribe and
// periodically from authenticated traffic or WebSocket Pong handling.
//
// Every Upsert also drops members with expiresAt <= now and refreshes a
// whole-key fallback TTL of 2*leaseTTL (+ granularity margin). That fallback
// is not the member validity window — members still expire strictly by their
// own score at 1*leaseTTL — it only bounds Redis key lifetime when nobody
// renews/Lookup, leaving headroom for peer members with slightly later scores
// or small cross-Gateway clock skew. Lookup still filters expired members.
package presence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// keyPrefix uses "lease:" so leftover Hash keys from the pre-TTL registry
	// (`farm:connreg:connection|room:*`) cannot collide with ZSET ops (WRONGTYPE).
	keyPrefix = "farm:connreg:lease:"

	// DefaultLeaseTTL is longer than Gateway wsReadTimeout (90s) so an idle but
	// still-connected client is not dropped from the registry before the read
	// deadline closes the socket. Active requests renew the same member.
	DefaultLeaseTTL = 2 * time.Minute

	// keyTTLSafetyMargin covers Redis EXPIRE second granularity so the key is
	// not deleted while a member score is still considered alive.
	keyTTLSafetyMargin = time.Second
)

// ErrAlreadyConnected means another live WebSocket already owns the player's
// lifecycle lease. Room subscriptions remain multi-member.
var ErrAlreadyConnected = errors.New("connreg: player already connected")

// keyFallbackTTL returns the whole-key EXPIRE duration: 2*leaseTTL plus the
// granularity margin. It never returns a non-positive duration and clamps on
// int64 overflow instead of wrapping to a negative TTL.
func keyFallbackTTL(leaseTTL time.Duration) time.Duration {
	if leaseTTL <= 0 {
		return keyTTLSafetyMargin
	}
	max := time.Duration(math.MaxInt64)
	if leaseTTL > max/2 {
		return max
	}
	doubled := leaseTTL * 2
	if doubled > max-keyTTLSafetyMargin {
		return max
	}
	return doubled + keyTTLSafetyMargin
}

// ConnRef identifies a connection that must receive a server-side push.
type ConnRef struct {
	ConnID    uint64 `json:"conn_id"`
	GatewayID string `json:"gateway_id"`
}

// Backend is the narrow Redis sorted-set surface used by Registry.
// Each member expires independently via its score (expiresAtUnixMilli).
type Backend interface {
	// Upsert writes/renews member at expiresAtUnixMilli, removes every member
	// whose score is <= nowUnixMilli (without touching still-valid peers), and
	// sets the key's fallback TTL to keyTTL. nowUnixMilli and keyTTL are
	// supplied by Registry from its injectable clock — backends must not read
	// a separate wall clock.
	Upsert(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) error
	// Claim removes expired members and writes member only when no other live
	// member owns the key. The check-and-write must be atomic.
	Claim(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) (bool, error)
	// Replace atomically transfers ownership to member and returns the live
	// members that were evicted. Renewing the same sole member evicts nobody.
	Replace(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) ([]string, error)
	Delete(ctx context.Context, key, member string) error
	// AliveMembers removes members with expiresAt <= nowUnixMilli and returns
	// the remaining members. Removal may be best-effort as long as expired
	// members are never returned.
	AliveMembers(ctx context.Context, key string, nowUnixMilli int64) ([]string, error)
}

// Option configures Registry clock and lease duration (tests inject short TTL).
type Option func(*Registry)

// WithClock injects a deterministic clock. now must be non-nil.
func WithClock(now func() time.Time) Option {
	return func(r *Registry) {
		if now != nil {
			r.now = now
		}
	}
}

// WithLeaseTTL overrides DefaultLeaseTTL. Non-positive values are ignored.
func WithLeaseTTL(ttl time.Duration) Option {
	return func(r *Registry) {
		if ttl > 0 {
			r.leaseTTL = ttl
		}
	}
}

// Registry maps player connections and active farm-room subscriptions to the
// Gateway that owns the WebSocket. The same connection may appear in both maps.
type Registry struct {
	backend  Backend
	now      func() time.Time
	leaseTTL time.Duration
}

// New constructs a Redis-backed connection registry.
func New(client redis.UniversalClient, opts ...Option) *Registry {
	return NewWithBackend(redisBackend{client: client}, opts...)
}

// NewWithBackend constructs a registry from a narrow storage boundary. It is
// exported so transport tests can exercise registry behavior without Redis.
func NewWithBackend(backend Backend, opts ...Option) *Registry {
	registry := &Registry{
		backend:  backend,
		now:      time.Now,
		leaseTTL: DefaultLeaseTTL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(registry)
		}
	}
	return registry
}

// Register records or renews a connected player's WebSocket lifecycle lease.
func (r *Registry) Register(ctx context.Context, uid, connID uint64, gatewayID string) error {
	if r == nil || r.backend == nil {
		return errors.New("connreg: registry backend is nil")
	}
	if connID == 0 || strings.TrimSpace(gatewayID) == "" {
		return errors.New("connreg: connection ID and gateway ID are required")
	}
	now := r.now()
	claimed, err := r.backend.Claim(
		ctx,
		connectionKey(uid),
		encodeRefField(gatewayID, connID),
		now.Add(r.leaseTTL).UnixMilli(),
		now.UnixMilli(),
		keyFallbackTTL(r.leaseTTL),
	)
	if err != nil {
		return fmt.Errorf("connreg: claim connection: %w", err)
	}
	if !claimed {
		return ErrAlreadyConnected
	}
	return nil
}

// ReplaceConnection atomically makes the new WebSocket the sole lifecycle
// owner and returns the previous owners that must receive Kick.
func (r *Registry) ReplaceConnection(ctx context.Context, uid, connID uint64, gatewayID string) ([]ConnRef, error) {
	if r == nil || r.backend == nil {
		return nil, errors.New("connreg: registry backend is nil")
	}
	if connID == 0 || strings.TrimSpace(gatewayID) == "" {
		return nil, errors.New("connreg: connection ID and gateway ID are required")
	}
	now := r.now()
	members, err := r.backend.Replace(
		ctx,
		connectionKey(uid),
		encodeRefField(gatewayID, connID),
		now.Add(r.leaseTTL).UnixMilli(),
		now.UnixMilli(),
		keyFallbackTTL(r.leaseTTL),
	)
	if err != nil {
		return nil, fmt.Errorf("connreg: replace connection: %w", err)
	}
	refs := make([]ConnRef, 0, len(members))
	for _, member := range members {
		if ref, ok := decodeRefField(member); ok {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].GatewayID == refs[j].GatewayID {
			return refs[i].ConnID < refs[j].ConnID
		}
		return refs[i].GatewayID < refs[j].GatewayID
	})
	return refs, nil
}

// Unregister removes a disconnected player's WebSocket lifecycle record.
func (r *Registry) Unregister(ctx context.Context, uid, connID uint64, gatewayID string) error {
	return r.delete(ctx, connectionKey(uid), connID, gatewayID)
}

// Lookup returns the sole currently leased connection for uid, if any.
func (r *Registry) Lookup(ctx context.Context, uid uint64) ([]ConnRef, error) {
	return r.lookup(ctx, connectionKey(uid))
}

// Subscribe registers or renews a connection as viewing ownerUID's farm.
// Farm Delta fan-out uses this index rather than the player's lifecycle index.
func (r *Registry) Subscribe(ctx context.Context, ownerUID, connID uint64, gatewayID string) error {
	return r.upsert(ctx, roomKey(ownerUID), connID, gatewayID)
}

// Unsubscribe removes a connection from ownerUID's farm-room fan-out index.
func (r *Registry) Unsubscribe(ctx context.Context, ownerUID, connID uint64, gatewayID string) error {
	return r.delete(ctx, roomKey(ownerUID), connID, gatewayID)
}

// LookupSubscribers returns the WebSockets currently leased for ownerUID's farm.
func (r *Registry) LookupSubscribers(ctx context.Context, ownerUID uint64) ([]ConnRef, error) {
	return r.lookup(ctx, roomKey(ownerUID))
}

func (r *Registry) upsert(ctx context.Context, key string, connID uint64, gatewayID string) error {
	if r == nil || r.backend == nil {
		return errors.New("connreg: registry backend is nil")
	}
	if connID == 0 || strings.TrimSpace(gatewayID) == "" {
		return errors.New("connreg: connection ID and gateway ID are required")
	}
	now := r.now()
	nowMs := now.UnixMilli()
	expiresAt := now.Add(r.leaseTTL).UnixMilli()
	// Member score = 1*leaseTTL; key fallback = 2*leaseTTL (+ margin) so peers
	// with a slightly later score / clock skew are not cut off by EXPIRE.
	keyTTL := keyFallbackTTL(r.leaseTTL)
	if err := r.backend.Upsert(ctx, key, encodeRefField(gatewayID, connID), expiresAt, nowMs, keyTTL); err != nil {
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
	members, err := r.backend.AliveMembers(ctx, key, r.now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("connreg: lookup connections: %w", err)
	}
	refs := make([]ConnRef, 0, len(members))
	for _, member := range members {
		ref, ok := decodeRefField(member)
		if !ok {
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

var claimConnectionScript = redis.NewScript(`
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[2])
local members = redis.call("ZRANGE", KEYS[1], 0, -1)
if #members > 0 and not (#members == 1 and members[1] == ARGV[1]) then
	return 0
end
redis.call("ZADD", KEYS[1], ARGV[3], ARGV[1])
redis.call("PEXPIRE", KEYS[1], ARGV[4])
return 1
`)

var replaceConnectionScript = redis.NewScript(`
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[2])
local members = redis.call("ZRANGE", KEYS[1], 0, -1)
if #members == 1 and members[1] == ARGV[1] then
	redis.call("ZADD", KEYS[1], ARGV[3], ARGV[1])
	redis.call("PEXPIRE", KEYS[1], ARGV[4])
	return {}
end
redis.call("DEL", KEYS[1])
redis.call("ZADD", KEYS[1], ARGV[3], ARGV[1])
redis.call("PEXPIRE", KEYS[1], ARGV[4])
return members
`)

func (b redisBackend) Claim(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) (bool, error) {
	if b.client == nil {
		return false, errors.New("redis client is nil")
	}
	if keyTTL <= 0 {
		return false, errors.New("key TTL must be positive")
	}
	result, err := claimConnectionScript.Run(
		ctx,
		b.client,
		[]string{key},
		member,
		nowUnixMilli,
		expiresAtUnixMilli,
		keyTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

func (b redisBackend) Replace(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) ([]string, error) {
	if b.client == nil {
		return nil, errors.New("redis client is nil")
	}
	if keyTTL <= 0 {
		return nil, errors.New("key TTL must be positive")
	}
	members, err := replaceConnectionScript.Run(
		ctx,
		b.client,
		[]string{key},
		member,
		nowUnixMilli,
		expiresAtUnixMilli,
		keyTTL.Milliseconds(),
	).StringSlice()
	if err != nil {
		return nil, err
	}
	return members, nil
}

func (b redisBackend) Upsert(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) error {
	if b.client == nil {
		return errors.New("redis client is nil")
	}
	if keyTTL <= 0 {
		return errors.New("key TTL must be positive")
	}
	// Single-key TxPipeline: drop expired members, write/renew, refresh key TTL.
	pipe := b.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(nowUnixMilli, 10))
	pipe.ZAdd(ctx, key, redis.Z{
		Score:  float64(expiresAtUnixMilli),
		Member: member,
	})
	pipe.Expire(ctx, key, keyTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (b redisBackend) Delete(ctx context.Context, key, member string) error {
	if b.client == nil {
		return errors.New("redis client is nil")
	}
	return b.client.ZRem(ctx, key, member).Err()
}

func (b redisBackend) AliveMembers(ctx context.Context, key string, nowUnixMilli int64) ([]string, error) {
	if b.client == nil {
		return nil, errors.New("redis client is nil")
	}
	// Drop leases whose expiresAt <= now, then return the remainder.
	pipe := b.client.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(nowUnixMilli, 10))
	rangeCmd := pipe.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: "(" + strconv.FormatInt(nowUnixMilli, 10),
		Max: "+inf",
	})
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return rangeCmd.Result()
}
