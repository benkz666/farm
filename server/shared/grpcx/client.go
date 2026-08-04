package grpcx

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

const (
	defaultMaxRecvMsgSize = 16 << 20
	defaultMaxSendMsgSize = 16 << 20
	defaultCallTimeout    = 5 * time.Second
)

// Pool reuses one *grpc.ClientConn per target address.
type Pool struct {
	token string
	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// NewPool constructs a connection pool that injects the internal bearer token.
func NewPool(token string) *Pool {
	return &Pool{
		token: strings.TrimSpace(token),
		conns: make(map[string]*grpc.ClientConn),
	}
}

// Conn returns a shared client connection for target (host:port).
func (pool *Pool) Conn(ctx context.Context, target string) (*grpc.ClientConn, error) {
	if pool == nil {
		return nil, fmt.Errorf("grpcx: pool is nil")
	}
	target = normalizeTarget(target)
	if target == "" {
		return nil, fmt.Errorf("grpcx: target must not be empty")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if conn, ok := pool.conns[target]; ok {
		return conn, nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(
		dialCtx,
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(defaultMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(defaultMaxSendMsgSize),
		),
		grpc.WithUnaryInterceptor(func(
			ctx context.Context,
			method string,
			req, reply any,
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				return invoker(WithBearerToken(ctx, pool.token), method, req, reply, cc, opts...)
			}
			callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
			defer cancel()
			return invoker(WithBearerToken(callCtx, pool.token), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(
			ctx context.Context,
			desc *grpc.StreamDesc,
			cc *grpc.ClientConn,
			method string,
			streamer grpc.Streamer,
			opts ...grpc.CallOption,
		) (grpc.ClientStream, error) {
			return streamer(WithBearerToken(ctx, pool.token), desc, cc, method, opts...)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("grpcx: dial %s: %w", target, err)
	}
	pool.conns[target] = conn
	return conn, nil
}

// Close closes every pooled connection.
func (pool *Pool) Close() error {
	if pool == nil {
		return nil
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	var first error
	for target, conn := range pool.conns {
		if err := conn.Close(); err != nil && first == nil {
			first = fmt.Errorf("grpcx: close %s: %w", target, err)
		}
		delete(pool.conns, target)
	}
	return first
}

// RegisterTestConn pins a connection for tests that host multiple in-memory targets.
func (pool *Pool) RegisterTestConn(target string, conn *grpc.ClientConn) {
	if pool == nil {
		return
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.conns[normalizeTarget(target)] = conn
}

func (pool *Pool) Ready(ctx context.Context, targets ...string) error {
	if pool == nil {
		return fmt.Errorf("grpcx: pool is nil")
	}
	for _, target := range targets {
		target = normalizeTarget(target)
		if target == "" {
			return fmt.Errorf("grpcx: empty target")
		}
		conn, err := pool.Conn(ctx, target)
		if err != nil {
			return err
		}
		conn.Connect()
		for {
			state := conn.GetState()
			switch state {
			case connectivity.Ready:
				goto nextTarget
			case connectivity.Shutdown:
				return fmt.Errorf("grpcx: connection to %s is shutdown", target)
			}
			if !conn.WaitForStateChange(ctx, state) {
				return fmt.Errorf("grpcx: connection to %s not ready: %w", target, ctx.Err())
			}
		}
	nextTarget:
	}
	return nil
}

func normalizeTarget(target string) string {
	return strings.TrimSpace(target)
}
