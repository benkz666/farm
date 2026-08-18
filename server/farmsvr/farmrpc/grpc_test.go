package farmrpc

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"
	"farm/server/shared/store"

	"google.golang.org/grpc"
)

type sendFailingBatchStream struct {
	grpc.ServerStream
	context    context.Context
	sendFailed chan struct{}
	sendOnce   sync.Once
	receives   atomic.Uint32
}

func (stream *sendFailingBatchStream) Context() context.Context { return stream.context }

func (stream *sendFailingBatchStream) Send(*farmv1.StreamExecuteBatchResponse) error {
	stream.sendOnce.Do(func() { close(stream.sendFailed) })
	return errors.New("forced send failure")
}

func (stream *sendFailingBatchStream) Recv() (*farmv1.StreamExecuteBatchRequest, error) {
	call := stream.receives.Add(1)
	if call == 1 {
		return batchRequests(1, 1), nil
	}
	<-stream.sendFailed
	return batchRequests(call*100, streamBatchMax), nil
}

func batchRequests(firstID uint32, count int) *farmv1.StreamExecuteBatchRequest {
	requests := make([]*farmv1.StreamExecuteRequest, 0, count)
	for index := 0; index < count; index++ {
		requestID := uint64(firstID) + uint64(index)
		requests = append(requests, &farmv1.StreamExecuteRequest{
			RequestId: requestID,
			Request:   typedRequest(64, 204, uint32(requestID)),
		})
	}
	return &farmv1.StreamExecuteBatchRequest{Requests: requests}
}

type commandExecutorFunc func(context.Context, *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse

func (execute commandExecutorFunc) ExecuteClient(ctx context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
	return execute(ctx, request)
}

type taskAdvancingExecutor struct {
	commandExecutorFunc
	uid    uint64
	taskID uint32
	amount uint32
}

type preparedSyncExecutorStub struct {
	commandExecutorFunc
	uid        uint64
	originator presence.ConnRef
	fromSeq    uint64
}

func (executor *preparedSyncExecutorStub) ExecutePreparedSelfSync(
	_ context.Context,
	uid uint64,
	originator presence.ConnRef,
	fromSeq uint64,
) CommandResponse {
	executor.uid, executor.originator, executor.fromSeq = uid, originator, fromSeq
	return CommandResponse{
		Err: errcode.OK, FarmSeq: fromSeq,
		PreparedField: clientwire.PreparedSyncFarmResponse,
		SyncCaughtUp:  true, SyncServerTime: 123, SyncTimeProfile: "demo",
	}
}

func (executor *taskAdvancingExecutor) AdvanceTask(_ context.Context, uid uint64, taskID, amount uint32) errcode.Code {
	executor.uid, executor.taskID, executor.amount = uid, taskID, amount
	return errcode.OK
}

func typedRequest(uid uint64, command, sequence uint32) *farmv1.ClientCommandRequest {
	return &farmv1.ClientCommandRequest{
		Uid: uid, RouteUid: uid,
		Envelope: &publicv3.WireEnvelope{
			Cmd: command, ClientSeq: sequence,
			Payload: &publicv3.WireEnvelope_CommandRequest{CommandRequest: &publicv3.CommandRequest{}},
		},
	}
}

func typedResponse(request *farmv1.ClientCommandRequest, code errcode.Code) *farmv1.ClientCommandResponse {
	return &farmv1.ClientCommandResponse{Envelope: errorEnvelope(request.Envelope.Cmd, request.Envelope.ClientSeq, code)}
}

func TestCommandServerRejectsUnownedRoute(t *testing.T) {
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		RegisterCommandService(server, commandExecutorFunc(func(_ context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
			return typedResponse(request, errcode.OK)
		}), func(uint64) bool { return false })
	})
	conn, err := pair.Pool.Conn(t.Context(), "bufconn")
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	response, err := farmv1.NewFarmCommandServiceClient(conn).Execute(t.Context(), typedRequest(42, 212, 1))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if response.Envelope.GetErr() != int32(errcode.BadRequest) {
		t.Fatalf("err=%d, want %d", response.Envelope.GetErr(), errcode.BadRequest)
	}
}

