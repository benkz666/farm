package crossfarm

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
)

// FriendAuthorizer is Social's typed authorization boundary used by Farm.
type FriendAuthorizer interface {
	AreFriends(context.Context, uint64, uint64) (bool, error)
}

// ClientCoordinator owns the complete cross-farm workflow. Gateway routes the
// request to the visitor's Farm and remains unaware of reservations, outbox
// acknowledgements, owner adjudication and visitor settlement.
type ClientCoordinator struct {
	friends FriendAuthorizer
	local   *GRPCServer
	remote  *GRPCClient
	now     func() int64
	profile func() string
	nextID  atomic.Uint64
}

func NewClientCoordinator(
	friends FriendAuthorizer,
	local *GRPCServer,
	remote *GRPCClient,
	now func() int64,
	profile func() string,
) *ClientCoordinator {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if profile == nil {
		profile = func() string { return gameconfig.TimeProfileDemo }
	}
	coordinator := &ClientCoordinator{friends: friends, local: local, remote: remote, now: now, profile: profile}
	coordinator.nextID.Store(crossRequestSeed())
	return coordinator
}

func (coordinator *ClientCoordinator) ExecuteClient(ctx context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	if coordinator == nil || coordinator.local == nil || coordinator.remote == nil || coordinator.friends == nil ||
		request == nil || request.Uid == 0 || request.ActiveFarmUid == 0 || request.Envelope == nil {
		return crossClientError(request, errcode.BadRequest)
	}
	payload := request.Envelope.GetCommandRequest()
	ownerUID := request.ActiveFarmUid
	if payload == nil || ownerUID == request.Uid || payload.OwnerUid != ownerUID || payload.PlotIndex > 255 {
		return crossClientError(request, errcode.NotOwner)
	}
	friends, err := coordinator.friends.AreFriends(ctx, request.Uid, ownerUID)
	if err != nil {
		return crossClientError(request, errcode.Internal)
	}
	if !friends {
		return crossClientError(request, errcode.NotFriend)
	}

	kind, ok := clientCrossActionKind(request.Envelope.Cmd)
	if !ok {
		return crossClientError(request, errcode.BadRequest)
	}
	action := CrossAction{
		ReqID: coordinator.nextID.Add(1), Kind: kind,
		VisitorUID: request.Uid, OwnerUID: ownerUID,
		PlotIndex: uint8(payload.PlotIndex), FriendshipVerified: true,
	}
	if kind == Steal {
		if payload.CropId > 0xffff {
			return crossClientError(request, errcode.BadRequest)
		}
		crop, exists := gameconfig.CropByID(uint16(payload.CropId))
		if !exists {
			return crossClientError(request, errcode.BadRequest)
		}
		action.CropID = uint16(payload.CropId)
		action.Compensation = gameconfig.StealCompensation(crop)
	}
	dayID := uint32(0)
	if kind != Steal {
		dayID = gameconfig.LogicDayID(coordinator.profile(), coordinator.now())
	}

	if coordinator.remote.CanExecuteCrossAction(action) {
		action.Originator = connRefFromProto(request.Originator)
		response, callErr := coordinator.local.ExecuteCrossAction(ctx, &farmv1.ExecuteCrossActionRequest{
			Action: actionToProto(action), DayId: dayID,
		})
		if callErr != nil {
			return crossClientError(request, errcode.Internal)
		}
		return crossExecutionResponse(request, response)
	}

	reserve, reserveErr := coordinator.local.ReserveCrossAction(ctx, &farmv1.ReserveCrossActionRequest{
		Action: actionToProto(action), DayId: dayID,
	})
	if reserveErr != nil {
		return crossClientError(request, errcode.Internal)
	}
	reserveCode := errcode.Code(reserve.Err)
	if reserveCode != errcode.OK && reserveCode != errcode.DuplicateOK {
		return crossClientError(request, reserveCode)
	}

	result, applyErr := coordinator.applyWithRetry(ctx, action)
	if applyErr != nil {
		return crossClientError(request, errcode.Internal)
	}
	settled, settleErr := coordinator.local.DeliverCrossResult(ctx, &farmv1.DeliverCrossResultRequest{
		Result: resultToProto(result),
	})
	if settleErr != nil {
		return crossClientError(request, errcode.Internal)
	}
	_ = coordinator.remote.EnqueueCrossResultAck(result.OwnerUID, result.VisitorUID, result.ReqID)
	return crossSettledResponse(request, result, settled)
}

