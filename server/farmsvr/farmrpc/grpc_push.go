package farmrpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"
	"farm/server/shared/store"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GatewayPushClient delivers internal push RPCs to Gateway instances.
type GatewayPushClient struct {
	pool      *grpcx.Pool
	targets   map[string]string
	resolver  GatewayTargetResolver
	now       func() time.Time
	cacheTTL  time.Duration
	targetTTL sync.Map
}

// GatewayTargetResolver resolves a dynamically registered Gateway instance.
type GatewayTargetResolver interface {
	ResolveGateway(ctx context.Context, gatewayID string) (string, error)
}

type gatewayTargetCacheEntry struct {
	target    string
	expiresAt time.Time
}

const defaultGatewayTargetCacheTTL = 30 * time.Second

// NewGatewayPushClient constructs a Gateway push client keyed by gateway ID.
func NewGatewayPushClient(pool *grpcx.Pool, targets map[string]string) *GatewayPushClient {
	return NewResolvingGatewayPushClient(pool, targets, nil)
}

// NewResolvingGatewayPushClient keeps static targets for Compose compatibility
// and falls back to dynamic discovery for replaceable Kubernetes Gateway Pods.
// Dynamic results are cached locally so Redis is not on the push hot path.
func NewResolvingGatewayPushClient(
	pool *grpcx.Pool,
	targets map[string]string,
	resolver GatewayTargetResolver,
) *GatewayPushClient {
	copied := make(map[string]string, len(targets))
	for gatewayID, target := range targets {
		copied[gatewayID] = strings.TrimSpace(target)
	}
	return &GatewayPushClient{
		pool:     pool,
		targets:  copied,
		resolver: resolver,
		now:      time.Now,
		cacheTTL: defaultGatewayTargetCacheTTL,
	}
}

func (client *GatewayPushClient) service(ctx context.Context, gatewayID string) (farmv1.GatewayPushServiceClient, error) {
	if client == nil || client.pool == nil {
		return nil, fmt.Errorf("farmrpc: gateway push client is nil")
	}
	target, err := client.target(ctx, gatewayID)
	if err != nil {
		return nil, err
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return nil, err
	}
	return farmv1.NewGatewayPushServiceClient(conn), nil
}

func (client *GatewayPushClient) deliverPush(
	ctx context.Context,
	ref presence.ConnRef,
	uid uint64,
	envelope *publicv3.WireEnvelope,
) error {
	service, err := client.service(ctx, ref.GatewayID)
	if err != nil {
		return err
	}
	_, err = service.DeliverPush(ctx, &farmv1.DeliverPushRequest{
		ConnectionId: ref.ConnID,
		Uid:          uid,
		Envelope:     envelope,
	})
	return err
}

func (client *GatewayPushClient) target(ctx context.Context, gatewayID string) (string, error) {
	gatewayID = strings.TrimSpace(gatewayID)
	if gatewayID == "" {
		return "", fmt.Errorf("farmrpc: gateway ID is empty")
	}
	if target := strings.TrimSpace(client.targets[gatewayID]); target != "" {
		return target, nil
	}
	now := time.Now()
	if client.now != nil {
		now = client.now()
	}
	if cached, ok := client.targetTTL.Load(gatewayID); ok {
		entry, valid := cached.(gatewayTargetCacheEntry)
		if valid && entry.target != "" && now.Before(entry.expiresAt) {
			return entry.target, nil
		}
		client.targetTTL.Delete(gatewayID)
	}
	if client.resolver == nil {
		return "", fmt.Errorf("farmrpc: no gRPC target configured for gateway %q", gatewayID)
	}
	target, err := client.resolver.ResolveGateway(ctx, gatewayID)
	if err != nil {
		return "", fmt.Errorf("farmrpc: resolve gateway %q: %w", gatewayID, err)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("farmrpc: resolver returned an empty target for gateway %q", gatewayID)
	}
	ttl := client.cacheTTL
	if ttl <= 0 {
		ttl = defaultGatewayTargetCacheTTL
	}
	client.targetTTL.Store(gatewayID, gatewayTargetCacheEntry{
		target:    target,
		expiresAt: now.Add(ttl),
	})
	return target, nil
}

