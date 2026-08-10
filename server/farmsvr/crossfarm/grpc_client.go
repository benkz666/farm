package crossfarm

import (
	"context"
	"fmt"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
)

// GRPCClient routes typed cross-farm RPCs to the Farm instance that owns a UID.
type GRPCClient struct {
	pool    *grpcx.Pool
	targets map[string]string
	route   RouteLookup
}

// RouteLookup resolves a UID to a farm instance ID.
type RouteLookup interface {
	FarmID(uid uint64) (string, error)
}

// NewGRPCClient constructs a routed CrossFarm gRPC client.
func NewGRPCClient(pool *grpcx.Pool, targets map[string]string, route RouteLookup) *GRPCClient {
	copied := make(map[string]string, len(targets))
	for farmID, target := range targets {
		copied[farmID] = target
	}
	return &GRPCClient{pool: pool, targets: copied, route: route}
}

func (client *GRPCClient) service(ctx context.Context, uid uint64) (farmv1.CrossFarmServiceClient, error) {
	if client == nil || client.pool == nil || client.route == nil {
		return nil, fmt.Errorf("crossfarm: gRPC client is nil")
	}
	farmID, err := client.route.FarmID(uid)
	if err != nil {
		return nil, err
	}
	target := client.targets[farmID]
	if target == "" {
		return nil, fmt.Errorf("crossfarm: no gRPC target configured for %q", farmID)
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("crossfarm: dial %q: %w", farmID, err)
	}
	return farmv1.NewCrossFarmServiceClient(conn), nil
}

// ReserveCrossVisitor durably reserves visitor resources through the typed
// cross-farm boundary, avoiding the generic JSON Farm command hot path.
func (client *GRPCClient) ReserveCrossVisitor(ctx context.Context, action CrossAction, dayID uint32) (errcode.Code, error) {
	service, err := client.service(ctx, action.VisitorUID)
	if err != nil {
		return errcode.Internal, err
	}
	response, err := service.ReserveCrossAction(ctx, &farmv1.ReserveCrossActionRequest{
		Action: actionToProto(action), DayId: dayID,
	})
	if err != nil {
		return errcode.Internal, err
	}
	return errcode.Code(response.Err), nil
}

// ApplyCrossAction adjudicates an action on the owner farm.
func (client *GRPCClient) ApplyCrossAction(ctx context.Context, action CrossAction) (CrossResult, error) {
	service, err := client.service(ctx, action.OwnerUID)
	if err != nil {
		return CrossResult{}, err
	}
	response, err := service.ApplyCrossAction(ctx, &farmv1.ApplyCrossActionRequest{
		Action: actionToProto(action),
	})
	if err != nil {
		// CrossFarm RPC 只用 gRPC status 表达传输/服务故障；保留原始
		// status，Gateway 才能按 Unavailable/Deadline/Internal 重试。
		return CrossResult{}, err
	}
	result, ok := resultFromProto(response.Result)
	if !ok {
		return CrossResult{}, fmt.Errorf("crossfarm: empty apply response")
	}
	return result, nil
}

// DeliverCrossResult settles a result on the visitor farm.
func (client *GRPCClient) DeliverCrossResult(ctx context.Context, result CrossResult) (VisitorReward, *farm.PlayerDelta, errcode.Code, error) {
	service, err := client.service(ctx, result.VisitorUID)
	if err != nil {
		return VisitorReward{}, nil, errcode.Internal, err
	}
	response, err := service.DeliverCrossResult(ctx, &farmv1.DeliverCrossResultRequest{
		Result: resultToProto(result),
	})
	if err != nil {
		return VisitorReward{}, nil, errcode.Internal, err
	}
	var playerDelta *farm.PlayerDelta
	if response.PlayerDelta != nil {
		decoded := clientwire.PlayerDeltaFromProto(response.PlayerDelta)
		playerDelta = &decoded
	}
	return rewardFromProto(response.Reward), playerDelta, errcode.Code(response.Err), nil
}

// AcknowledgeCrossResult marks an owner outbox row published after direct visitor settlement.
func (client *GRPCClient) AcknowledgeCrossResult(ctx context.Context, ownerUID, visitorUID, reqID uint64) error {
	service, err := client.service(ctx, ownerUID)
	if err != nil {
		return err
	}
	_, err = service.AcknowledgeCrossResult(ctx, &farmv1.AcknowledgeCrossResultRequest{
		OwnerUid:   ownerUID,
		VisitorUid: visitorUID,
		ReqId:      reqID,
	})
	if err != nil {
		return err
	}
	return nil
}
