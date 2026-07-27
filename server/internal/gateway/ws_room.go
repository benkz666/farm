package gateway

import (
	"context"
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

type syncFarmRequest struct {
	OwnerUID uint64 `json:"owner_uid"`
	FromSeq  uint64 `json:"from_seq"`
}

type syncFarmResponse struct {
	Deltas   []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq  uint64                 `json:"farm_seq"`
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

	ownerUID, relation, code := g.farmAccess(connection.uid, payload.OwnerUID)
	if code != pkgerr.OK {
		response.Err = code
		return response
	}

	var enter enterFarmResponse
	if err := g.runtime.Do(ownerUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		changes := farmActor.Aggregate.AdvanceAll(g.Now())
		enter = enterFarmResponse{
			Snapshot:   farmActor.Aggregate.Snapshot(),
			FarmSeq:    farmActor.Aggregate.FarmSeq,
			ServerTime: g.Now(),
			Relation:   relation,
		}
		g.enterRoom(connection, ownerUID)
		if len(changes) > 0 {
			delta := farm.FarmDelta{
				OwnerUID: ownerUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    changes,
			}
			farmActor.Deltas.Append(delta)
			g.rooms.BroadcastExcept(delta, connection.id)
		}
		return nil
	}); err != nil {
		response.Err = pkgerr.Internal
		return response
	}

	response.Payload = marshalPayload(enter)
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

	ownerUID, _, code := g.farmAccess(connection.uid, payload.OwnerUID)
	if code != pkgerr.OK {
		response.Err = code
		return response
	}

	var sync syncFarmResponse
	if err := g.runtime.Do(ownerUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}

		sync.FarmSeq = farmActor.Aggregate.FarmSeq
		if payload.FromSeq == sync.FarmSeq {
			return nil
		}
		if payload.FromSeq > sync.FarmSeq {
			snapshot := farmActor.Aggregate.Snapshot()
			sync.Snapshot = &snapshot
			return nil
		}
		deltas, ok := farmActor.Deltas.Since(payload.FromSeq + 1)
		if !ok || len(deltas) == 0 {
			snapshot := farmActor.Aggregate.Snapshot()
			sync.Snapshot = &snapshot
			return nil
		}
		sync.Deltas = deltas
		return nil
	}); err != nil {
		response.Err = pkgerr.Internal
		return response
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

func (g *Gateway) enterRoom(connection *wsConnection, ownerUID uint64) {
	if connection == nil || connection.id == 0 || g.rooms == nil {
		return
	}

	connection.roomMu.Lock()
	previousOwnerUID := connection.roomUID
	connection.roomUID = ownerUID
	connection.roomMu.Unlock()
	if previousOwnerUID == ownerUID {
		return
	}
	if previousOwnerUID != 0 {
		g.rooms.Unsubscribe(previousOwnerUID, connection.id)
	}
	g.rooms.SubscribeViewer(ownerUID, connection.id, connection.uid, func(delta farm.FarmDelta) {
		connection.roomMu.Lock()
		receiving := connection.roomUID == ownerUID
		connection.roomMu.Unlock()
		if !receiving {
			return
		}
		_ = connection.respond(Envelope{
			Cmd:       CommandFarmDelta,
			ClientSeq: 0,
			Payload:   marshalPayload(delta),
		})
	})
}

func (g *Gateway) leaveFarm(connection *wsConnection) {
	if connection == nil || connection.id == 0 || g.rooms == nil {
		return
	}

	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomUID = 0
	connection.roomMu.Unlock()
	if ownerUID != 0 {
		g.rooms.Unsubscribe(ownerUID, connection.id)
	}
}
