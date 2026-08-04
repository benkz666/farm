package farmrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/store"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GatewayPushClient delivers internal push RPCs to Gateway instances.
type GatewayPushClient struct {
	pool    *grpcx.Pool
	targets map[string]string
}

// NewGatewayPushClient constructs a Gateway push client keyed by gateway ID.
func NewGatewayPushClient(pool *grpcx.Pool, targets map[string]string) *GatewayPushClient {
	copied := make(map[string]string, len(targets))
	for gatewayID, target := range targets {
		copied[gatewayID] = target
	}
	return &GatewayPushClient{pool: pool, targets: copied}
}

func (client *GatewayPushClient) service(ctx context.Context, gatewayID string) (farmv1.GatewayPushServiceClient, error) {
	if client == nil || client.pool == nil {
		return nil, fmt.Errorf("farmrpc: gateway push client is nil")
	}
	target := client.targets[gatewayID]
	if target == "" {
		return nil, fmt.Errorf("farmrpc: no gRPC target configured for gateway %q", gatewayID)
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return nil, err
	}
	return farmv1.NewGatewayPushServiceClient(conn), nil
}

// GRPCDeltaPusher implements DeltaBatchPusher over gRPC.
type GRPCDeltaPusher struct {
	client *GatewayPushClient
}

// NewGRPCDeltaPusher constructs a Farm-to-Gateway Delta batch pusher.
func NewGRPCDeltaPusher(client *GatewayPushClient) *GRPCDeltaPusher {
	return &GRPCDeltaPusher{client: client}
}

// PushBatch sends one pre-encoded Envelope to many connections on one Gateway.
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
		_, pushErr := service.PushFarmDeltaBatch(ctx, &farmv1.PushFarmDeltaBatchRequest{
			ConnIds:  batch.ConnIDs,
			Envelope: batch.Envelope,
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
	body, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("farmrpc: encode PlayerDelta: %w", err)
	}
	service, err := pusher.client.service(ctx, ref.GatewayID)
	if err != nil {
		return err
	}
	_, err = service.PushPlayerDelta(ctx, &farmv1.PushPlayerDeltaRequest{
		ConnectionId: ref.ConnID,
		Uid:          uid,
		DeltaJson:    body,
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
	service, err := pusher.client.service(ctx, ref.GatewayID)
	if err != nil {
		return err
	}
	_, err = service.PushTaskNotify(ctx, &farmv1.PushTaskNotifyRequest{
		ConnectionId: ref.ConnID,
		Uid:          uid,
		Task:         taskToProto(task),
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
	service, err := pusher.client.service(ctx, ref.GatewayID)
	if err != nil {
		return err
	}
	_, err = service.PushMailNotify(ctx, &farmv1.PushMailNotifyRequest{
		ConnectionId: ref.ConnID,
		Uid:          uid,
		Kind:         kind,
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

func taskToProto(task store.Task) *farmv1.Task {
	return &farmv1.Task{
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