func TestCommandServerRoutesTypedTaskAdvancement(t *testing.T) {
	executor := &taskAdvancingExecutor{commandExecutorFunc: func(_ context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
		return typedResponse(request, errcode.OK)
	}}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		RegisterCommandService(server, executor, func(uid uint64) bool { return uid == 42 })
	})
	conn, err := pair.Pool.Conn(t.Context(), "bufconn")
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	response, err := farmv1.NewFarmCommandServiceClient(conn).AdvanceTask(t.Context(), &farmv1.AdvanceTaskRequest{
		Uid: 42, TaskId: 3, Amount: 1,
	})
	if err != nil || response.GetErr() != int32(errcode.OK) || executor.uid != 42 || executor.taskID != 3 || executor.amount != 1 {
		t.Fatalf("response=%v task=(%d,%d,%d) err=%v", response, executor.uid, executor.taskID, executor.amount, err)
	}
}

func TestCommandServerExecutesFlatPreparedSelfSync(t *testing.T) {
	executor := &preparedSyncExecutorStub{commandExecutorFunc: func(
		_ context.Context,
		request *farmv1.ClientCommandRequest,
	) *farmv1.ClientCommandResponse {
		return typedResponse(request, errcode.OK)
	}}
	server := NewCommandServer(executor, func(uid uint64) bool { return uid == 42 })
	response := server.executeBatchRequest(t.Context(), &farmv1.StreamExecuteRequest{
		RequestId: 99, FastSyncUid: 42, FastSyncClientSeq: 7,
		FastSyncFromSeq: 11, FastSyncConnId: 8, FastSyncGatewayId: "gateway-1",
	})
	if response.Response != nil || response.FastSyncClientSeq != 7 ||
		response.FastSyncUid != 42 || response.FastSyncFarmSeq != 11 ||
		!response.FastSyncCaughtUp || response.FastSyncServerTime != 123 ||
		response.FastSyncTimeProfile != "demo" {
		t.Fatalf("flat response=%#v", response)
	}
	if executor.uid != 42 || executor.fromSeq != 11 || executor.originator.ConnID != 8 ||
		executor.originator.GatewayID != "gateway-1" {
		t.Fatalf("execution=(%d,%d,%#v)", executor.uid, executor.fromSeq, executor.originator)
	}
}

func TestBatchStreamReturnsFastFarmBeforeBlockedFarm(t *testing.T) {
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	executor := commandExecutorFunc(func(_ context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
		if request.RouteUid == 1 {
			close(slowStarted)
			<-releaseSlow
		}
		return typedResponse(request, errcode.OK)
	})
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		RegisterCommandService(server, executor, func(uint64) bool { return true })
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
		{RequestId: 1, Request: typedRequest(1, 602, 1)},
		// UID 65 collided with UID 1 in the old routeUID%64 worker. A blocking
		// claim must no longer hold this unrelated normal command behind it.
		{RequestId: 2, Request: typedRequest(65, 204, 2)},
	}}); err != nil {
		t.Fatalf("send batch: %v", err)
	}
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow request did not start")
	}
	first, err := stream.Recv()
	if err != nil || len(first.Responses) != 1 || first.Responses[0].RequestId != 2 {
		t.Fatalf("first response=%v err=%v", first, err)
	}
	close(releaseSlow)
	second, err := stream.Recv()
	if err != nil || len(second.Responses) != 1 || second.Responses[0].RequestId != 1 {
		t.Fatalf("second response=%v err=%v", second, err)
	}
	_ = stream.CloseSend()
}

func TestBatchStreamPreservesOrderWithinUIDAcrossLanes(t *testing.T) {
	claimStarted := make(chan struct{})
	releaseClaim := make(chan struct{})
	normalStarted := make(chan struct{})
	executor := commandExecutorFunc(func(_ context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
		switch request.Envelope.ClientSeq {
		case 1:
			close(claimStarted)
			<-releaseClaim
		case 2:
			close(normalStarted)
		}
		return typedResponse(request, errcode.OK)
	})
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		RegisterCommandService(server, executor, func(uint64) bool { return true })
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
		{RequestId: 1, Request: typedRequest(42, 602, 1)},
		{RequestId: 2, Request: typedRequest(42, 204, 2)},
	}}); err != nil {
		t.Fatalf("send batch: %v", err)
	}
	select {
	case <-claimStarted:
	case <-time.After(time.Second):
		t.Fatal("claim did not start")
	}
	select {
	case <-normalStarted:
		t.Fatal("same-UID normal command overtook the blocking claim")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseClaim)
	select {
	case <-normalStarted:
	case <-time.After(time.Second):
		t.Fatal("same-UID normal command did not resume after claim")
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	responses := 0
	for responses < 2 {
		batch, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv responses: %v", err)
		}
		responses += len(batch.Responses)
	}
}

