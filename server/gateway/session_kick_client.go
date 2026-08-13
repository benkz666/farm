package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"
)

// GatewayTargetResolver resolves a replaceable Gateway instance. The Redis
// directory is infrastructure discovery; no business state crosses this API.
type GatewayTargetResolver interface {
	ResolveGateway(context.Context, string) (string, error)
}

// PeerSessionKickClient delivers the one Gateway-to-Gateway transport event
// needed for single-session replacement.
type PeerSessionKickClient struct {
	pool     *grpcx.Pool
	targets  map[string]string
	resolver GatewayTargetResolver
}

func NewPeerSessionKickClient(
	pool *grpcx.Pool,
	targets map[string]string,
	resolver GatewayTargetResolver,
) *PeerSessionKickClient {
	copied := make(map[string]string, len(targets))
	for gatewayID, target := range targets {
		copied[gatewayID] = strings.TrimSpace(target)
	}
	return &PeerSessionKickClient{pool: pool, targets: copied, resolver: resolver}
}

func (client *PeerSessionKickClient) PushSessionKick(
	ctx context.Context,
	ref presence.ConnRef,
	uid uint64,
	reason errcode.Code,
) error {
	if client == nil || client.pool == nil || ref.ConnID == 0 || uid == 0 || strings.TrimSpace(ref.GatewayID) == "" {
		return errors.New("gateway: invalid peer session kick")
	}
	target := strings.TrimSpace(client.targets[ref.GatewayID])
	if target == "" && client.resolver != nil {
		var err error
		target, err = client.resolver.ResolveGateway(ctx, ref.GatewayID)
		if err != nil {
			return fmt.Errorf("gateway: resolve peer %q: %w", ref.GatewayID, err)
		}
	}
	if target == "" {
		return fmt.Errorf("gateway: no peer target for %q", ref.GatewayID)
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return fmt.Errorf("gateway: dial peer %q: %w", ref.GatewayID, err)
	}
	_, err = farmv1.NewGatewayPushServiceClient(conn).PushSessionKick(ctx, &farmv1.PushSessionKickRequest{
		ConnectionId: ref.ConnID,
		Uid:          uid,
		Reason:       int32(reason),
	})
	if err != nil {
		return fmt.Errorf("gateway: push session kick: %w", err)
	}
	return nil
}

var _ SessionKickPusher = (*PeerSessionKickClient)(nil)
