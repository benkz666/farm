package gateway

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

type recordingFarmCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	last      chan *farmv1.ClientCommandRequest
	maxBatch  atomic.Int64
	streams   atomic.Int64
	slowRoute uint64
}

func (server *recordingFarmCommandServer) ExecuteBatchStream(stream farmv1.FarmCommandService_ExecuteBatchStreamServer) error {
	server.streams.Add(1)
	for {
		batch, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		for {
			current := server.maxBatch.Load()
			if int64(len(batch.Requests)) <= current || server.maxBatch.CompareAndSwap(current, int64(len(batch.Requests))) {
				break
			}
		}
		responses := make([]*farmv1.StreamExecuteResponse, 0, len(batch.Requests))
		for _, streamRequest := range batch.Requests {
			request := streamRequest.GetRequest()
			if server.last != nil {
				server.last <- request
			}
			if request.GetRouteUid() == server.slowRoute {
				time.Sleep(20 * time.Millisecond)
			}
			responses = append(responses, &farmv1.StreamExecuteResponse{
				RequestId: streamRequest.GetRequestId(),
				Response:  farmClientResponse(request, errcode.OK),
			})
		}
		if err := stream.Send(&farmv1.StreamExecuteBatchResponse{Responses: responses}); err != nil {
			return err
		}
	}
}

func farmClientRequest(uid uint64, command, sequence uint32) *farmv1.ClientCommandRequest {
	return &farmv1.ClientCommandRequest{
		Uid: uid, RouteUid: uid,
		Envelope: &publicv3.WireEnvelope{
			Cmd: command, ClientSeq: sequence,
			Payload: &publicv3.WireEnvelope_CommandRequest{CommandRequest: &publicv3.CommandRequest{}},
		},
	}
}

func farmClientResponse(request *farmv1.ClientCommandRequest, code errcode.Code) *farmv1.ClientCommandResponse {
	return &farmv1.ClientCommandResponse{Envelope: commandResponseFor(request.GetEnvelope(), code)}
}

func newTestFarmClient(t *testing.T, serverStub farmv1.FarmCommandServiceServer) *FarmClient {
	t.Helper()
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterFarmCommandServiceServer(server, serverStub)
	})
	return NewFarmClient(pair.Pool, map[string]string{"farm-0": "bufconn"})
}

func TestFarmClientPreservesTypedProtobufContract(t *testing.T) {
	serverStub := &recordingFarmCommandServer{last: make(chan *farmv1.ClientCommandRequest, 1)}
	client := newTestFarmClient(t, serverStub)
	request := farmClientRequest(42, 212, 9)
	request.Originator = &farmv1.ConnRef{ConnId: 7, GatewayId: "gateway-1"}
	request.Envelope.GetCommandRequest().OwnerUid = 42
	request.Envelope.GetCommandRequest().PlotIndex = 3

	response, err := client.Execute(t.Context(), "farm-0", request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	received := <-serverStub.last
	if response.GetEnvelope().GetErr() != int32(errcode.OK) || received.GetOriginator().GetConnId() != 7 ||
		received.GetEnvelope().GetCommandRequest().GetPlotIndex() != 3 {
		t.Fatalf("response=%v received=%v", response, received)
	}
}

func TestFarmClientCoalescesConcurrentCommands(t *testing.T) {
	serverStub := &recordingFarmCommandServer{}
	client := newTestFarmClient(t, serverStub)
	const requests = 128
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, _ = client.Execute(t.Context(), "farm-0", farmClientRequest(uint64(index+1), 204, uint32(index+1)))
		}(index)
	}
	close(start)
	wait.Wait()
	if serverStub.maxBatch.Load() <= 1 {
		t.Fatalf("maximum batch=%d, want >1", serverStub.maxBatch.Load())
	}
}

func TestFarmClientTimeoutDoesNotTearDownSharedStream(t *testing.T) {
	serverStub := &recordingFarmCommandServer{slowRoute: 1}
	client := newTestFarmClient(t, serverStub)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := client.Execute(ctx, "farm-0", farmClientRequest(1, 204, 1)); err == nil {
		t.Fatal("timed request unexpectedly succeeded")
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := client.Execute(t.Context(), "farm-0", farmClientRequest(2, 204, 2)); err != nil {
		t.Fatalf("healthy request after timeout: %v", err)
	}
	if serverStub.streams.Load() != 1 {
		t.Fatalf("stream count=%d, want 1", serverStub.streams.Load())
	}
}
