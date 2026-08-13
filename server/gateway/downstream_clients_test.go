package gateway

import (
	"context"
	"testing"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"

	"google.golang.org/grpc"
)

type socialCommandServerStub struct {
	farmv1.UnimplementedSocialServiceServer
	request *farmv1.ClientCommandRequest
}

func (server *socialCommandServerStub) ExecuteClientCommand(
	_ context.Context,
	request *farmv1.ClientCommandRequest,
) (*farmv1.ClientCommandResponse, error) {
	server.request = request
	return &farmv1.ClientCommandResponse{Envelope: commandResponseFor(request.Envelope, errcode.OK)}, nil
}

func TestSocialClientForwardsTypedEnvelope(t *testing.T) {
	serverStub := &socialCommandServerStub{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterSocialServiceServer(server, serverStub)
	})
	request := &farmv1.ClientCommandRequest{Uid: 42, Envelope: &publicv3.WireEnvelope{
		Cmd: 410, ClientSeq: 7,
		Payload: &publicv3.WireEnvelope_CommandRequest{CommandRequest: &publicv3.CommandRequest{
			Username: "alice",
		}},
	}}
	response, err := NewSocialClient(pair.Pool, "bufconn").ExecuteClientCommand(t.Context(), request)
	if err != nil || response.GetEnvelope().GetErr() != int32(errcode.OK) ||
		serverStub.request.GetEnvelope().GetCommandRequest().GetUsername() != "alice" {
		t.Fatalf("response=%v request=%v err=%v", response, serverStub.request, err)
	}
}

type sessionKickServerStub struct {
	farmv1.UnimplementedGatewayPushServiceServer
	request *farmv1.PushSessionKickRequest
}

func (server *sessionKickServerStub) PushSessionKick(
	_ context.Context,
	request *farmv1.PushSessionKickRequest,
) (*farmv1.Empty, error) {
	server.request = request
	return &farmv1.Empty{}, nil
}

func TestPeerSessionKickClientUsesTypedGatewayContract(t *testing.T) {
	serverStub := &sessionKickServerStub{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterGatewayPushServiceServer(server, serverStub)
	})
	client := NewPeerSessionKickClient(pair.Pool, map[string]string{"gateway-1": "bufconn"}, nil)
	err := client.PushSessionKick(t.Context(), presence.ConnRef{GatewayID: "gateway-1", ConnID: 9}, 42, errcode.Kicked)
	request := serverStub.request
	if err != nil || request.GetConnectionId() != 9 || request.GetUid() != 42 || request.GetReason() != int32(errcode.Kicked) {
		t.Fatalf("request=%v err=%v", request, err)
	}
}