func TestBatchStreamReturnsAfterSenderFailure(t *testing.T) {
	release := make(chan struct{})
	var executions atomic.Uint32
	executor := commandExecutorFunc(func(_ context.Context, request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
		if executions.Add(1) > 1 {
			<-release
		}
		return typedResponse(request, errcode.OK)
	})
	stream := &sendFailingBatchStream{
		context: context.Background(), sendFailed: make(chan struct{}),
	}
	completed := make(chan error, 1)
	go func() {
		completed <- NewCommandServer(executor, func(uint64) bool { return true }).ExecuteBatchStream(stream)
	}()

	select {
	case <-stream.sendFailed:
	case <-time.After(time.Second):
		t.Fatal("batch sender did not fail")
	}
	// Let the receive loop reach the closed-done branch while the routed worker
	// queue is full, then release the workers so shutdown can complete.
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case err := <-completed:
		if err == nil || err.Error() != "forced send failure" {
			t.Fatalf("ExecuteBatchStream error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecuteBatchStream blocked after sender failure")
	}
}

type stubPushServer struct {
	farmv1.UnimplementedGatewayPushServiceServer
	lastBatch *farmv1.PushFarmDeltaBatchRequest
	lastPush  *farmv1.DeliverPushRequest
}

func (stub *stubPushServer) PushFarmDeltaBatch(_ context.Context, request *farmv1.PushFarmDeltaBatchRequest) (*farmv1.Empty, error) {
	stub.lastBatch = request
	return &farmv1.Empty{}, nil
}

func (stub *stubPushServer) DeliverPush(_ context.Context, request *farmv1.DeliverPushRequest) (*farmv1.Empty, error) {
	stub.lastPush = request
	return &farmv1.Empty{}, nil
}

func TestGRPCDeltaPusherDeliversTypedBatch(t *testing.T) {
	stub := &stubPushServer{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterGatewayPushServiceServer(server, stub)
	})
	pusher := NewGRPCDeltaPusher(NewGatewayPushClient(pair.Pool, map[string]string{"gateway-0": "bufconn"}))
	if err := pusher.PushBatch(t.Context(), "gateway-0", PushBatch{
		ConnIDs: []uint64{7, 8}, Delta: clientwire.FarmDeltaToProto(farm.FarmDelta{OwnerUID: 42, FarmSeq: 1}),
	}); err != nil {
		t.Fatalf("PushBatch: %v", err)
	}
	if stub.lastBatch == nil || len(stub.lastBatch.ConnIds) != 2 || stub.lastBatch.Delta.GetFarmSeq() != 1 {
		t.Fatalf("batch=%v", stub.lastBatch)
	}
}

func TestGRPCTaskNotifyPusherBuildsPublicEnvelope(t *testing.T) {
	stub := &stubPushServer{}
	pair := grpcx.NewBufconnPair(t, "internal-token", func(server *grpc.Server) {
		farmv1.RegisterGatewayPushServiceServer(server, stub)
	})
	client := NewGatewayPushClient(pair.Pool, map[string]string{"gateway-0": "bufconn"})
	pusher := NewGRPCTaskNotifyPusher(client)
	if err := pusher.PushTaskNotify(t.Context(), presence.ConnRef{GatewayID: "gateway-0", ConnID: 7}, 42, store.Task{
		ID: 1, Progress: 2, Target: 3,
	}); err != nil {
		t.Fatalf("PushTaskNotify: %v", err)
	}
	if stub.lastPush == nil || stub.lastPush.ConnectionId != 7 || stub.lastPush.Uid != 42 ||
		stub.lastPush.Envelope.GetCmd() != clientwire.CommandTaskNotify ||
		stub.lastPush.Envelope.GetTaskNotify().GetProgress() != 2 {
		t.Fatalf("push=%v", stub.lastPush)
	}
}
