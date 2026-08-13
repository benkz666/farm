package gateway

import (
	"context"
	"errors"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/telemetry"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PushServer implements GatewayPushService by applying pushes to local sockets.
type PushServer struct {
	farmv1.UnimplementedGatewayPushServiceServer
	gateway *Gateway
}

// NewPushServer registers Gateway push handlers on gRPC.
func NewPushServer(gateway *Gateway) *PushServer {
	return &PushServer{gateway: gateway}
}

// PushFarmDeltaBatch delivers one pre-encoded envelope to many local connections.
func (server *PushServer) PushFarmDeltaBatch(_ context.Context, request *farmv1.PushFarmDeltaBatchRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil {
		return &farmv1.Empty{}, nil
	}
	if err := server.gateway.applyFarmDeltaBatchProto(request.ConnIds, request.Delta); err != nil {
		if errors.Is(err, errApplyBadRequest) {
			return nil, status.Error(codes.InvalidArgument, "bad_request")
		}
		return nil, status.Error(codes.Unavailable, "push_failed")
	}
	return &farmv1.Empty{}, nil
}

// DeliverPush enqueues one application-owned public envelope without
// interpreting its payload. This keeps Gateway independent of business
// domains while retaining one typed Protobuf service boundary.
func (server *PushServer) DeliverPush(_ context.Context, request *farmv1.DeliverPushRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil {
		return &farmv1.Empty{}, nil
	}
	if request.ConnectionId == 0 || request.Uid == 0 || request.Envelope == nil {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	if err := server.gateway.applyPush(request.ConnectionId, request.Uid, request.Envelope); err != nil {
		if errors.Is(err, errApplyBadRequest) {
			return nil, status.Error(codes.InvalidArgument, "bad_request")
		}
		return nil, status.Error(codes.Unavailable, "push_failed")
	}
	return &farmv1.Empty{}, nil
}

// PushSessionKick closes one replaced local connection.
func (server *PushServer) PushSessionKick(_ context.Context, request *farmv1.PushSessionKickRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil {
		return &farmv1.Empty{}, nil
	}
	if request.ConnectionId == 0 || request.Uid == 0 || errcode.Code(request.Reason) != errcode.Kicked {
		return &farmv1.Empty{}, nil
	}
	server.gateway.applySessionKick(request.ConnectionId, request.Uid, errcode.Code(request.Reason))
	return &farmv1.Empty{}, nil
}

// RevokeFarmAccess immediately removes a viewer from an owner room after
// Social commits an unfriend. Gateway only applies the transport directive.
func (server *PushServer) RevokeFarmAccess(_ context.Context, request *farmv1.RevokeFarmAccessRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil ||
		request.ConnectionId == 0 || request.ViewerUid == 0 || request.OwnerUid == 0 {
		return &farmv1.Empty{}, nil
	}
	server.gateway.applyFarmAccessRevocation(request.ConnectionId, request.ViewerUid, request.OwnerUid)
	return &farmv1.Empty{}, nil
}

func (g *Gateway) applyFarmDeltaBatchProto(connIDs []uint64, encodedDelta *publicv3.FarmDelta) error {
	if len(connIDs) == 0 || encodedDelta == nil {
		return errApplyBadRequest
	}
	if encodedDelta.OwnerUid == 0 {
		return errApplyBadRequest
	}
	record, err := clientwire.EncodeFarmDeltaProtoRecord(encodedDelta)
	if err != nil {
		return errApplyBadRequest
	}
	seen := make(map[uint64]struct{}, len(connIDs))
	var firstWriteErr error
	for _, connID := range connIDs {
		if connID == 0 {
			continue
		}
		if _, dup := seen[connID]; dup {
			continue
		}
		seen[connID] = struct{}{}
		connection, ok := g.connections.Load(connID)
		if !ok {
			continue
		}
		wsConn := connection.(*wsConnection)
		if !wsConn.subscribedTo(encodedDelta.OwnerUid) {
			continue
		}
		if err := wsConn.pushFarmDelta(encodedDelta.OwnerUid, encodedDelta, record); err != nil {
			if firstWriteErr == nil {
				firstWriteErr = err
			}
			telemetry.L().Warn("gateway batched FarmDelta push failed",
				"component", "gateway",
				"op", "push_farm_delta_batch",
				"owner_uid", encodedDelta.OwnerUid,
				"farm_seq", encodedDelta.FarmSeq,
				"err", err.Error(),
			)
			wsConn.dropSlowConnection()
		}
	}
	return firstWriteErr
}

func (g *Gateway) applyPush(connectionID, uid uint64, envelope *publicv3.WireEnvelope) error {
	if connectionID == 0 || uid == 0 || envelope == nil {
		return errApplyBadRequest
	}
	record, err := clientwire.EncodeWireRecord(envelope)
	if err != nil {
		return errApplyBadRequest
	}
	connection, ok := g.connections.Load(connectionID)
	if !ok {
		return nil
	}
	wsConn := connection.(*wsConnection)
	if !wsConn.authed || wsConn.uid != uid {
		return nil
	}
	if err := wsConn.enqueuePush(record); err != nil {
		telemetry.L().Warn("gateway transport push failed",
			"component", "gateway",
			"op", "deliver_push",
			"uid", uid,
			"cmd", envelope.Cmd,
			"err", err.Error(),
		)
		wsConn.dropSlowConnection()
		return err
	}
	return nil
}

func (g *Gateway) applySessionKick(connectionID, uid uint64, reason errcode.Code) {
	value, ok := g.connections.Load(connectionID)
	if !ok {
		return
	}
	connection, valid := value.(*wsConnection)
	if valid && connection != nil && connection.authed && connection.uid == uid {
		connection.kick(reason)
	}
}

func (g *Gateway) applyFarmAccessRevocation(connectionID, viewerUID, ownerUID uint64) {
	value, ok := g.connections.Load(connectionID)
	if !ok {
		return
	}
	connection, valid := value.(*wsConnection)
	if !valid || connection == nil || !connection.authed || connection.uid != viewerUID || !connection.subscribedTo(ownerUID) {
		return
	}
	g.leaveFarm(connection)
}

var errApplyBadRequest = errors.New("gateway: invalid push payload")
