package gateway

import (
	"context"
	"fmt"
	"sort"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DebugServer implements local Gateway debug controls over gRPC.
type DebugServer struct {
	farmv1.UnimplementedDebugServiceServer
	gateway *Gateway
}

// NewDebugServer constructs the Gateway debug gRPC adapter.
func NewDebugServer(gateway *Gateway) *DebugServer {
	return &DebugServer{gateway: gateway}
}

// Advance moves only this Gateway clock forward.
func (server *DebugServer) Advance(_ context.Context, request *farmv1.AdvanceRequest) (*farmv1.AdvanceResponse, error) {
	if server == nil || server.gateway == nil || request == nil || request.Ms <= 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	server.gateway.offsetMs.Add(request.Ms)
	return &farmv1.AdvanceResponse{ServerTime: server.gateway.Now()}, nil
}

// SetTimeProfile hot-switches only this Gateway process.
func (server *DebugServer) SetTimeProfile(_ context.Context, request *farmv1.SetTimeProfileRequest) (*farmv1.SetTimeProfileResponse, error) {
	if server == nil || server.gateway == nil || request == nil ||
		!gameconfig.ValidTimeProfile(request.TimeProfile) ||
		!server.gateway.timeProfiles.Set(request.TimeProfile) {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	return &farmv1.SetTimeProfileResponse{TimeProfile: server.gateway.TimeProfile()}, nil
}

// DebugFanout fans dev-only time controls to Farm and peer Gateway instances.
type DebugFanout struct {
	pool         *grpcx.Pool
	farmTargets  map[string]string
	gatewayPeers map[string]string
	localGateway string
	directory    GatewayPeerDirectory
}

// GatewayPeerDirectory lists the current dynamically registered Gateway Pods.
type GatewayPeerDirectory interface {
	ListGateways(ctx context.Context) (map[string]string, error)
}

// NewDebugFanout constructs a debug fan-out client.
func NewDebugFanout(
	pool *grpcx.Pool,
	farmTargets, gatewayPeers map[string]string,
	localGateway string,
	directories ...GatewayPeerDirectory,
) *DebugFanout {
	fanout := &DebugFanout{
		pool:         pool,
		farmTargets:  copyTargets(farmTargets),
		gatewayPeers: copyTargets(gatewayPeers),
		localGateway: localGateway,
	}
	if len(directories) > 0 {
		fanout.directory = directories[0]
	}
	return fanout
}

func copyTargets(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// Advance fans a clock advance to every Farm and peer Gateway.
func (fanout *DebugFanout) Advance(ctx context.Context, ms int64) error {
	if fanout == nil || fanout.pool == nil || ms <= 0 {
		return fmt.Errorf("gateway: invalid debug advance fanout")
	}
	for _, farmID := range sortedKeys(fanout.farmTargets) {
		if err := fanout.advanceTarget(ctx, fanout.farmTargets[farmID], ms); err != nil {
			return fmt.Errorf("gateway: debug advance farm %s: %w", farmID, err)
		}
	}
	gatewayPeers, err := fanout.currentGatewayPeers(ctx)
	if err != nil {
		return err
	}
	for _, peerID := range sortedKeys(gatewayPeers) {
		if peerID == fanout.localGateway {
			continue
		}
		if err := fanout.advanceTarget(ctx, gatewayPeers[peerID], ms); err != nil {
			return fmt.Errorf("gateway: debug advance gateway %s: %w", peerID, err)
		}
	}
	return nil
}

// SetTimeProfile fans a profile switch to every Farm and peer Gateway.
func (fanout *DebugFanout) SetTimeProfile(ctx context.Context, profile string) error {
	if fanout == nil || fanout.pool == nil || !gameconfig.ValidTimeProfile(profile) {
		return fmt.Errorf("gateway: invalid debug time profile fanout")
	}
	for _, farmID := range sortedKeys(fanout.farmTargets) {
		if err := fanout.setProfileTarget(ctx, fanout.farmTargets[farmID], profile); err != nil {
			return fmt.Errorf("gateway: debug time profile farm %s: %w", farmID, err)
		}
	}
	gatewayPeers, err := fanout.currentGatewayPeers(ctx)
	if err != nil {
		return err
	}
	for _, peerID := range sortedKeys(gatewayPeers) {
		if peerID == fanout.localGateway {
			continue
		}
		if err := fanout.setProfileTarget(ctx, gatewayPeers[peerID], profile); err != nil {
			return fmt.Errorf("gateway: debug time profile gateway %s: %w", peerID, err)
		}
	}
	return nil
}

func (fanout *DebugFanout) currentGatewayPeers(ctx context.Context) (map[string]string, error) {
	peers := copyTargets(fanout.gatewayPeers)
	if fanout.directory == nil {
		return peers, nil
	}
	dynamic, err := fanout.directory.ListGateways(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: list debug Gateway peers: %w", err)
	}
	for gatewayID, target := range dynamic {
		peers[gatewayID] = target
	}
	return peers, nil
}

func sortedKeys(targets map[string]string) []string {
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (fanout *DebugFanout) advanceTarget(ctx context.Context, target string, ms int64) error {
	conn, err := fanout.pool.Conn(ctx, target)
	if err != nil {
		return err
	}
	_, err = farmv1.NewDebugServiceClient(conn).Advance(ctx, &farmv1.AdvanceRequest{Ms: ms})
	return err
}

func (fanout *DebugFanout) setProfileTarget(ctx context.Context, target, profile string) error {
	conn, err := fanout.pool.Conn(ctx, target)
	if err != nil {
		return err
	}
	_, err = farmv1.NewDebugServiceClient(conn).SetTimeProfile(ctx, &farmv1.SetTimeProfileRequest{
		TimeProfile: profile,
	})
	return err
}

// RegisterDebugService registers Gateway debug handlers.
func RegisterDebugService(server *grpc.Server, gateway *Gateway) {
	farmv1.RegisterDebugServiceServer(server, NewDebugServer(gateway))
}

// RegisterPushService registers Gateway push handlers.
func RegisterPushService(server *grpc.Server, gateway *Gateway) {
	farmv1.RegisterGatewayPushServiceServer(server, NewPushServer(gateway))
}
