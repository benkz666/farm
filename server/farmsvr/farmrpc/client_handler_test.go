package farmrpc

import (
	"context"
	"testing"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
)

type crossClientExecutorFunc func(context.Context, *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse

func (execute crossClientExecutorFunc) ExecuteClient(
	ctx context.Context,
	request *farmv1.ClientCommandRequest,
) *farmv1.ClientCommandResponse {
	return execute(ctx, request)
}

func (crossClientExecutorFunc) AdvanceTask(context.Context, uint64, uint32, uint32) error {
	return nil
}

func TestClientHandlerDelegatesCrossFarmCommandWithoutLosingUint64Identity(t *testing.T) {
	const (
		visitorUID = uint64(9007199254740993)
		ownerUID   = uint64(9007199254740995)
	)
	var captured *farmv1.ClientCommandRequest
	cross := crossClientExecutorFunc(func(_ context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
		captured = request
		return &farmv1.ClientCommandResponse{Envelope: responseEnvelope(
			request.Envelope, errcode.OK, &publicv3.CommandResponse{},
		)}
	})
	handler := NewClientHandler(&Handler{}, nil, cross, nil)
	request := &farmv1.ClientCommandRequest{
		Uid: visitorUID, ActiveFarmUid: ownerUID, RouteUid: visitorUID,
		Envelope: &publicv3.WireEnvelope{
			Cmd: 212, ClientSeq: 7,
			Payload: &publicv3.WireEnvelope_CommandRequest{CommandRequest: &publicv3.CommandRequest{
				OwnerUid: ownerUID, PlotIndex: 3,
			}},
		},
	}

	response := handler.ExecuteClient(t.Context(), request)
	if response.GetEnvelope().GetErr() != int32(errcode.OK) {
		t.Fatalf("ExecuteClient err = %d", response.GetEnvelope().GetErr())
	}
	if captured == nil || captured.GetUid() != visitorUID || captured.GetActiveFarmUid() != ownerUID ||
		captured.GetEnvelope().GetCommandRequest().GetOwnerUid() != ownerUID {
		t.Fatalf("cross request lost identity: %#v", captured)
	}
}
