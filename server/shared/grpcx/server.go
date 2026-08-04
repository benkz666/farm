package grpcx

import (
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
