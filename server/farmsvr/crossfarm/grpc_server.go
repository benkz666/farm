package crossfarm

import (
	"context"
	"database/sql"
	"errors"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/outbox"
	"farm/server/shared/store"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VisitorSettler settles cross results on the visitor's authoritative farm.
type VisitorSettler struct {
	runtime Runtime
	now     func() int64
}

// NewVisitorSettler constructs the visitor-side durable settlement boundary.
func NewVisitorSettler(runtime Runtime, now func() int64) *VisitorSettler {
	return &VisitorSettler{runtime: runtime, now: now}
}

// Reserve durably records visitor-side cost/quota before owner adjudication.
func (s *VisitorSettler) Reserve(ctx context.Context, action CrossAction, dayID uint32) (errcode.Code, error) {
	if s == nil || s.runtime == nil {
		return errcode.Internal, errors.New("cross: visitor settler is nil")
	}
	var code errcode.Code
	err := s.runtime.Do(action.VisitorUID, func(visitor *room.FarmActor) error {
		if visitor == nil || visitor.Aggregate == nil {
			return errors.New("cross: visitor actor aggregate is nil")
		}
		code = ReserveVisitor(visitor.Aggregate, VisitorReservation{Action: action, DayID: dayID}, s.now())
		if code == errcode.OK {
			visitor.RequireCrossVisitorFlush(false)
		}
		return nil
	})
	if err != nil {
		return errcode.Internal, err
	}
	return code, nil
}

// Settle applies an owner result to the visitor aggregate.
func (s *VisitorSettler) Settle(ctx context.Context, result CrossResult) (VisitorReward, *farm.PlayerDelta, errcode.Code, error) {
	if s == nil || s.runtime == nil {
		return VisitorReward{}, nil, errcode.Internal, errors.New("cross: visitor settler is nil")
	}
	var reward VisitorReward
	var playerDelta *farm.PlayerDelta
	var code errcode.Code
	err := s.runtime.Do(result.VisitorUID, func(visitor *room.FarmActor) error {
		if visitor == nil || visitor.Aggregate == nil {
			return errors.New("cross: visitor actor aggregate is nil")
		}
		reward, playerDelta, code = SettleVisitor(visitor.Aggregate, result, s.now())
		if result.CropID != 0 {
			key := farm.FruitItem(result.CropID)
			visitor.RecordItemCounts(map[farm.ItemKey]uint32{key: visitor.Aggregate.Items[key]})
		}
		// Timeout 也必须形成 durable barrier：它可能是前一次结算已修改内存、
		// 但 Commit 返回不确定错误后的重投。只有再次落盘成功，outbox 才能 ack。
		visitor.RequireCrossVisitorFlush(true)
		return nil
	})
	if err != nil {
		return VisitorReward{ReqID: result.ReqID}, nil, errcode.Internal, err
	}
	return reward, playerDelta, code, nil
}

// GRPCServer implements CrossFarmService on the local farm runtime.
type GRPCServer struct {
	farmv1.UnimplementedCrossFarmServiceServer
	owner   *Owner
	visitor *VisitorSettler
	owns    func(uint64) bool
	players PlayerDeltaPublisher
	outbox  store.OutboxStore
}

// NewGRPCServer wires owner adjudication and visitor settlement handlers.
func NewGRPCServer(
	owner *Owner,
	visitor *VisitorSettler,
	owns func(uint64) bool,
	players PlayerDeltaPublisher,
	outboxStore store.OutboxStore,
) *GRPCServer {
	return &GRPCServer{owner: owner, visitor: visitor, owns: owns, players: players, outbox: outboxStore}
}

// RegisterCrossFarmService registers CrossFarmService on a gRPC server.
func RegisterCrossFarmService(server *grpc.Server, handler *GRPCServer) {
	farmv1.RegisterCrossFarmServiceServer(server, handler)
}

func (server *GRPCServer) ReserveCrossAction(ctx context.Context, request *farmv1.ReserveCrossActionRequest) (*farmv1.ReserveCrossActionResponse, error) {
	if server == nil || server.visitor == nil || request == nil || request.Action == nil {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	action, ok := actionFromProto(request.Action)
	if !ok || server.owns != nil && !server.owns(action.VisitorUID) {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	code, err := server.visitor.Reserve(ctx, action, request.DayId)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "commit_unavailable")
	}
	return &farmv1.ReserveCrossActionResponse{Err: int32(code)}, nil
}

func (server *GRPCServer) ApplyCrossAction(ctx context.Context, request *farmv1.ApplyCrossActionRequest) (*farmv1.ApplyCrossActionResponse, error) {
	if server == nil || server.owner == nil || request == nil || request.Action == nil {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	action, ok := actionFromProto(request.Action)
	if !ok || server.owns != nil && !server.owns(action.OwnerUID) {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	result, err := server.owner.Apply(ctx, action)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "commit_unavailable")
	}
	return &farmv1.ApplyCrossActionResponse{Result: resultToProto(result)}, nil
}

func (server *GRPCServer) DeliverCrossResult(ctx context.Context, request *farmv1.DeliverCrossResultRequest) (*farmv1.DeliverCrossResultResponse, error) {
	if server == nil || server.visitor == nil || request == nil || request.Result == nil {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	result, ok := resultFromProto(request.Result)
	if !ok || server.owns != nil && !server.owns(result.VisitorUID) {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	reward, playerDelta, code, err := server.visitor.Settle(ctx, result)
	if err != nil {
		return nil, status.Error(codes.Internal, "internal")
	}
	if playerDelta != nil && server.players != nil {
		_ = server.players.PublishPlayerDelta(context.Background(), result.VisitorUID, *playerDelta)
	}
	response := &farmv1.DeliverCrossResultResponse{
		Reward: rewardToProto(reward),
		Err:    int32(code),
	}
	if playerDelta != nil {
		response.PlayerDelta = clientwire.PlayerDeltaToProto(*playerDelta)
	}
	if code == errcode.Timeout {
		response.Err = int32(errcode.OK)
	}
	return response, nil
}

func (server *GRPCServer) AcknowledgeCrossResult(ctx context.Context, request *farmv1.AcknowledgeCrossResultRequest) (*farmv1.Empty, error) {
	if server == nil || server.outbox == nil || request == nil ||
		request.OwnerUid == 0 || request.VisitorUid == 0 || request.ReqId == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	if server.owns != nil && !server.owns(request.OwnerUid) {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	eventID := outbox.CrossResultEventID(request.OwnerUid, request.VisitorUid, request.ReqId)
	if err := server.outbox.MarkOutboxPublished(ctx, eventID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.Internal, "internal")
	}
	return &farmv1.Empty{}, nil
}
