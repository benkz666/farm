package farmrpc

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
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

func TestGRPCActionReturnsPreparedPublicProtobuf(t *testing.T) {
	handler := NewHandler(
		runtimeStub{actor: &room.FarmActor{Aggregate: farm.NewAggregate(42, "alice")}},
		[]byte("internal-token"),
		func(uid uint64) bool { return uid == 42 },
		func() int64 { return 123 },
	)
	client := newCommandTestClient(t, handler, func(uid uint64) bool { return uid == 42 })
	response, err := client.Execute(t.Context(), "farm-0", CommandRequest{
		Operation:     OperationPlotAction,
		FarmUID:       42,
		ClientCommand: 206,
		ClientRequest: &publicv3.CommandRequest{PlotIndex: 0},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Err != errcode.OK || response.PreparedField != clientwire.PreparedCommandResponse || len(response.PreparedPayload) == 0 {
		t.Fatalf("response=%#v", response)
	}
	if response.ClientResponse != nil || len(response.Payload) != 0 {
		t.Fatalf("prepared response carried duplicate representations: %#v", response)
	}
	frame, err := clientwire.EncodeBinaryBatch([]clientwire.Envelope{{
		Cmd:             206,
		ClientSeq:       9,
		PreparedPayload: response.PreparedPayload,
		PreparedField:   response.PreparedField,
	}})
	if err != nil {
		t.Fatalf("encode public frame: %v", err)
	}
	decoded, err := clientwire.DecodeBinaryBatch(frame)
	if err != nil || len(decoded) != 1 || decoded[0].CommandResponse == nil || decoded[0].CommandResponse.Action == nil {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
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
	if err := pusher.PushBatch(context.Background(), "gateway-0", PushBatch{
		ConnIDs: []uint64{7, 8},
		Delta: clientwire.FarmDeltaToProto(farm.FarmDelta{
			OwnerUID: 42,
			FarmSeq:  1,
		}),
	}); err != nil {
		t.Fatalf("PushBatch: %v", err)
	}
	if stub.lastBatch == nil || len(stub.lastBatch.ConnIds) != 2 {
		t.Fatalf("batch = %#v", stub.lastBatch)
	}
}

func TestGatewayPushClientResolvesAndCachesDynamicTarget(t *testing.T) {
	resolver := &recordingGatewayTargetResolver{target: "dynamic:9202"}
	client := NewResolvingGatewayPushClient(nil, nil, resolver)
	client.pool = &grpcx.Pool{}
	client.now = func() time.Time { return time.Unix(100, 0) }

	first, err := client.target(t.Context(), "gateway-pod")
	if err != nil || first != "dynamic:9202" {
		t.Fatalf("first target = %q, %v", first, err)
	}
	resolver.target = "changed:9202"
	second, err := client.target(t.Context(), "gateway-pod")
	if err != nil || second != "dynamic:9202" {
		t.Fatalf("cached target = %q, %v", second, err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
}

func TestGatewayPushClientPrefersStaticTarget(t *testing.T) {
	resolver := &recordingGatewayTargetResolver{target: "dynamic:9202"}
	client := NewResolvingGatewayPushClient(nil, map[string]string{"gateway-0": "static:9202"}, resolver)
	target, err := client.target(t.Context(), "gateway-0")
	if err != nil || target != "static:9202" {
		t.Fatalf("target = %q, %v", target, err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
}

type recordingGatewayTargetResolver struct {
	target string
	err    error
	calls  int
}

func (resolver *recordingGatewayTargetResolver) ResolveGateway(context.Context, string) (string, error) {
	resolver.calls++
	return resolver.target, resolver.err
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
