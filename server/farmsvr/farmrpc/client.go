package farmrpc

import "context"

// Client sends a routed command to the Farm that owns it.
type Client interface {
	Execute(ctx context.Context, farmID string, request CommandRequest) (CommandResponse, error)
}
