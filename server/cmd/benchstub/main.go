// Command benchstub provides deterministic Farm and Social gRPC dependencies
// for isolated Gateway performance tests. It is never part of production.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

type farmStub struct {
	farmv1.UnimplementedFarmCommandServiceServer
	snapshot *publicv3.FarmSnapshot
	delay    time.Duration
}

func (stub *farmStub) Execute(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	if stub.delay > 0 {
		time.Sleep(stub.delay)
	}
	return stub.response(request), nil
}

func (stub *farmStub) ExecuteStream(stream farmv1.FarmCommandService_ExecuteStreamServer) error {
	for {
		request, err := stream.Recv()
		if err != nil {
			return err
		}
		if stub.delay > 0 {
			time.Sleep(stub.delay)
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
	completed := make(chan *farmv1.StreamExecuteBatchResponse, 256)
	sendErr := make(chan error, 1)
	go func() {
		for response := range completed {
			if err := stream.Send(response); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()
	var workers sync.WaitGroup
	slots := make(chan struct{}, 128)
	for {
		batch, err := stream.Recv()
		if err != nil {
			workers.Wait()
			close(completed)
			if sendFailure := <-sendErr; sendFailure != nil {
				return sendFailure
			}
			return err
		}
		slots <- struct{}{}
		workers.Add(1)
		go func(batch *farmv1.StreamExecuteBatchRequest) {
			defer workers.Done()
			defer func() { <-slots }()
			if stub.delay > 0 {
				time.Sleep(stub.delay)
			}
			responses := make([]*farmv1.StreamExecuteResponse, 0, len(batch.GetRequests()))
			for _, request := range batch.GetRequests() {
				if request.GetRequest() == nil && request.GetFastSyncUid() != 0 {
					responses = append(responses, &farmv1.StreamExecuteResponse{
						RequestId: request.GetRequestId(), FastSyncClientSeq: request.GetFastSyncClientSeq(),
						FastSyncUid: request.GetFastSyncUid(), FastSyncFarmSeq: 1,
						FastSyncCaughtUp: true, FastSyncServerTime: time.Now().UnixMilli(),
						FastSyncTimeProfile: "authentic",
					})
					continue
				}
				responses = append(responses, &farmv1.StreamExecuteResponse{
					RequestId: request.GetRequestId(), Response: stub.response(request.GetRequest()),
				})
			}
			completed <- &farmv1.StreamExecuteBatchResponse{Responses: responses}
		}(batch)
	}
}

func (stub *farmStub) response(request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	requestEnvelope := request.GetEnvelope()
	if request == nil || requestEnvelope == nil {
		log.Printf("benchstub invalid Farm request: nil_request=%t uid=%d route_uid=%d", request == nil, request.GetUid(), request.GetRouteUid())
		return &farmv1.ClientCommandResponse{Envelope: &publicv3.WireEnvelope{Err: 1001}}
	}
	responseEnvelope := &publicv3.WireEnvelope{
		Cmd:       requestEnvelope.GetCmd(),
		ClientSeq: requestEnvelope.GetClientSeq(),
	}
	switch requestEnvelope.GetCmd() {
	case 200:
		responseEnvelope.Payload = &publicv3.WireEnvelope_EnterFarmResponse{
			EnterFarmResponse: &publicv3.EnterFarmResponse{Snapshot: stub.snapshot, FarmSeq: 1, ServerTime: time.Now().UnixMilli(), TimeProfile: "authentic"},
		}
		return &farmv1.ClientCommandResponse{
			Envelope: responseEnvelope, RoomAction: farmv1.RoomAction_ROOM_ACTION_SUBSCRIBE,
			RoomUid: request.GetRouteUid(), RoomSeq: 1,
		}
	case 204:
		responseEnvelope.Payload = &publicv3.WireEnvelope_SyncFarmResponse{
			SyncFarmResponse: &publicv3.SyncFarmResponse{Snapshot: stub.snapshot, FarmSeq: 1, ServerTime: time.Now().UnixMilli(), TimeProfile: "authentic"},
		}
	default:
		responseEnvelope.Payload = &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}}
	}
	return &farmv1.ClientCommandResponse{Envelope: responseEnvelope}
}

type socialStub struct {
	farmv1.UnimplementedSocialServiceServer
	delay time.Duration
}

func (stub *socialStub) ExecuteClientCommand(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	if stub.delay > 0 {
		time.Sleep(stub.delay)
	}
	return stub.response(request), nil
}

func (stub *socialStub) ExecuteBatchStream(stream farmv1.SocialService_ExecuteBatchStreamServer) error {
	completed := make(chan *farmv1.StreamExecuteBatchResponse, 256)
	sendErr := make(chan error, 1)
	go func() {
		for response := range completed {
			if err := stream.Send(response); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()
	var workers sync.WaitGroup
	slots := make(chan struct{}, 128)
	for {
		batch, err := stream.Recv()
		if err != nil {
			workers.Wait()
			close(completed)
			if sendFailure := <-sendErr; sendFailure != nil {
				return sendFailure
			}
			return err
		}
		slots <- struct{}{}
		workers.Add(1)
		go func(batch *farmv1.StreamExecuteBatchRequest) {
			defer workers.Done()
			defer func() { <-slots }()
			if stub.delay > 0 {
				time.Sleep(stub.delay)
			}
			responses := make([]*farmv1.StreamExecuteResponse, 0, len(batch.GetRequests()))
			for _, request := range batch.GetRequests() {
				responses = append(responses, &farmv1.StreamExecuteResponse{
					RequestId: request.GetRequestId(), Response: stub.response(request.GetRequest()),
				})
			}
			completed <- &farmv1.StreamExecuteBatchResponse{Responses: responses}
		}(batch)
	}
}

func (stub *socialStub) response(request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	envelope := request.GetEnvelope()
	return &farmv1.ClientCommandResponse{Envelope: &publicv3.WireEnvelope{
		Cmd:       envelope.GetCmd(),
		ClientSeq: envelope.GetClientSeq(),
		Payload:   &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}},
	}}
}

func (stub *socialStub) AreFriends(context.Context, *farmv1.AreFriendsRequest) (*farmv1.AreFriendsResponse, error) {
	if stub.delay > 0 {
		time.Sleep(stub.delay)
	}
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
	token := flag.String("token", os.Getenv("FARM_INTERNAL_TOKEN"), "internal bearer token")
	enterBytes := flag.Int("enter-bytes", 4800, "approximate EnterFarm JSON payload bytes")
	farmDelay := flag.Duration("farm-delay", 2*time.Millisecond, "representative Farm response delay")
	socialDelay := flag.Duration("social-delay", time.Millisecond, "representative Social response delay")
	flag.Parse()
	if *token == "" {
		panic("internal bearer token is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stub := &farmStub{snapshot: enterSnapshot(*enterBytes), delay: *farmDelay}
	social := &socialStub{delay: *socialDelay}
	servers := []struct {
		addr     string
		register func(*grpc.Server)
	}{
		{addr: *farmAddr, register: func(server *grpc.Server) { farmv1.RegisterFarmCommandServiceServer(server, stub) }},
		{addr: *socialAddr, register: func(server *grpc.Server) { farmv1.RegisterSocialServiceServer(server, social) }},
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
