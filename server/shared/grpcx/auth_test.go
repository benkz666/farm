package grpcx

import (
	"context"
	"net"
	"testing"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestBearerTokenUnaryInterceptor(t *testing.T) {
	pair := NewBufconnPair(t, "secret", func(s *grpc.Server) {
		farmv1.RegisterFarmCommandServiceServer(s, stubCommandServer{})
	})

	t.Run("reject missing token", func(t *testing.T) {
		conn := dialBufconnNoAuth(t, pair.Listener)
		client := farmv1.NewFarmCommandServiceClient(conn)
		_, err := client.Execute(context.Background(), commandRequest(1))
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("status = %v, want Unauthenticated", err)
		}
	})

	t.Run("accept bearer token", func(t *testing.T) {
		conn, err := pair.Pool.Conn(context.Background(), "bufconn")
		if err != nil {
			t.Fatalf("pool conn: %v", err)
		}
		client := farmv1.NewFarmCommandServiceClient(conn)
		resp, err := client.Execute(context.Background(), commandRequest(1))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if resp.GetEnvelope().GetErr() != 0 {
			t.Fatalf("err = %d", resp.GetEnvelope().GetErr())
		}
	})
}

type stubCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
}

func (stubCommandServer) Execute(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	return &farmv1.ClientCommandResponse{Envelope: &publicv3.WireEnvelope{
		Cmd:       request.GetEnvelope().GetCmd(),
		ClientSeq: request.GetEnvelope().GetClientSeq(),
	}}, nil
}

func commandRequest(uid uint64) *farmv1.ClientCommandRequest {
	return &farmv1.ClientCommandRequest{
		Uid:      uid,
		RouteUid: uid,
		Envelope: &publicv3.WireEnvelope{
			Cmd:       200,
			ClientSeq: 1,
			Payload: &publicv3.WireEnvelope_EnterFarmRequest{
				EnterFarmRequest: &publicv3.EnterFarmRequest{OwnerUid: uid},
			},
		},
	}
}

func dialBufconnNoAuth(t *testing.T, listener *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.DialContext(
		context.Background(),
		"bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
