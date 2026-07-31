package gateway

import (
	"context"
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/farmrpc"
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/pkgjson"
)

var errFriendAccessRevoked = errors.New("gateway: friend access revoked during enter")

type syncFarmRequest struct {
	OwnerUID pkgjson.UID    `json:"owner_uid"`
	FromSeq  pkgjson.Uint64 `json:"from_seq"`
}

type syncFarmResponse struct {
	Deltas             []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot           *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq            pkgjson.Uint64         `json:"farm_seq"`
	ServerTime         int64                  `json:"server_time"`
	TimeProfile        string                 `json:"time_profile"`
	TimeProfileMutable bool                   `json:"time_profile_mutable"`
}

func (g *Gateway) handleEnterFarm(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	var payload enterFarmRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil {
		response.Err = pkgerr.BadRequest
		return response
	}

	ownerUID, relation, code := g.farmAccess(connection.uid, uint64(payload.OwnerUID))
	if code != pkgerr.OK {
		response.Err = code
		return response
	}

	if g.farmRPC != nil {
		// Subscribe before the Farm executes so a concurrent callback is held
		// until the EnterFarm snapshot response has reached this client.
		if err := g.enterRoom(connection, ownerUID); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		result, err := g.executeFarmRPC(context.Background(), ownerUID, farmrpc.CommandRequest{
			Operation:  farmrpc.OperationEnterFarm,
			Originator: g.connectionRef(connection),
		})
		if err != nil {
			g.leaveFarm(connection)
			response.Err = pkgerr.Internal
			return response
		}
		if result.Err != pkgerr.OK {
			g.leaveFarm(connection)
			response.Err = result.Err
			return response
		}
		var remote farmrpc.EnterFarmResponse
		if err := unmarshalPayload(result.Payload, &remote); err != nil {
			g.leaveFarm(connection)
			response.Err = pkgerr.Internal
			return response
		}
		if relation == "FRIEND" {
			stillFriends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
			if err != nil || !stillFriends {
				g.leaveFarm(connection)
				response.Err = pkgerr.NotFriend
				if err != nil {
					response.Err = pkgerr.Internal
				}
				return response
			}
		}
		response.Payload = marshalPayload(enterFarmResponse{
			Snapshot:           redactFarmSnapshot(remote.Snapshot, relation),
			FarmSeq:            pkgjson.Uint64(remote.FarmSeq),
			ServerTime:         remote.ServerTime,
			TimeProfile:        remote.TimeProfile,
			TimeProfileMutable: g.allowDebug,
			Relation:           relation,
		})
		if relation == "FRIEND" {
			// 已经拿到快照并组好响应，串门任务计数失败不能让玩家进不去好友农场。
			if err := g.advanceVisitTask(connection.uid); err != nil {
				obs.L().Error("gateway advance visit task failed",
					"component", "gateway",
					"op", "advance_visit_task",
					"err", err.Error(),
				)
			}
		}
		return response
	}

	var enter enterFarmResponse
	var stealable bool
	var refreshHint bool
	if err := g.runtime.Do(ownerUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		changes := farmActor.Aggregate.AdvanceAllWithProfile(g.Now(), g.TimeProfile())
		enter = enterFarmResponse{
			Snapshot:           redactFarmSnapshot(farmActor.Aggregate.Snapshot(), relation),
			FarmSeq:            pkgjson.Uint64(farmActor.Aggregate.FarmSeq),
			ServerTime:         g.Now(),
			TimeProfile:        g.TimeProfile(),
			TimeProfileMutable: g.allowDebug,
			Relation:           relation,
		}
		if err := g.enterRoom(connection, ownerUID); err != nil {
			return err
		}
		if relation == "FRIEND" {
			stillFriends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
			if err != nil || !stillFriends {
				g.leaveFarm(connection)
				if err != nil {
					return err
				}
				return errFriendAccessRevoked
			}
		}
		if len(changes) > 0 {
			// 进入农场时惰性推进出的成熟/枯萎是权威状态，必须在响应前落盘。
			farmActor.RequireFlush()
			delta := farm.FarmDelta{
				OwnerUID: ownerUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    changes,
			}
			farmActor.Deltas.Append(delta)
			g.rooms.BroadcastExcept(delta, connection.id)
			stealable = farmActor.Aggregate.HasStealable()
			refreshHint = true
		}
		return nil
	}); err != nil {
		if errors.Is(err, errFriendAccessRevoked) {
			response.Err = pkgerr.NotFriend
			return response
		}
		response.Err = pkgerr.Internal
		return response
	}
	if refreshHint {
		g.writeStealHint(ownerUID, stealable)
	}

	response.Payload = marshalPayload(enter)
	if relation == "FRIEND" {
		if err := g.advanceVisitTask(connection.uid); err != nil {
			obs.L().Error("gateway advance visit task failed",
				"component", "gateway",
				"op", "advance_visit_task",
				"err", err.Error(),
			)
		}
	}
	return response
}