// GRPCDeltaPusher implements DeltaBatchPusher over gRPC.
type GRPCDeltaPusher struct {
	client *GatewayPushClient
}

// NewGRPCDeltaPusher constructs a Farm-to-Gateway Delta batch pusher.
func NewGRPCDeltaPusher(client *GatewayPushClient) *GRPCDeltaPusher {
	return &GRPCDeltaPusher{client: client}
}

// PushBatch sends one typed FarmDelta to many connections on one Gateway.
func (pusher *GRPCDeltaPusher) PushBatch(ctx context.Context, gatewayID string, batch PushBatch) error {
	if pusher == nil || pusher.client == nil {
		return fmt.Errorf("farmrpc: gRPC Delta pusher is nil")
	}
	service, err := pusher.client.service(ctx, gatewayID)
	if err != nil {
		return fmt.Errorf("farmrpc: push Delta batch: %w", err)
	}
	var lastErr error
	for attempt := 1; attempt <= deltaPushMaxAttempts; attempt++ {
		if batch.Delta == nil {
			return fmt.Errorf("farmrpc: push Delta batch: missing typed delta")
		}
		_, pushErr := service.PushFarmDeltaBatch(ctx, &farmv1.PushFarmDeltaBatchRequest{
			ConnIds: batch.ConnIDs,
			Delta:   batch.Delta,
		})
		if pushErr == nil {
			return nil
		}
		lastErr = fmt.Errorf("farmrpc: push Delta batch: %w", pushErr)
		if st, ok := status.FromError(pushErr); ok {
			if st.Code() != codes.Unavailable && st.Code() != codes.ResourceExhausted {
				return lastErr
			}
		}
		if attempt == deltaPushMaxAttempts {
			break
		}
		delay := time.Duration(attempt) * deltaPushRetryBaseDelay
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// GRPCPlayerDeltaPusher implements PlayerDeltaPusher over gRPC.
type GRPCPlayerDeltaPusher struct {
	client *GatewayPushClient
}

// NewGRPCPlayerDeltaPusher constructs a PlayerDelta pusher.
func NewGRPCPlayerDeltaPusher(client *GatewayPushClient) *GRPCPlayerDeltaPusher {
	return &GRPCPlayerDeltaPusher{client: client}
}

// PushPlayerDelta sends a personal-state update to one Gateway-owned connection.
func (pusher *GRPCPlayerDeltaPusher) PushPlayerDelta(ctx context.Context, ref presence.ConnRef, uid uint64, delta farm.PlayerDelta) error {
	if pusher == nil || pusher.client == nil {
		return fmt.Errorf("farmrpc: gRPC PlayerDelta pusher is nil")
	}
	err := pusher.client.deliverPush(ctx, ref, uid, &publicv3.WireEnvelope{
		Cmd: clientwire.CommandPlayerDelta,
		Payload: &publicv3.WireEnvelope_PlayerDelta{
			PlayerDelta: clientwire.PlayerDeltaToProto(delta),
		},
	})
	if err != nil {
		return fmt.Errorf("farmrpc: push PlayerDelta: %w", err)
	}
	return nil
}

// GRPCTaskNotifyPusher implements TaskNotifyPusher over gRPC.
type GRPCTaskNotifyPusher struct {
	client *GatewayPushClient
}

// NewGRPCTaskNotifyPusher constructs a TaskNotify pusher.
func NewGRPCTaskNotifyPusher(client *GatewayPushClient) *GRPCTaskNotifyPusher {
	return &GRPCTaskNotifyPusher{client: client}
}

// PushTaskNotify sends a task progress update to one Gateway-owned connection.
func (pusher *GRPCTaskNotifyPusher) PushTaskNotify(ctx context.Context, ref presence.ConnRef, uid uint64, task store.Task) error {
	if pusher == nil || pusher.client == nil {
		return fmt.Errorf("farmrpc: gRPC TaskNotify pusher is nil")
	}
	err := pusher.client.deliverPush(ctx, ref, uid, &publicv3.WireEnvelope{
		Cmd: clientwire.CommandTaskNotify,
		Payload: &publicv3.WireEnvelope_TaskNotify{
			TaskNotify: taskToProto(task),
		},
	})
	if err != nil {
		return fmt.Errorf("farmrpc: push TaskNotify: %w", err)
	}
	return nil
}

// GRPCMailNotifyPusher implements MailNotifyPusher over gRPC.
type GRPCMailNotifyPusher struct {
	client *GatewayPushClient
}

// NewGRPCMailNotifyPusher constructs a MailNotify pusher.
func NewGRPCMailNotifyPusher(client *GatewayPushClient) *GRPCMailNotifyPusher {
	return &GRPCMailNotifyPusher{client: client}
}

// PushMailNotify sends an advisory MailNotify hint to one Gateway-owned session.
func (pusher *GRPCMailNotifyPusher) PushMailNotify(ctx context.Context, ref presence.ConnRef, uid uint64, kind string) error {
	if pusher == nil || pusher.client == nil {
		return fmt.Errorf("farmrpc: gRPC MailNotify pusher is nil")
	}
	err := pusher.client.deliverPush(ctx, ref, uid, &publicv3.WireEnvelope{
		Cmd: clientwire.CommandMailNotify,
		Payload: &publicv3.WireEnvelope_MailNotify{
			MailNotify: &publicv3.MailNotify{Kind: kind},
		},
	})
	if err != nil {
		return fmt.Errorf("farmrpc: push MailNotify: %w", err)
	}
	return nil
}

// GRPCSessionKickPusher implements SessionKickPusher over gRPC.
type GRPCSessionKickPusher struct {
	client *GatewayPushClient
}

// NewGRPCSessionKickPusher constructs a SessionKick pusher.
func NewGRPCSessionKickPusher(client *GatewayPushClient) *GRPCSessionKickPusher {
	return &GRPCSessionKickPusher{client: client}
}

// PushSessionKick closes an evicted player connection on its owning Gateway.
func (pusher *GRPCSessionKickPusher) PushSessionKick(ctx context.Context, ref presence.ConnRef, uid uint64, reason errcode.Code) error {
	if pusher == nil || pusher.client == nil {
		return fmt.Errorf("farmrpc: gRPC SessionKick pusher is nil")
	}
	service, err := pusher.client.service(ctx, ref.GatewayID)
	if err != nil {
		return err
	}
	_, err = service.PushSessionKick(ctx, &farmv1.PushSessionKickRequest{
		ConnectionId: ref.ConnID,
		Uid:          uid,
		Reason:       int32(reason),
	})
	if err != nil {
		return fmt.Errorf("farmrpc: push SessionKick: %w", err)
	}
	return nil
}

// GRPCFarmAccessPusher removes a stale farm-room subscription on one Gateway.
type GRPCFarmAccessPusher struct{ client *GatewayPushClient }

func NewGRPCFarmAccessPusher(client *GatewayPushClient) *GRPCFarmAccessPusher {
	return &GRPCFarmAccessPusher{client: client}
}

func (pusher *GRPCFarmAccessPusher) RevokeFarmAccess(ctx context.Context, ref presence.ConnRef, viewerUID, ownerUID uint64) error {
	if pusher == nil || pusher.client == nil {
		return fmt.Errorf("farmrpc: gRPC Farm access pusher is nil")
	}
	service, err := pusher.client.service(ctx, ref.GatewayID)
	if err != nil {
		return err
	}
	_, err = service.RevokeFarmAccess(ctx, &farmv1.RevokeFarmAccessRequest{
		ConnectionId: ref.ConnID, ViewerUid: viewerUID, OwnerUid: ownerUID,
	})
	if err != nil {
		return fmt.Errorf("farmrpc: revoke Farm access: %w", err)
	}
	return nil
}

func taskToProto(task store.Task) *publicv3.Task {
	return &publicv3.Task{
		Id:         task.ID,
		DayKey:     task.DayKey,
		Kind:       task.Kind,
		Title:      task.Title,
		Progress:   task.Progress,
		Target:     task.Target,
		RewardCoin: task.RewardCoin,
		Claimed:    task.Claimed,
	}
}
