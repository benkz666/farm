package farmrpc

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/farmsvr/room"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

type delayedBatchCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	streams atomic.Int64
}

type selectiveBlockingRuntime struct {
	slowUID     uint64
	slowStarted chan struct{}
	releaseSlow chan struct{}
}

func (runtime *selectiveBlockingRuntime) Do(uid uint64, fn func(*room.FarmActor) error) error {
	if uid == runtime.slowUID {
		close(runtime.slowStarted)
		<-runtime.releaseSlow
	}
	return fn(nil)
}

func (server *delayedBatchCommandServer) ExecuteBatchStream(stream farmv1.FarmCommandService_ExecuteBatchStreamServer) error {
	server.streams.Add(1)
	for {
		batch, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		responses := make([]*farmv1.StreamExecuteResponse, 0, len(batch.Requests))
		for _, request := range batch.Requests {
			if request.Request.FarmUid == 1 {
				time.Sleep(20 * time.Millisecond)
			}
			responses = append(responses, &farmv1.StreamExecuteResponse{RequestId: request.RequestId, Response: &farmv1.ExecuteResponse{Err: int32(errcode.Internal)}})
		}
		if err := stream.Send(&farmv1.StreamExecuteBatchResponse{Responses: responses}); err != nil {
			return err
		}
	}
}

type batchRecordingCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
	maxBatch atomic.Int64
}

func (server *batchRecordingCommandServer) ExecuteBatchStream(stream farmv1.FarmCommandService_ExecuteBatchStreamServer) error {
	for {
		batch, err := stream.Recv()
		if err != nil {
			return err
		}
		for {
			current := server.maxBatch.Load()
			if int64(len(batch.Requests)) <= current || server.maxBatch.CompareAndSwap(current, int64(len(batch.Requests))) {
				break
			}
		}
		responses := make([]*farmv1.StreamExecuteResponse, 0, len(batch.Requests))
		for _, request := range batch.Requests {
			responses = append(responses, &farmv1.StreamExecuteResponse{
				RequestId: request.RequestId,
				Response:  &farmv1.ExecuteResponse{Err: int32(errcode.Internal)},
			})
		}
		if err := stream.Send(&farmv1.StreamExecuteBatchResponse{Responses: responses}); err != nil {
			return err
		}
	}
}

type stubPushServer struct {
	farmv1.UnimplementedGatewayPushServiceServer
	lastBatch *farmv1.PushFarmDeltaBatchRequest
}

func (stub *stubPushServer) PushFarmDeltaBatch(_ context.Context, request *farmv1.PushFarmDeltaBatchRequest) (*farmv1.Empty, error) {
	stub.lastBatch = request
	return &farmv1.Empty{}, nil
}

func newCommandTestClient(t *testing.T, handler *Handler, owns func(uint64) bool) *GRPCClient {
	t.Helper()
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		RegisterCommandService(server, handler, owns)
	})
	return NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"})
}

func TestGRPCClientExecute(t *testing.T) {
	handler := NewHandler(
		runtimeStub{actor: nil},
		[]byte("internal-token"),
		func(uint64) bool { return true },
		func() int64 { return 123 },
	)
	client := newCommandTestClient(t, handler, func(uint64) bool { return true })
	response, err := client.Execute(context.Background(), "farm-0", CommandRequest{
		Operation: OperationEnterFarm,
		FarmUID:   42,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Err != errcode.Internal {
		t.Fatalf("response.Err = %d, want %d", response.Err, errcode.Internal)
	}
}

func TestGRPCClientExecuteStreamMultiplexesConcurrentCommands(t *testing.T) {
	handler := NewHandler(
		runtimeStub{actor: nil},
		[]byte("internal-token"),
		func(uint64) bool { return true },
		func() int64 { return 123 },
	)
	client := newCommandTestClient(t, handler, func(uint64) bool { return true })

	const requests = 256
	errors := make(chan error, requests)
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := client.Execute(context.Background(), "farm-0", CommandRequest{
				Operation: OperationEnterFarm,
				FarmUID:   uint64(index%32 + 1),
			})
			if err != nil {
				errors <- err
				return
			}
			if response.Err != errcode.Internal {
				errors <- &unexpectedCodeError{got: response.Err, want: errcode.Internal}
			}
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent Execute: %v", err)
	}
}

func TestGRPCClientCoalescesConcurrentCommandsIntoBatches(t *testing.T) {
	serverStub := &batchRecordingCommandServer{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterFarmCommandServiceServer(server, serverStub)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"})
	const requests = 128
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, _ = client.Execute(t.Context(), "farm-0", CommandRequest{
				Operation: OperationEnterFarm,
				FarmUID:   uint64(index + 1),
			})
		}(index)
	}
	close(start)
	wait.Wait()
	if serverStub.maxBatch.Load() <= 1 {
		t.Fatalf("maximum batch=%d, want >1", serverStub.maxBatch.Load())
	}
}

