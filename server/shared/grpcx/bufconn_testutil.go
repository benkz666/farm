package grpcx

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const testBufSize = 1 << 20

// BufconnPair wires a client pool to an in-memory gRPC server for tests.
type BufconnPair struct {
	Listener *bufconn.Listener
	Server   *grpc.Server
	Pool     *Pool
}

// NewBufconnPair dials in-memory connections through pool.Conn.
func NewBufconnPair(t *testing.T, token string, register func(*grpc.Server)) *BufconnPair {
	t.Helper()
	listener := bufconn.Listen(testBufSize)
	server := grpc.NewServer(ServerOptions([]byte(token))...)
	if register != nil {
		register(server)
	}
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Logf("bufconn serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	pool := NewPool(token)
	pool.mu.Lock()
	pool.conns["bufconn"] = dialBufconn(t, listener, token)
	pool.mu.Unlock()
	return &BufconnPair{Listener: listener, Server: server, Pool: pool}
}

func dialBufconn(t *testing.T, listener *bufconn.Listener, token string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.DialContext(
		context.Background(),
		"bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(
			ctx context.Context,
			method string,
			req, reply any,
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			return invoker(WithBearerToken(ctx, token), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(
			ctx context.Context,
			desc *grpc.StreamDesc,
			cc *grpc.ClientConn,
			method string,
			streamer grpc.Streamer,
			opts ...grpc.CallOption,
		) (grpc.ClientStream, error) {
			return streamer(WithBearerToken(ctx, token), desc, cc, method, opts...)
		}),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
