package grpcx

import (
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// ServerOptions returns production server options with bearer auth and message limits.
func ServerOptions(token []byte) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(defaultMaxRecvMsgSize),
		grpc.MaxSendMsgSize(defaultMaxSendMsgSize),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(BearerTokenUnaryInterceptor(token)),
		grpc.ChainStreamInterceptor(BearerTokenStreamInterceptor(token)),
	}
}

// ListenAndServe binds addr and serves until stop is closed or Serve returns an error.
func ListenAndServe(addr string, server *grpc.Server, stop <-chan struct{}) error {
	if server == nil {
		return fmt.Errorf("grpcx: server is nil")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpcx: listen %s: %w", addr, err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		return fmt.Errorf("grpcx: serve %s: %w", addr, err)
	case <-stop:
		server.GracefulStop()
		err := <-serveErr
		if err != nil {
			return fmt.Errorf("grpcx: serve %s after stop: %w", addr, err)
		}
		return nil
	}
}
