package farmrpc

import (
	"context"
	"encoding/json"

	"farm/server/gateway/presence"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
)

// CommandServer implements FarmCommandService over a transport-neutral Handler.
type CommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	handler *Handler
	owns    func(uint64) bool
}

// NewCommandServer registers the farm command executor behind gRPC.
func NewCommandServer(handler *Handler, owns func(uint64) bool) *CommandServer {
	return &CommandServer{handler: handler, owns: owns}
}

// Execute routes one Gateway-authorized command to the local farm runtime.
func (server *CommandServer) Execute(_ context.Context, request *farmv1.ExecuteRequest) (*farmv1.ExecuteResponse, error) {
	if server == nil || server.handler == nil {
		return &farmv1.ExecuteResponse{Err: int32(errcode.Internal)}, nil
	}
	if request == nil || request.FarmUid == 0 || server.owns != nil && !server.owns(request.FarmUid) {
		return &farmv1.ExecuteResponse{Err: int32(errcode.BadRequest)}, nil
	}
	operation, ok := operationFromProtoEnum(request.Operation)
	if !ok {
		return &farmv1.ExecuteResponse{Err: int32(errcode.BadRequest)}, nil
	}
	response := server.handler.Execute(CommandRequest{
		Operation:  operation,
		FarmUID:    request.FarmUid,
		Originator: connRefFromProto(request.Originator),
		Payload:    json.RawMessage(request.PayloadJson),
	})
	return &farmv1.ExecuteResponse{
		Err:         int32(response.Err),
		PayloadJson: response.Payload,
	}, nil
}

func connRefFromProto(ref *farmv1.ConnRef) presence.ConnRef {
	if ref == nil {
		return presence.ConnRef{}
	}
	return presence.ConnRef{ConnID: ref.ConnId, GatewayID: ref.GatewayId}
}

func connRefToProto(ref presence.ConnRef) *farmv1.ConnRef {
	if ref.ConnID == 0 && ref.GatewayID == "" {
		return nil
	}
	return &farmv1.ConnRef{ConnId: ref.ConnID, GatewayId: ref.GatewayID}
}
