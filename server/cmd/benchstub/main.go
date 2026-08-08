// Command benchstub provides deterministic Farm and Social gRPC dependencies
// for isolated Gateway performance tests. It is never part of production.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

type farmStub struct {
	farmv1.UnimplementedFarmCommandServiceServer
	enterPayload []byte
	syncPayload  []byte
}

func (stub *farmStub) Execute(_ context.Context, request *farmv1.ExecuteRequest) (*farmv1.ExecuteResponse, error) {
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

func (stub *farmStub) response(request *farmv1.ExecuteRequest) *farmv1.ExecuteResponse {
	if request == nil || request.FarmUid == 0 {
		return &farmv1.ExecuteResponse{Err: 1001}
	}
	payload := stub.syncPayload
	if request.Operation == farmv1.Operation_OPERATION_ENTER_FARM {
		payload = stub.enterPayload
	}
	return &farmv1.ExecuteResponse{PayloadJson: payload}
}

type socialStub struct {
	farmv1.UnimplementedSocialServiceServer
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
	stub := &farmStub{
		enterPayload: enterPayload(*enterBytes),
		syncPayload:  []byte(`{"farm_seq":"1","server_time":1,"time_profile":"demo"}`),
	}
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

func enterPayload(targetBytes int) []byte {
	base := map[string]any{
		"snapshot":     map[string]any{"plots": []any{}},
		"farm_seq":     "1",
		"server_time":  1,
		"time_profile": "demo",
	}
	encoded, _ := json.Marshal(base)
	if targetBytes <= len(encoded)+20 {
		return encoded
	}
	base["padding"] = strings.Repeat("x", targetBytes-len(encoded)-20)
	encoded, err := json.Marshal(base)
	if err != nil {
		panic(fmt.Errorf("encode payload: %w", err))
	}
	return encoded
}
