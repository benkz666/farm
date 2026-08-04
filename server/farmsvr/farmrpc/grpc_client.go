package farmrpc

import (
	"context"
	"encoding/json"
	"fmt"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
	"farm/server/shared/grpcx"

	"google.golang.org/grpc"
)

// GRPCClient implements Client over FarmCommandService.
type GRPCClient struct {
	pool    *grpcx.Pool
	targets map[string]string
}

// NewGRPCClient constructs a routed Farm command client.
func NewGRPCClient(pool *grpcx.Pool, targets map[string]string) *GRPCClient {
	copied := make(map[string]string, len(targets))
	for farmID, target := range targets {
		copied[farmID] = target
	}
	return &GRPCClient{pool: pool, targets: copied}
}

// Execute forwards one command to the Farm instance that owns FarmUID.
func (client *GRPCClient) Execute(ctx context.Context, farmID string, command CommandRequest) (CommandResponse, error) {
	if client == nil || client.pool == nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: gRPC client is nil")
	}
	target := client.targets[farmID]
	if target == "" {
		return CommandResponse{}, fmt.Errorf("farmrpc: no gRPC target configured for %q", farmID)
	}
	conn, err := client.pool.Conn(ctx, target)
	if err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: dial %q: %w", farmID, err)
	}
	protoOperation, ok := operationToProtoEnum(command.Operation)
	if !ok {
		return CommandResponse{}, fmt.Errorf("farmrpc: unsupported operation %q", command.Operation)
	}
	response, err := farmv1.NewFarmCommandServiceClient(conn).Execute(ctx, &farmv1.ExecuteRequest{
		Operation:   protoOperation,
		FarmUid:     command.FarmUID,
		Originator:  connRefToProto(command.Originator),
		PayloadJson: command.Payload,
	})
	if err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: execute command: %w", err)
	}
	return CommandResponse{
		Err:     errcode.Code(response.Err),
		Payload: json.RawMessage(response.PayloadJson),
	}, nil
}

// RegisterCommandService registers FarmCommandService on a gRPC server.
func RegisterCommandService(server *grpc.Server, handler *Handler, owns func(uint64) bool) {
	farmv1.RegisterFarmCommandServiceServer(server, NewCommandServer(handler, owns))
}
