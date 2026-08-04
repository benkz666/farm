package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"farm/server/domain/farm"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/store"
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
	if err := server.gateway.applyFarmDeltaBatch(request.ConnIds, request.Envelope); err != nil {
		if errors.Is(err, errApplyBadRequest) {
			return nil, status.Error(codes.InvalidArgument, "bad_request")
		}
		return nil, status.Error(codes.Unavailable, "push_failed")
	}
	return &farmv1.Empty{}, nil
}

// PushPlayerDelta delivers one personal-state update to a local connection.
func (server *PushServer) PushPlayerDelta(_ context.Context, request *farmv1.PushPlayerDeltaRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil {
		return &farmv1.Empty{}, nil
	}
	var delta farm.PlayerDelta
	if len(request.DeltaJson) > 0 {
		if err := json.Unmarshal(request.DeltaJson, &delta); err != nil {
			return nil, status.Error(codes.InvalidArgument, "bad_request")
		}
	}
	server.gateway.applyPlayerDelta(request.ConnectionId, request.Uid, delta)
	return &farmv1.Empty{}, nil
}

// PushTaskNotify delivers one task progress update to a local connection.
func (server *PushServer) PushTaskNotify(_ context.Context, request *farmv1.PushTaskNotifyRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil {
		return &farmv1.Empty{}, nil
	}
	task := taskFromProto(request.Task)
	if request.ConnectionId == 0 || request.Uid == 0 || !isTaskNotifyID(task.ID) {
		return &farmv1.Empty{}, nil
	}
	server.gateway.applyTaskNotify(request.ConnectionId, request.Uid, task)
	return &farmv1.Empty{}, nil
}

// PushMailNotify delivers one advisory mail refresh hint to a local connection.
func (server *PushServer) PushMailNotify(_ context.Context, request *farmv1.PushMailNotifyRequest) (*farmv1.Empty, error) {
	if server == nil || server.gateway == nil || request == nil {
		return &farmv1.Empty{}, nil
	}
	if request.ConnectionId == 0 || request.Uid == 0 || request.Kind == "" {
		return &farmv1.Empty{}, nil
	}
	server.gateway.applyMailNotify(request.ConnectionId, request.Uid, request.Kind)
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

func (g *Gateway) applyFarmDeltaBatch(connIDs []uint64, envelope []byte) error {
	if len(connIDs) == 0 || len(envelope) == 0 {
		return errApplyBadRequest
	}
	delta, err := clientwire.DecodeFarmDelta(envelope)
	if err != nil || delta.OwnerUID == 0 {
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
		if !wsConn.subscribedTo(delta.OwnerUID) {
			continue
		}
		if err := wsConn.pushFarmDelta(delta.OwnerUID, delta, envelope); err != nil {
			if firstWriteErr == nil {
				firstWriteErr = err
			}
			telemetry.L().Warn("gateway batched FarmDelta push failed",
				"component", "gateway",
				"op", "push_farm_delta_batch",
				"owner_uid", delta.OwnerUID,
				"farm_seq", delta.FarmSeq,
				"err", err.Error(),
			)
			wsConn.dropSlowConnection()
		}
	}
	return firstWriteErr
}

func (g *Gateway) applyPlayerDelta(connectionID, uid uint64, delta farm.PlayerDelta) {
	connection, ok := g.connections.Load(connectionID)
	if !ok {
		return
	}
	wsConn := connection.(*wsConnection)
	if wsConn.uid != uid {
		return
	}
	if err := wsConn.pushPlayerDelta(delta); err != nil {
		telemetry.L().Warn("gateway PlayerDelta push failed",
			"component", "gateway",
			"op", "push_player_delta",
			"uid", uid,
			"err", err.Error(),
		)
		wsConn.dropSlowConnection()
	}
}

func (g *Gateway) applyTaskNotify(connectionID, uid uint64, task store.Task) {
	connection, ok := g.connections.Load(connectionID)
	if !ok {
		return
	}
	wsConnection := connection.(*wsConnection)
	if wsConnection.uid == uid && wsConnection.authed {
		wsConnection.enqueueTaskNotify(task)
	}
}

func (g *Gateway) applyMailNotify(connectionID, uid uint64, kind string) {
	connection, ok := g.connections.Load(connectionID)
	if !ok {
		return
	}
	wsConnection := connection.(*wsConnection)
	if wsConnection.uid == uid && wsConnection.authed {
		wsConnection.enqueueMailNotify(kind)
	}
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

func taskFromProto(task *farmv1.Task) store.Task {
	if task == nil {
		return store.Task{}
	}
	return store.Task{
		ID:         task.Id,
		DayKey:     task.DayKey,
		Kind:       task.Kind,
		Title:      task.Title,
		Progress:   task.Progress,
		Target:     task.Target,
		RewardCoin: task.RewardCoin,
		Claimed:    task.Claimed,
	}
}

var errApplyBadRequest = errors.New("gateway: invalid push payload")
