package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"
	"farm/server/shared/rpcerr"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SocialClient is Gateway's transport adapter for public social commands.
// It depends only on the generated Protobuf contract, never Social's server
// implementation or storage package.
type SocialClient struct {
	pool   *grpcx.Pool
	target string
	mu     sync.Mutex
	stream *commandStream
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
	stream, err := client.streamFor(conn)
	if err != nil {
		return client.executeUnary(ctx, conn, request)
	}
	response, err := stream.execute(ctx, request)
	if err == nil {
		return response, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	client.discardStream(stream)
	if status.Code(err) == codes.Unimplemented {
		return client.executeUnary(ctx, conn, request)
	}
	if isReplaySafeSocialCommand(request.Envelope.Cmd) && ctx.Err() == nil {
		replacement, createErr := client.streamFor(conn)
		if createErr != nil {
			return nil, fmt.Errorf("gateway: recreate Social stream after %v: %w", err, createErr)
		}
		response, retryErr := replacement.execute(ctx, request)
		if retryErr == nil {
			return response, nil
		}
		if !errors.Is(retryErr, context.Canceled) && !errors.Is(retryErr, context.DeadlineExceeded) {
			client.discardStream(replacement)
		}
		return nil, fmt.Errorf("gateway: execute Social command after stream recovery: %w", retryErr)
	}
	return nil, fmt.Errorf("gateway: execute Social command: %w", err)
}

func (client *SocialClient) streamFor(conn grpc.ClientConnInterface) (*commandStream, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.stream != nil && !client.stream.failed() {
		return client.stream, nil
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	stream, err := farmv1.NewSocialServiceClient(conn).ExecuteBatchStream(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	client.stream = newCommandStream(stream, cancel)
	return client.stream, nil
}

func (client *SocialClient) discardStream(stream *commandStream) {
	client.mu.Lock()
	if client.stream == stream {
		client.stream = nil
	}
	client.mu.Unlock()
	stream.stop(fmt.Errorf("gateway: Social stream discarded"))
}

func (client *SocialClient) executeUnary(
	ctx context.Context,
	conn grpc.ClientConnInterface,
	request *farmv1.ClientCommandRequest,
) (*farmv1.ClientCommandResponse, error) {
	response, err := farmv1.NewSocialServiceClient(conn).ExecuteClientCommand(ctx, request)
	if err != nil {
		return nil, rpcerr.FromGRPC(err)
	}
	return response, nil
}

func isReplaySafeSocialCommand(command uint32) bool {
	switch command {
	case 400, 410, 414:
		return true
	default:
		return false
	}
}

var _ SocialCommandClient = (*SocialClient)(nil)
