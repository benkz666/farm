package gateway

import (
	"testing"

	"farm/server/farmsvr/farmrpc"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

func newTestPushPool(t *testing.T, token string, gateway *Gateway) *grpcx.Pool {
	t.Helper()
	pair := grpcx.NewBufconnPair(t, token, func(server *grpc.Server) {
		RegisterPushService(server, gateway)
	})
	return pair.Pool
}

func newTestSessionKickPusher(t *testing.T, token string, gateway *Gateway, gatewayID string) farmrpc.SessionKickPusher {
	t.Helper()
	pool := newTestPushPool(t, token, gateway)
	return farmrpc.NewGRPCSessionKickPusher(farmrpc.NewGatewayPushClient(pool, map[string]string{gatewayID: "bufconn"}))
}
