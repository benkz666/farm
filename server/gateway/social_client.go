package gateway

import (
	"context"
	"errors"
	"strings"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"
	"farm/server/shared/rpcerr"
)

// SocialClient is Gateway's transport adapter for public social commands.
// It depends only on the generated Protobuf contract, never Social's server
// implementation or storage package.
type SocialClient struct {
	pool   *grpcx.Pool
	target string
}

func NewSocialClient(pool *grpcx.Pool, target string) *SocialClient {
	return &SocialClient{pool: pool, target: strings.TrimSpace(target)}
}

func (client *SocialClient) ExecuteClientCommand(
	ctx context.Context,
	request *farmv1.ClientCommandRequest,
) (*farmv1.ClientCommandResponse, error) {
	if client == nil || client.pool == nil || client.target == "" || request == nil || request.Envelope == nil {
		return nil, errors.New("gateway: invalid Social command client request")
	}
	conn, err := client.pool.Conn(ctx, client.target)
	if err != nil {
		return nil, err
	}
	response, err := farmv1.NewSocialServiceClient(conn).ExecuteClientCommand(ctx, request)
	if err != nil {
		return nil, rpcerr.FromGRPC(err)
	}
	return response, nil
}

var _ SocialCommandClient = (*SocialClient)(nil)