func TestExecuteBatchStreamDoesNotHoldFastResponseBehindSlowPeer(t *testing.T) {
	runtime := &selectiveBlockingRuntime{
		slowUID:     1,
		slowStarted: make(chan struct{}),
		releaseSlow: make(chan struct{}),
	}
	handler := NewHandler(runtime, []byte("internal-token"), func(uint64) bool { return true }, nil)
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		RegisterCommandService(server, handler, func(uint64) bool { return true })
	})
	conn, err := pair.Pool.Conn(t.Context(), "bufconn")
	if err != nil {
		t.Fatalf("pool conn: %v", err)
	}
	stream, err := farmv1.NewFarmCommandServiceClient(conn).ExecuteBatchStream(t.Context())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(&farmv1.StreamExecuteBatchRequest{Requests: []*farmv1.StreamExecuteRequest{
		{RequestId: 1, Request: &farmv1.ExecuteRequest{Operation: farmv1.Operation_OPERATION_ENTER_FARM, FarmUid: 1}},
		{RequestId: 2, Request: &farmv1.ExecuteRequest{Operation: farmv1.Operation_OPERATION_ENTER_FARM, FarmUid: 2}},
	}}); err != nil {
		t.Fatalf("send batch: %v", err)
	}
	select {
	case <-runtime.slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow request did not start")
	}

	firstResult := make(chan *farmv1.StreamExecuteBatchResponse, 1)
	firstErr := make(chan error, 1)
	go func() {
		response, receiveErr := stream.Recv()
		if receiveErr != nil {
			firstErr <- receiveErr
			return
		}
		firstResult <- response
	}()
	select {
	case response := <-firstResult:
		if len(response.Responses) != 1 || response.Responses[0].RequestId != 2 {
			t.Fatalf("first response = %#v, want only fast request 2", response.Responses)
		}
	case err := <-firstErr:
		t.Fatalf("receive fast response: %v", err)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("fast response waited for blocked peer")
	}

	close(runtime.releaseSlow)
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive slow response: %v", err)
	}
	if len(second.Responses) != 1 || second.Responses[0].RequestId != 1 {
		t.Fatalf("second response = %#v, want slow request 1", second.Responses)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
}

func TestGRPCClientCallerTimeoutDoesNotTearDownSharedStream(t *testing.T) {
	serverStub := &delayedBatchCommandServer{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterFarmCommandServiceServer(server, serverStub)
	})
	client := NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"})
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := client.Execute(ctx, "farm-0", CommandRequest{Operation: OperationEnterFarm, FarmUID: 1}); err == nil {
		t.Fatal("timed request unexpectedly succeeded")
	}
	time.Sleep(25 * time.Millisecond) // let the abandoned response reclaim its slot
	if _, err := client.Execute(t.Context(), "farm-0", CommandRequest{Operation: OperationEnterFarm, FarmUID: 2}); err != nil {
		t.Fatalf("healthy request after caller timeout: %v", err)
	}
	if got := serverStub.streams.Load(); got != 1 {
		t.Fatalf("stream count = %d, want 1", got)
	}
}

type unexpectedCodeError struct {
	got  errcode.Code
	want errcode.Code
}

func (err *unexpectedCodeError) Error() string {
	return "unexpected response code"
}

func TestGRPCDeltaPusherDeliverBatch(t *testing.T) {
	stub := &stubPushServer{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterGatewayPushServiceServer(server, stub)
	})
	pusher := NewGRPCDeltaPusher(NewGatewayPushClient(pair.Pool, map[string]string{"gateway-0": "bufconn"}))
	envelope := []byte(`{"cmd":9000,"client_seq":0,"err":0,"payload":{"owner_uid":"42","farm_seq":"1","plots":[]}}`)
	if err := pusher.PushBatch(context.Background(), "gateway-0", PushBatch{
		ConnIDs:  []uint64{7, 8},
		Envelope: envelope,
	}); err != nil {
		t.Fatalf("PushBatch: %v", err)
	}
	if stub.lastBatch == nil || len(stub.lastBatch.ConnIds) != 2 {
		t.Fatalf("batch = %#v", stub.lastBatch)
	}
}

func TestCommandServerRejectsUnownedFarm(t *testing.T) {
	handler := NewHandler(runtimeStub{}, []byte("internal-token"), func(uint64) bool { return false }, nil)
	client := newCommandTestClient(t, handler, func(uint64) bool { return false })
	response, err := client.Execute(context.Background(), "farm-0", CommandRequest{
		Operation: OperationEnterFarm,
		FarmUID:   42,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Err != errcode.BadRequest {
		t.Fatalf("err = %d, want %d", response.Err, errcode.BadRequest)
	}
}