func (g *Gateway) handleSyncFarm(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	var payload syncFarmRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil {
		response.Err = pkgerr.BadRequest
		return response
	}

	ownerUID, relation, code := g.farmAccess(connection.uid, uint64(payload.OwnerUID))
	if code != pkgerr.OK {
		response.Err = code
		return response
	}

	if g.farmRPC != nil {
		result, err := g.executeFarmRPC(context.Background(), ownerUID, farmrpc.CommandRequest{
			Operation:  farmrpc.OperationSyncFarm,
			Originator: g.connectionRef(connection),
			Payload:    marshalPayload(farmrpc.SyncFarmRequest{FromSeq: uint64(payload.FromSeq)}),
		})
		if err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Err = result.Err
		if result.Err == pkgerr.OK {
			var sync syncFarmResponse
			if err := unmarshalPayload(result.Payload, &sync); err != nil {
				response.Err = pkgerr.Internal
				return response
			}
			if sync.Snapshot != nil {
				safe := redactFarmSnapshot(*sync.Snapshot, relation)
				sync.Snapshot = &safe
			}
			sync.TimeProfileMutable = g.allowDebug
			response.Payload = marshalPayload(sync)
		}
		return response
	}

	var sync syncFarmResponse
	var advancedDelta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := g.runtime.Do(ownerUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}

		now := g.Now()
		changes := farmActor.Aggregate.AdvanceAllWithProfile(now, g.TimeProfile())
		if len(changes) > 0 {
			// SyncFarm 既是补 Delta，也是客户端到期时主动触发惰性时间推进的读屏障。
			// 成熟/枯萎与最终产量必须先持久化，再返回给请求方和房间内其他观察者。
			farmActor.RequireFlush()
			emitted := farm.FarmDelta{
				OwnerUID: ownerUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    changes,
			}
			farmActor.Deltas.Append(emitted)
			advancedDelta = &emitted
			stealable = farmActor.Aggregate.HasStealable()
			refreshHint = true
		}
		sync.FarmSeq = pkgjson.Uint64(farmActor.Aggregate.FarmSeq)
		sync.ServerTime = now
		sync.TimeProfile = g.TimeProfile()
		sync.TimeProfileMutable = g.allowDebug
		if uint64(payload.FromSeq) == uint64(sync.FarmSeq) {
			return nil
		}
		if uint64(payload.FromSeq) > uint64(sync.FarmSeq) {
			snapshot := redactFarmSnapshot(farmActor.Aggregate.Snapshot(), relation)
			sync.Snapshot = &snapshot
			return nil
		}
		deltas, ok := farmActor.Deltas.Since(uint64(payload.FromSeq) + 1)
		if !ok || len(deltas) == 0 {
			snapshot := redactFarmSnapshot(farmActor.Aggregate.Snapshot(), relation)
			sync.Snapshot = &snapshot
			return nil
		}
		sync.Deltas = deltas
		return nil
	}); err != nil {
		response.Err = pkgerr.Internal
		return response
	}

	if advancedDelta != nil {
		g.rooms.BroadcastExcept(*advancedDelta, connection.id)
	}
	if refreshHint {
		g.writeStealHint(ownerUID, stealable)
	}
	response.Payload = marshalPayload(sync)
	return response
}

func (g *Gateway) farmAccess(viewerUID, requestedOwnerUID uint64) (ownerUID uint64, relation string, code pkgerr.Code) {
	ownerUID = requestedOwnerUID
	if ownerUID == 0 {
		ownerUID = viewerUID
	}
	if ownerUID == viewerUID {
		return ownerUID, "SELF", pkgerr.OK
	}
	if g.friends == nil {
		return 0, "", pkgerr.NotFriend
	}
	friends, err := g.friends.AreFriends(context.Background(), viewerUID, ownerUID)
	if err != nil {
		return 0, "", pkgerr.Internal
	}
	if !friends {
		return 0, "", pkgerr.NotFriend
	}
	return ownerUID, "FRIEND", pkgerr.OK
}

func redactFarmSnapshot(snap farm.FarmSnapshotJSON, relation string) farm.FarmSnapshotJSON {
	if relation == "FRIEND" {
		return farm.VisitorSafeFarmSnapshot(snap)
	}
	return snap
}

func (g *Gateway) enterRoom(connection *wsConnection, ownerUID uint64) error {
	if connection == nil || connection.id == 0 || g.rooms == nil {
		return nil
	}
	if g.connRegistry != nil {
		if err := g.connRegistry.Subscribe(context.Background(), ownerUID, connection.id, g.gatewayID); err != nil {
			return err
		}
	}

	connection.writeMu.Lock()
	connection.roomMu.Lock()
	previousOwnerUID := connection.roomUID
	connection.roomUID = ownerUID
	connection.holdFarmDeltas = true
	connection.heldFarmDeltas = nil
	connection.roomMu.Unlock()
	connection.writeMu.Unlock()
	if previousOwnerUID == ownerUID {
		return nil
	}
	if previousOwnerUID != 0 {
		g.rooms.Unsubscribe(previousOwnerUID, connection.id)
		if g.connRegistry != nil {
			_ = g.connRegistry.Unsubscribe(context.Background(), previousOwnerUID, connection.id, g.gatewayID)
		}
	}
	g.rooms.SubscribeViewer(ownerUID, connection.id, connection.uid, func(delta farm.FarmDelta, encoded []byte) {
		connection.pushFarmDelta(ownerUID, delta, encoded)
	})
	return nil
}

func (g *Gateway) leaveFarm(connection *wsConnection) {
	if connection == nil {
		return
	}

	connection.writeMu.Lock()
	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomUID = 0
	connection.holdFarmDeltas = false
	connection.heldFarmDeltas = nil
	connection.roomMu.Unlock()
	connection.writeMu.Unlock()
	if connection.id == 0 || g.rooms == nil {
		return
	}
	if ownerUID != 0 {
		g.rooms.Unsubscribe(ownerUID, connection.id)
		if g.connRegistry != nil {
			_ = g.connRegistry.Unsubscribe(context.Background(), ownerUID, connection.id, g.gatewayID)
		}
	}
}
