package farmrpc

import (
	"context"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/gameconfig"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DebugServer implements dev-only time controls for Farm peers.
type DebugServer struct {
	farmv1.UnimplementedDebugServiceServer
	advance      func(int64)
	now          func() int64
	timeProfiles *gameconfig.TimeProfileSwitch
}

// NewDebugServer constructs the Farm debug gRPC adapter.
func NewDebugServer(advance func(int64), now func() int64, profiles *gameconfig.TimeProfileSwitch) *DebugServer {
	return &DebugServer{advance: advance, now: now, timeProfiles: profiles}
}

// Advance moves the local farm clock forward.
func (server *DebugServer) Advance(_ context.Context, request *farmv1.AdvanceRequest) (*farmv1.AdvanceResponse, error) {
	if server == nil || server.advance == nil || server.now == nil || request == nil || request.Ms <= 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	server.advance(request.Ms)
	return &farmv1.AdvanceResponse{ServerTime: server.now()}, nil
}

// SetTimeProfile hot-switches the local farm time profile.
func (server *DebugServer) SetTimeProfile(_ context.Context, request *farmv1.SetTimeProfileRequest) (*farmv1.SetTimeProfileResponse, error) {
	if server == nil || server.timeProfiles == nil || request == nil ||
		!gameconfig.ValidTimeProfile(request.TimeProfile) ||
		!server.timeProfiles.Set(request.TimeProfile) {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	return &farmv1.SetTimeProfileResponse{TimeProfile: server.timeProfiles.Get()}, nil
}
