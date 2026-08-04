package farmrpc

import (
	"context"
	"testing"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

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
