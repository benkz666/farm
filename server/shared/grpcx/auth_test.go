package grpcx

import (
	"context"
	"net"
	"testing"

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
		_, err := client.Execute(context.Background(), &farmv1.ExecuteRequest{
			Operation: farmv1.Operation_OPERATION_ENTER_FARM,
			FarmUid:   1,
		})
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
		resp, err := client.Execute(context.Background(), &farmv1.ExecuteRequest{
			Operation: farmv1.Operation_OPERATION_ENTER_FARM,
			FarmUid:   1,
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if resp.GetErr() != 0 {
			t.Fatalf("err = %d", resp.GetErr())
		}
	})
}

type stubCommandServer struct {
	farmv1.UnimplementedFarmCommandServiceServer
}

func (stubCommandServer) Execute(context.Context, *farmv1.ExecuteRequest) (*farmv1.ExecuteResponse, error) {
	return &farmv1.ExecuteResponse{}, nil
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
