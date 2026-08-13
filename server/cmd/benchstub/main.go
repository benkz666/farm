// Command benchstub provides deterministic Farm and Social gRPC dependencies
// for isolated Gateway performance tests. It is never part of production.
package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

type farmStub struct {
	farmv1.UnimplementedFarmCommandServiceServer
	snapshot *publicv3.FarmSnapshot
}

func (stub *farmStub) Execute(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	return stub.response(request), nil
}

func (stub *farmStub) ExecuteStream(stream farmv1.FarmCommandService_ExecuteStreamServer) error {
	for {
		request, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(&farmv1.StreamExecuteResponse{
			RequestId: request.GetRequestId(),
			Response:  stub.response(request.GetRequest()),
		}); err != nil {
			return err
		}
	}
}

func (stub *farmStub) ExecuteBatchStream(stream farmv1.FarmCommandService_ExecuteBatchStreamServer) error {
	for {
		batch, err := stream.Recv()
		if err != nil {
			return err
		}
		responses := make([]*farmv1.StreamExecuteResponse, 0, len(batch.GetRequests()))
		for _, request := range batch.GetRequests() {
			responses = append(responses, &farmv1.StreamExecuteResponse{
				RequestId: request.GetRequestId(),
				Response:  stub.response(request.GetRequest()),
			})
		}
		if err := stream.Send(&farmv1.StreamExecuteBatchResponse{Responses: responses}); err != nil {
			return err
		}
	}
}

func (stub *farmStub) response(request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	requestEnvelope := request.GetEnvelope()
	if request == nil || request.GetRouteUid() == 0 || requestEnvelope == nil {
		return &farmv1.ClientCommandResponse{Envelope: &publicv3.WireEnvelope{Err: 1001}}
	}
	responseEnvelope := &publicv3.WireEnvelope{
		Cmd:       requestEnvelope.GetCmd(),
		ClientSeq: requestEnvelope.GetClientSeq(),
	}
	switch requestEnvelope.GetCmd() {
	case 200:
		responseEnvelope.Payload = &publicv3.WireEnvelope_EnterFarmResponse{
			EnterFarmResponse: &publicv3.EnterFarmResponse{Snapshot: stub.snapshot, FarmSeq: 1, ServerTime: 1},
		}
	case 204:
		responseEnvelope.Payload = &publicv3.WireEnvelope_SyncFarmResponse{
			SyncFarmResponse: &publicv3.SyncFarmResponse{Snapshot: stub.snapshot, FarmSeq: 1, ServerTime: 1},
		}
	default:
		responseEnvelope.Payload = &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}}
	}
	return &farmv1.ClientCommandResponse{Envelope: responseEnvelope}
}

type socialStub struct {
	farmv1.UnimplementedSocialServiceServer
}

func (*socialStub) ExecuteClientCommand(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	envelope := request.GetEnvelope()
	return &farmv1.ClientCommandResponse{Envelope: &publicv3.WireEnvelope{
		Cmd:       envelope.GetCmd(),
		ClientSeq: envelope.GetClientSeq(),
		Payload:   &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}},
	}}, nil
}

func (*socialStub) AreFriends(context.Context, *farmv1.AreFriendsRequest) (*farmv1.AreFriendsResponse, error) {
	return &farmv1.AreFriendsResponse{Value: true}, nil
}

func (*socialStub) ListFriends(context.Context, *farmv1.UidRequest) (*farmv1.ListFriendsResponse, error) {
	return &farmv1.ListFriendsResponse{}, nil
}

func (*socialStub) FindUser(_ context.Context, request *farmv1.FindUserRequest) (*farmv1.Friend, error) {
	return &farmv1.Friend{Uid: 1, Nickname: request.GetUsername()}, nil
}

func (*socialStub) CountFriends(context.Context, *farmv1.UidRequest) (*farmv1.CountFriendsResponse, error) {
	return &farmv1.CountFriendsResponse{}, nil
}

func main() {
	farmAddr := flag.String("farm-addr", ":9210", "Farm stub listen address")
	socialAddr := flag.String("social-addr", ":9204", "Social stub listen address")
	token := flag.String("token", "perf-internal-token", "internal bearer token")
	enterBytes := flag.Int("enter-bytes", 4800, "approximate EnterFarm JSON payload bytes")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stub := &farmStub{snapshot: enterSnapshot(*enterBytes)}
	servers := []struct {
		addr     string
		register func(*grpc.Server)
	}{
		{addr: *farmAddr, register: func(server *grpc.Server) { farmv1.RegisterFarmCommandServiceServer(server, stub) }},
		{addr: *socialAddr, register: func(server *grpc.Server) { farmv1.RegisterSocialServiceServer(server, &socialStub{}) }},
	}
	for _, definition := range servers {
		listener, err := net.Listen("tcp", definition.addr)
		if err != nil {
			panic(err)
		}
		server := grpc.NewServer(grpcx.ServerOptions([]byte(*token))...)
		definition.register(server)
		go func() {
			if err := server.Serve(listener); err != nil {
				panic(err)
			}
		}()
		go func() {
			<-ctx.Done()
			server.Stop()
		}()
	}
	<-ctx.Done()
}

func enterSnapshot(targetBytes int) *publicv3.FarmSnapshot {
	padding := targetBytes - 64
	if padding < 0 {
		padding = 0
	}
	return &publicv3.FarmSnapshot{OwnerUid: 1, Nickname: strings.Repeat("x", padding)}
}