// AdvanceTask routes a Farm-owned side effect to the Farm shard that owns uid.
func (coordinator *ClientCoordinator) AdvanceTask(ctx context.Context, uid uint64, taskID, amount uint32) error {
	if coordinator == nil || coordinator.remote == nil {
		return errors.New("crossfarm: task route is unavailable")
	}
	return coordinator.remote.AdvanceTask(ctx, uid, taskID, amount)
}

func (coordinator *ClientCoordinator) applyWithRetry(ctx context.Context, action CrossAction) (CrossResult, error) {
	var result CrossResult
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		result, err = coordinator.remote.ApplyCrossAction(ctx, action)
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			break
		}
	}
	return CrossResult{}, err
}

func clientCrossActionKind(command uint32) (ActionKind, bool) {
	switch command {
	case 212:
		return Water, true
	case 214:
		return RemoveWeed, true
	case 216:
		return RemovePest, true
	case 222:
		return Steal, true
	default:
		return "", false
	}
}

func crossExecutionResponse(request *farmv1.ClientCommandRequest, execution *farmv1.ExecuteCrossActionResponse) *farmv1.ClientCommandResponse {
	if execution == nil || execution.Result == nil {
		return crossClientError(request, errcode.Internal)
	}
	result, ok := resultFromProto(execution.Result)
	if !ok {
		return crossClientError(request, errcode.Internal)
	}
	reward := rewardFromProto(execution.Reward)
	response := crossRewardResponse(request, errcode.Code(execution.Err), result, reward)
	if execution.PlayerDelta != nil {
		response.Pushes = append(response.Pushes, &publicv3.WireEnvelope{
			Cmd:     clientwire.CommandPlayerDelta,
			Payload: &publicv3.WireEnvelope_PlayerDelta{PlayerDelta: execution.PlayerDelta},
		})
	}
	if execution.FarmDelta != nil {
		response.Pushes = append(response.Pushes, &publicv3.WireEnvelope{
			Cmd:     clientwire.CommandFarmDelta,
			Payload: &publicv3.WireEnvelope_FarmDelta{FarmDelta: execution.FarmDelta},
		})
	}
	if execution.AckRequired && execution.OwnerCommitted {
		// Same-instance executions normally do not require this; retaining the
		// flag makes the response safe if the persistence implementation changes.
	}
	return response
}

func crossSettledResponse(request *farmv1.ClientCommandRequest, result CrossResult, settled *farmv1.DeliverCrossResultResponse) *farmv1.ClientCommandResponse {
	if settled == nil {
		return crossClientError(request, errcode.Internal)
	}
	response := crossRewardResponse(request, errcode.Code(settled.Err), result, rewardFromProto(settled.Reward))
	if settled.PlayerDelta != nil {
		response.Pushes = append(response.Pushes, &publicv3.WireEnvelope{
			Cmd:     clientwire.CommandPlayerDelta,
			Payload: &publicv3.WireEnvelope_PlayerDelta{PlayerDelta: settled.PlayerDelta},
		})
	}
	return response
}

func crossRewardResponse(request *farmv1.ClientCommandRequest, code errcode.Code, result CrossResult, reward VisitorReward) *farmv1.ClientCommandResponse {
	command, sequence := uint32(0), uint32(0)
	if request != nil && request.Envelope != nil {
		command, sequence = request.Envelope.Cmd, request.Envelope.ClientSeq
	}
	clientResponse := &publicv3.CommandResponse{}
	if code == errcode.OK || command == 222 && code == errcode.StealIntercepted {
		clientResponse.VisitorReward = clientwire.NewVisitorRewardCommandResponse(
			result.ReqID, reward.ExpGained, reward.CoinGained,
			uint32(reward.CropID), uint32(reward.Amount), reward.Compensation, uint32(reward.DogType),
		).VisitorReward
	}
	return &farmv1.ClientCommandResponse{Envelope: &publicv3.WireEnvelope{
		Cmd: command, ClientSeq: sequence, Err: int32(code),
		Payload: &publicv3.WireEnvelope_CommandResponse{CommandResponse: clientResponse},
	}}
}

func crossClientError(request *farmv1.ClientCommandRequest, code errcode.Code) *farmv1.ClientCommandResponse {
	return crossRewardResponse(request, code, CrossResult{}, VisitorReward{})
}

func crossRequestSeed() uint64 {
	var seed [8]byte
	if _, err := cryptorand.Read(seed[:]); err == nil {
		return binary.LittleEndian.Uint64(seed[:])
	}
	return uint64(time.Now().UnixNano())
}
