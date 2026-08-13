package crossfarm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/telemetry"
)

const (
	crossAckBatchWindow = 10 * time.Millisecond
	crossAckBatchSize   = 256
	crossAckTimeout     = 2 * time.Second
)

type queuedCrossAck struct {
	ownerUID   uint64
	visitorUID uint64
	reqID      uint64
}

type crossAckBatch struct {
	items []queuedCrossAck
	timer *time.Timer
}

// GRPCClient routes typed cross-farm RPCs to the Farm instance that owns a UID.
type GRPCClient struct {
	pool    *grpcx.Pool
	targets map[string]string
	route   RouteLookup
	ackMu   sync.Mutex
	acks    map[string]*crossAckBatch
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
	return &GRPCClient{pool: pool, targets: copied, route: route, acks: make(map[string]*crossAckBatch)}
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

// AdvanceTask delivers a typed side effect to the Farm shard that owns uid.
func (client *GRPCClient) AdvanceTask(ctx context.Context, uid uint64, taskID, amount uint32) error {
	if client == nil || client.pool == nil || client.route == nil || uid == 0 || taskID == 0 || amount == 0 {
		return fmt.Errorf("crossfarm: invalid task advancement")
	}
	farmID, err := client.route.FarmID(uid)
	if err != nil {
		return err
	}
	target := client.targets[farmID]
	if target == "" {
		return fmt.Errorf("crossfarm: no gRPC target configured for %q", farmID)
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return fmt.Errorf("crossfarm: dial %q: %w", farmID, err)
	}
	response, err := farmv1.NewFarmCommandServiceClient(conn).AdvanceTask(ctx, &farmv1.AdvanceTaskRequest{
		Uid: uid, TaskId: taskID, Amount: amount,
	})
	if err != nil {
		return err
	}
	if code := errcode.Code(response.GetErr()); code != errcode.OK {
		return fmt.Errorf("crossfarm: advance task rejected: %d", code)
	}
	return nil
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

// CanExecuteCrossAction reports whether visitor and owner are routed to the
// same Farm instance and can use the one-RPC Saga fast path.
func (client *GRPCClient) CanExecuteCrossAction(action CrossAction) bool {
	if client == nil || client.route == nil || action.VisitorUID == 0 || action.OwnerUID == 0 {
		return false
	}
	visitorFarm, err := client.route.FarmID(action.VisitorUID)
	if err != nil {
		return false
	}
	ownerFarm, err := client.route.FarmID(action.OwnerUID)
	return err == nil && visitorFarm == ownerFarm && client.targets[ownerFarm] != ""
}

// ExecuteCrossAction runs reserve, owner adjudication and visitor settlement
// through one internal RPC when both actors are colocated.
func (client *GRPCClient) ExecuteCrossAction(ctx context.Context, action CrossAction, dayID uint32) (CrossExecution, error) {
	if !client.CanExecuteCrossAction(action) {
		return CrossExecution{}, fmt.Errorf("crossfarm: actors are not colocated")
	}
	service, err := client.service(ctx, action.OwnerUID)
	if err != nil {
		return CrossExecution{}, err
	}
	response, err := service.ExecuteCrossAction(ctx, &farmv1.ExecuteCrossActionRequest{
		Action: actionToProto(action),
		DayId:  dayID,
	})
	if err != nil {
		return CrossExecution{}, err
	}
	result, ok := resultFromProto(response.Result)
	if !ok {
		return CrossExecution{}, fmt.Errorf("crossfarm: empty execute response")
	}
	var playerDelta *farm.PlayerDelta
	if response.PlayerDelta != nil {
		decoded := clientwire.PlayerDeltaFromProto(response.PlayerDelta)
		playerDelta = &decoded
	}
	var farmDelta *farm.FarmDelta
	if response.FarmDelta != nil {
		decoded := clientwire.FarmDeltaFromProto(response.FarmDelta)
		farmDelta = &decoded
	}
	return CrossExecution{
		Result:         result,
		Reward:         rewardFromProto(response.Reward),
		PlayerDelta:    playerDelta,
		FarmDelta:      farmDelta,
		Code:           errcode.Code(response.Err),
		OwnerCommitted: response.OwnerCommitted,
		AckRequired:    response.AckRequired,
	}, nil
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

// EnqueueCrossResultAck batches the best-effort suppression ACK. If a process
// exits before flushing, the durable owner outbox simply redelivers the
// idempotent result, so foreground correctness does not depend on this queue.
func (client *GRPCClient) EnqueueCrossResultAck(ownerUID, visitorUID, reqID uint64) error {
	if client == nil || client.pool == nil || client.route == nil || ownerUID == 0 || visitorUID == 0 || reqID == 0 {
		return fmt.Errorf("crossfarm: invalid result acknowledgement")
	}
	farmID, err := client.route.FarmID(ownerUID)
	if err != nil {
		return err
	}
	if client.targets[farmID] == "" {
		return fmt.Errorf("crossfarm: no gRPC target configured for %q", farmID)
	}

	client.ackMu.Lock()
	batch := client.acks[farmID]
	if batch == nil {
		batch = &crossAckBatch{items: make([]queuedCrossAck, 0, crossAckBatchSize)}
		client.acks[farmID] = batch
	}
	batch.items = append(batch.items, queuedCrossAck{
		ownerUID: ownerUID, visitorUID: visitorUID, reqID: reqID,
	})
	if len(batch.items) == 1 {
		batch.timer = time.AfterFunc(crossAckBatchWindow, func() {
			client.flushCrossAckBatch(farmID)
		})
	}
	if len(batch.items) < crossAckBatchSize {
		client.ackMu.Unlock()
		return nil
	}
	delete(client.acks, farmID)
	if batch.timer != nil {
		batch.timer.Stop()
	}
	client.ackMu.Unlock()
	go client.sendCrossAckBatch(farmID, batch.items)
	return nil
}

func (client *GRPCClient) flushCrossAckBatch(farmID string) {
	client.ackMu.Lock()
	batch := client.acks[farmID]
	delete(client.acks, farmID)
	client.ackMu.Unlock()
	if batch != nil && len(batch.items) > 0 {
		client.sendCrossAckBatch(farmID, batch.items)
	}
}

func (client *GRPCClient) sendCrossAckBatch(farmID string, items []queuedCrossAck) {
	target := client.targets[farmID]
	if target == "" || len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), crossAckTimeout)
	defer cancel()
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		client.logCrossAckError(farmID, len(items), err)
		return
	}
	acknowledgements := make([]*farmv1.AcknowledgeCrossResultRequest, len(items))
	for index, item := range items {
		acknowledgements[index] = &farmv1.AcknowledgeCrossResultRequest{
			OwnerUid: item.ownerUID, VisitorUid: item.visitorUID, ReqId: item.reqID,
		}
	}
	_, err = farmv1.NewCrossFarmServiceClient(conn).AcknowledgeCrossResults(ctx, &farmv1.AcknowledgeCrossResultsRequest{
		Acknowledgements: acknowledgements,
	})
	if err != nil {
		client.logCrossAckError(farmID, len(items), err)
	}
}

func (*GRPCClient) logCrossAckError(farmID string, count int, err error) {
	telemetry.L().Debug("cross result ack batch failed",
		"component", "crossfarm_client",
		"farm_id", farmID,
		"count", count,
		"err", err.Error(),
	)
}
