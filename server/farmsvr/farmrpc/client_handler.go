package farmrpc

import (
	"context"
	"strings"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/presence"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

// FriendChecker is the Social-owned authorization boundary used by Farm.
type FriendChecker interface {
	AreFriends(context.Context, uint64, uint64) (bool, error)
}

// CrossClientExecutor owns the complete cross-farm workflow inside Farm.
type CrossClientExecutor interface {
	ExecuteClient(context.Context, *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse
	AdvanceTask(context.Context, uint64, uint32, uint32) error
}

type WriteAdmission interface {
	Acquire() bool
	Release()
}

// ClientHandler translates the public typed contract into Farm application
// calls. It deliberately lives in Farm: Gateway never validates or assembles
// Farm business payloads.
type ClientHandler struct {
	commands  *Handler
	friends   FriendChecker
	cross     CrossClientExecutor
	owns      func(uint64) bool
	admission WriteAdmission
}

func NewClientHandler(
	commands *Handler,
	friends FriendChecker,
	cross CrossClientExecutor,
	owns func(uint64) bool,
	admission ...WriteAdmission,
) *ClientHandler {
	if owns == nil {
		owns = func(uint64) bool { return false }
	}
	handler := &ClientHandler{commands: commands, friends: friends, cross: cross, owns: owns}
	if len(admission) != 0 {
		handler.admission = admission[0]
	}
	return handler
}

func (handler *ClientHandler) ExecuteClient(ctx context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	if handler == nil || handler.commands == nil || request == nil || request.Envelope == nil || request.Uid == 0 {
		return errorClientResponse(request, errcode.BadRequest)
	}
	if isDurableWriteCommand(request.Envelope.Cmd) && handler.admission != nil {
		if !handler.admission.Acquire() {
			return errorClientResponse(request, errcode.RateLimited)
		}
		defer handler.admission.Release()
	}
	switch request.Envelope.Cmd {
	case 200:
		return handler.enterFarm(ctx, request)
	case 202:
		return &farmv1.ClientCommandResponse{
			Envelope:   responseEnvelope(request.Envelope, errcode.OK, &publicv3.CommandResponse{}),
			RoomAction: farmv1.RoomAction_ROOM_ACTION_UNSUBSCRIBE,
		}
	case 204:
		return handler.syncFarm(ctx, request)
	case 212, 214, 216, 222:
		if request.ActiveFarmUid != 0 && request.ActiveFarmUid != request.Uid {
			if handler.cross == nil {
				return errorClientResponse(request, errcode.Internal)
			}
			return handler.cross.ExecuteClient(ctx, request)
		}
	}
	return handler.selfCommand(request)
}

func isDurableWriteCommand(command uint32) bool {
	switch command {
	case 206, 208, 210, 212, 214, 216, 218, 220, 222,
		302, 304,
		502, 504,
		602, 608, 610, 614:
		return true
	default:
		return false
	}
}

func (handler *ClientHandler) enterFarm(ctx context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	payload := request.Envelope.GetEnterFarmRequest()
	if payload == nil {
		return errorClientResponse(request, errcode.BadRequest)
	}
	ownerUID := payload.OwnerUid
	if ownerUID == 0 {
		ownerUID = request.Uid
	}
	relation, code := handler.relation(ctx, request.Uid, ownerUID)
	if code != errcode.OK {
		return errorClientResponse(request, code)
	}
	result := handler.commands.Execute(CommandRequest{
		Operation: OperationEnterFarm, FarmUID: ownerUID,
		Originator: connRefFromProto(request.Originator),
	})
	wire := resultEnvelope(request.Envelope, result)
	if wire.Err != int32(errcode.OK) {
		return &farmv1.ClientCommandResponse{Envelope: wire}
	}
	response := wire.GetEnterFarmResponse()
	if response == nil {
		return errorClientResponse(request, errcode.Internal)
	}
	response.Relation = relation
	response.TimeProfileMutable = false
	if relation == "FRIEND" {
		snapshot := clientwire.FarmSnapshotFromProto(response.Snapshot)
		response.Snapshot = clientwire.FarmSnapshotToProto(farm.VisitorSafeFarmSnapshot(snapshot))
		var advanceErr error
		if handler.owns(request.Uid) {
			advanceErr = handler.commands.advanceTask(request.Uid, store.TaskVisitID, 1)
		} else if handler.cross != nil {
			advanceErr = handler.cross.AdvanceTask(ctx, request.Uid, store.TaskVisitID, 1)
		}
		if advanceErr != nil {
			telemetry.L().Error("farm advance visit task failed",
				"component", "farmrpc", "op", "advance_visit_task", "uid", request.Uid, "err", advanceErr.Error())
		}
	}
	return &farmv1.ClientCommandResponse{
		Envelope: wire, RoomAction: farmv1.RoomAction_ROOM_ACTION_SUBSCRIBE,
		RoomUid: ownerUID, RoomSeq: result.FarmSeq,
	}
}

// AdvanceTask implements the internal typed Farm-to-Farm task boundary.
func (handler *ClientHandler) AdvanceTask(_ context.Context, uid uint64, taskID, amount uint32) errcode.Code {
	if handler == nil || handler.commands == nil || uid == 0 || taskID == 0 || amount == 0 {
		return errcode.BadRequest
	}
	if err := handler.commands.advanceTask(uid, taskID, amount); err != nil {
		return errcode.Internal
	}
	return errcode.OK
}

func (handler *ClientHandler) syncFarm(ctx context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	payload := request.Envelope.GetSyncFarmRequest()
	if payload == nil || request.ActiveFarmUid == 0 {
		return errorClientResponse(request, errcode.BadRequest)
	}
	ownerUID := payload.OwnerUid
	if ownerUID == 0 {
		ownerUID = request.ActiveFarmUid
	}
	if ownerUID != request.ActiveFarmUid {
		return errorClientResponse(request, errcode.NotOwner)
	}
	if _, code := handler.relation(ctx, request.Uid, ownerUID); code != errcode.OK {
		return errorClientResponse(request, code)
	}
	result := handler.commands.Execute(CommandRequest{
		Operation: OperationSyncFarm, FarmUID: ownerUID,
		Originator: connRefFromProto(request.Originator), SyncRequest: payload,
	})
	wire := resultEnvelope(request.Envelope, result)
	if response := wire.GetSyncFarmResponse(); response != nil {
		response.TimeProfileMutable = false
	}
	return &farmv1.ClientCommandResponse{Envelope: wire, RoomUid: ownerUID, RoomSeq: result.FarmSeq}
}

func (handler *ClientHandler) selfCommand(request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	command := request.Envelope.Cmd
	clientRequest := request.Envelope.GetCommandRequest()
	if clientRequest == nil {
		return errorClientResponse(request, errcode.BadRequest)
	}
	operation, ok := operationForClientCommand(command)
	if !ok {
		return errorClientResponse(request, errcode.BadRequest)
	}
	if clientRequest.OwnerUid != 0 && clientRequest.OwnerUid != request.Uid {
		return errorClientResponse(request, errcode.NotOwner)
	}
	result := handler.commands.Execute(CommandRequest{
		Operation: operation, FarmUID: request.Uid,
		Originator:    connRefFromProto(request.Originator),
		ClientCommand: command, ClientRequest: clientRequest,
	})
	return &farmv1.ClientCommandResponse{Envelope: resultEnvelope(request.Envelope, result), RoomSeq: result.FarmSeq}
}

func (handler *ClientHandler) relation(ctx context.Context, viewerUID, ownerUID uint64) (string, errcode.Code) {
	if viewerUID == ownerUID {
		return "SELF", errcode.OK
	}
	if handler.friends == nil {
		return "", errcode.NotFriend
	}
	value, err := handler.friends.AreFriends(ctx, viewerUID, ownerUID)
	if err != nil {
		return "", errcode.Internal
	}
	if !value {
		return "", errcode.NotFriend
	}
	return "FRIEND", errcode.OK
}

func operationForClientCommand(command uint32) (Operation, bool) {
	switch {
	case command >= 206 && command <= 220 && command%2 == 0:
		return OperationPlotAction, true
	case command == 302 || command == 304:
		return OperationShop, true
	case command == 500 || command == 502 || command == 504:
		return OperationPet, true
	case command == 600:
		return OperationTaskList, true
	case command == 602:
		return OperationTaskClaim, true
	case command == 604:
		return OperationMailList, true
	case command == 606:
		return OperationMailRead, true
	case command == 608:
		return OperationMailClaim, true
	case command == 610:
		return OperationMailDelete, true
	case command == 612:
		return OperationCodexList, true
	case command == 614:
		return OperationDailyLogin, true
	default:
		return "", false
	}
}

func resultEnvelope(request *publicv3.WireEnvelope, result CommandResponse) *publicv3.WireEnvelope {
	if result.Err != errcode.OK {
		return responseEnvelope(request, result.Err, &publicv3.CommandResponse{})
	}
	switch request.Cmd {
	case 200:
		if result.EnterFarmResponse == nil {
			return responseEnvelope(request, errcode.Internal, &publicv3.CommandResponse{})
		}
		return &publicv3.WireEnvelope{
			Cmd: request.Cmd, ClientSeq: request.ClientSeq,
			Payload: &publicv3.WireEnvelope_EnterFarmResponse{EnterFarmResponse: result.EnterFarmResponse},
		}
	case 204:
		if result.SyncFarmResponse == nil {
			return responseEnvelope(request, errcode.Internal, &publicv3.CommandResponse{})
		}
		return &publicv3.WireEnvelope{
			Cmd: request.Cmd, ClientSeq: request.ClientSeq,
			Payload: &publicv3.WireEnvelope_SyncFarmResponse{SyncFarmResponse: result.SyncFarmResponse},
		}
	}
	response := result.ClientResponse
	if response == nil {
		return responseEnvelope(request, errcode.Internal, &publicv3.CommandResponse{})
	}
	return responseEnvelope(request, errcode.OK, response)
}

func responseEnvelope(request *publicv3.WireEnvelope, code errcode.Code, response *publicv3.CommandResponse) *publicv3.WireEnvelope {
	if response == nil {
		response = &publicv3.CommandResponse{}
	}
	return &publicv3.WireEnvelope{
		Cmd: request.GetCmd(), ClientSeq: request.GetClientSeq(), Err: int32(code),
		Payload: &publicv3.WireEnvelope_CommandResponse{CommandResponse: response},
	}
}

func connRefFromProto(ref *farmv1.ConnRef) presence.ConnRef {
	if ref == nil {
		return presence.ConnRef{}
	}
	return presence.ConnRef{ConnID: ref.ConnId, GatewayID: strings.TrimSpace(ref.GatewayId)}
}
