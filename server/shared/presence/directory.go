package presence

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	gatewayInstanceKeyPrefix = "farm:gateway:instance:"

	// DefaultGatewayInstanceTTL bounds how long a crashed Gateway remains
	// discoverable. Gateways renew the lease every third of this interval.
	DefaultGatewayInstanceTTL = 30 * time.Second
)

// ErrGatewayInstanceNotFound means no live Gateway currently owns the ID.
var ErrGatewayInstanceNotFound = errors.New("gateway directory: instance not found")

// GatewayDirectory stores ephemeral Gateway ID -> gRPC endpoint leases.
// Durable session and business state remain outside Gateway processes; the
// directory only lets Farm route a push to the Pod owning a live WebSocket.
type GatewayDirectory struct {
	backend gatewayDirectoryBackend
}

type gatewayDirectoryBackend interface {
	Put(ctx context.Context, key, value string, ttl time.Duration) error
	Get(ctx context.Context, key string) (string, error)
	DeleteIfValue(ctx context.Context, key, value string) error
	List(ctx context.Context, prefix string) (map[string]string, error)
}

// NewGatewayDirectory constructs a Redis-backed Gateway directory.
func NewGatewayDirectory(client redis.UniversalClient) *GatewayDirectory {
	return newGatewayDirectoryWithBackend(redisGatewayDirectoryBackend{client: client})
}

func newGatewayDirectoryWithBackend(backend gatewayDirectoryBackend) *GatewayDirectory {
	return &GatewayDirectory{backend: backend}
}

// Register publishes or renews one Gateway endpoint lease.
func (directory *GatewayDirectory) Register(
	ctx context.Context,
	gatewayID, target string,
	ttl time.Duration,
) error {
	if directory == nil || directory.backend == nil {
		return errors.New("gateway directory: backend is nil")
	}
	gatewayID = strings.TrimSpace(gatewayID)
	target = strings.TrimSpace(target)
	if !validGatewayInstanceID(gatewayID) {
		return errors.New("gateway directory: invalid instance ID")
	}
	if !validGatewayTarget(target) {
		return errors.New("gateway directory: target must be host:port")
	}
	if ttl <= 0 {
		return errors.New("gateway directory: TTL must be positive")
	}
	if err := directory.backend.Put(ctx, gatewayInstanceKey(gatewayID), target, ttl); err != nil {
		return fmt.Errorf("gateway directory: register %q: %w", gatewayID, err)
	}
	return nil
}

// ResolveGateway resolves the current gRPC endpoint for a live Gateway.
func (directory *GatewayDirectory) ResolveGateway(ctx context.Context, gatewayID string) (string, error) {
	if directory == nil || directory.backend == nil {
		return "", errors.New("gateway directory: backend is nil")
	}
	gatewayID = strings.TrimSpace(gatewayID)
	if !validGatewayInstanceID(gatewayID) {
		return "", errors.New("gateway directory: invalid instance ID")
	}
	target, err := directory.backend.Get(ctx, gatewayInstanceKey(gatewayID))
	if errors.Is(err, redis.Nil) {
		return "", ErrGatewayInstanceNotFound
	}
	if err != nil {
		return "", fmt.Errorf("gateway directory: resolve %q: %w", gatewayID, err)
	}
	target = strings.TrimSpace(target)
	if !validGatewayTarget(target) {
		return "", ErrGatewayInstanceNotFound
	}
	return target, nil
}

// ListGateways returns all live Gateway instance leases. It is used only by
// low-frequency debug fan-out and never by the request or push hot path.
func (directory *GatewayDirectory) ListGateways(ctx context.Context) (map[string]string, error) {
	if directory == nil || directory.backend == nil {
		return nil, errors.New("gateway directory: backend is nil")
	}
	entries, err := directory.backend.List(ctx, gatewayInstanceKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("gateway directory: list: %w", err)
	}
	result := make(map[string]string, len(entries))
	for key, target := range entries {
		gatewayID := strings.TrimPrefix(key, gatewayInstanceKeyPrefix)
		target = strings.TrimSpace(target)
		if validGatewayInstanceID(gatewayID) && validGatewayTarget(target) {
			result[gatewayID] = target
		}
	}
	return result, nil
}

// Unregister removes the lease only if it still points to target. The compare
// prevents a delayed shutdown from deleting a newer registration with the
// same human-readable instance ID.
func (directory *GatewayDirectory) Unregister(ctx context.Context, gatewayID, target string) error {
	if directory == nil || directory.backend == nil {
		return errors.New("gateway directory: backend is nil")
	}
	gatewayID = strings.TrimSpace(gatewayID)
	target = strings.TrimSpace(target)
	if !validGatewayInstanceID(gatewayID) || !validGatewayTarget(target) {
		return errors.New("gateway directory: invalid unregister request")
	}
	if err := directory.backend.DeleteIfValue(ctx, gatewayInstanceKey(gatewayID), target); err != nil {
		return fmt.Errorf("gateway directory: unregister %q: %w", gatewayID, err)
	}
	return nil
}

func gatewayInstanceKey(gatewayID string) string {
	return gatewayInstanceKeyPrefix + gatewayID
}

func validGatewayInstanceID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func validGatewayTarget(value string) bool {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil || strings.TrimSpace(host) == "" {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port <= 65535
}

type redisGatewayDirectoryBackend struct {
	client redis.UniversalClient
}

func (backend redisGatewayDirectoryBackend) Put(
	ctx context.Context,
	key, value string,
	ttl time.Duration,
) error {
	if backend.client == nil {
		return errors.New("redis client is nil")
	}
	return backend.client.Set(ctx, key, value, ttl).Err()
}

func (backend redisGatewayDirectoryBackend) Get(ctx context.Context, key string) (string, error) {
	if backend.client == nil {
		return "", errors.New("redis client is nil")
	}
	return backend.client.Get(ctx, key).Result()
}

var deleteGatewayInstanceScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

func (backend redisGatewayDirectoryBackend) DeleteIfValue(
	ctx context.Context,
	key, value string,
) error {
	if backend.client == nil {
		return errors.New("redis client is nil")
	}
	return deleteGatewayInstanceScript.Run(ctx, backend.client, []string{key}, value).Err()
}

func (backend redisGatewayDirectoryBackend) List(ctx context.Context, prefix string) (map[string]string, error) {
	if backend.client == nil {
		return nil, errors.New("redis client is nil")
	}
	keys := make([]string, 0, 8)
	iterator := backend.client.Scan(ctx, 0, prefix+"*", 32).Iterator()
	for iterator.Next(ctx) {
		keys = append(keys, iterator.Val())
	}
	if err := iterator.Err(); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return result, nil
	}
	pipe := backend.client.Pipeline()
	commands := make(map[string]*redis.StringCmd, len(keys))
	for _, key := range keys {
		commands[key] = pipe.Get(ctx, key)
	}
	_, _ = pipe.Exec(ctx)
	for key, command := range commands {
		value, err := command.Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}
