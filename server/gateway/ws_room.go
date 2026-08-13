package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/farmsvr/room"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/clientjson"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/telemetry"
)

var errFriendAccessRevoked = errors.New("gateway: friend access revoked during enter")

const roomWatermarkFreshness = 2 * time.Second

type syncFarmRequest struct {
	OwnerUID clientjson.UID    `json:"owner_uid"`
	FromSeq  clientjson.Uint64 `json:"from_seq"`
}

type syncFarmResponse struct {
	Deltas             []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot           *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq            clientjson.Uint64      `json:"farm_seq"`
	ServerTime         int64                  `json:"server_time"`
	TimeProfile        string                 `json:"time_profile"`
	TimeProfileMutable bool                   `json:"time_profile_mutable"`
}

func (g *Gateway) handleEnterFarm(connection *wsConnection, request Envelope) Envelope {
	return g.handleEnterFarmCore(connection, request, nil)
}

type gatewayPayloadFields struct {
	enabled  bool
	mutable  bool
	relation string
}

func (g *Gateway) handleEnterFarmForWire(connection *wsConnection, request Envelope) (Envelope, gatewayPayloadFields) {
	var fields gatewayPayloadFields
	response := g.handleEnterFarmCore(connection, request, &fields)
	return response, fields
}

func (g *Gateway) handleEnterFarmCore(connection *wsConnection, request Envelope, wireFields *gatewayPayloadFields) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	var payload enterFarmRequest
	if request.EnterFarmRequest != nil {
		payload.OwnerUID = clientjson.UID(request.EnterFarmRequest.OwnerUid)
	} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
		response.Err = errcode.BadRequest
		return response
	}

	ownerUID, relation, code := g.farmAccess(connection.uid, uint64(payload.OwnerUID))
	if code != errcode.OK {
		response.Err = code
		return response
	}

	if g.farmRPC != nil {
		// Subscribe before the Farm executes so a concurrent callback is held
		// until the EnterFarm snapshot response has reached this client.
		if err := g.enterRoom(connection, ownerUID); err != nil {
			response.Err = errcode.Internal
			return response
		}
		result, err := g.executeFarmRPC(context.Background(), ownerUID, farmrpc.CommandRequest{
			Operation:      farmrpc.OperationEnterFarm,
			Originator:     g.connectionRef(connection),
			PreferPrepared: relation == "SELF" && wireFields != nil,
			ClientCommand:  request.Cmd,
			ClientRequest:  &publicv3.CommandRequest{},
		})
		if err != nil {
			g.leaveFarm(connection)
			response.Err = errcode.Internal
			return response
		}
		if result.Err != errcode.OK {
			g.leaveFarm(connection)
			response.Err = result.Err
			return response
		}
		connection.setRoomWatermark(ownerUID, result.FarmSeq)
		if relation == "SELF" {
			if wireFields != nil {
				wireFields.enabled = true
				wireFields.mutable = g.allowDebug
				wireFields.relation = relation
				response.Payload = result.Payload
				response.PreparedPayload = result.PreparedPayload
				response.PreparedField = result.PreparedField
				return response
			}
			payload, appendErr := appendTrustedGatewayPayloadFields(result.Payload, g.allowDebug, relation)
			if appendErr != nil {
				g.leaveFarm(connection)
				response.Err = errcode.Internal
				return response
			}
			response.Payload = payload
			return response
		}
		var remote farmrpc.EnterFarmResponse
		if err := unmarshalPayload(result.Payload, &remote); err != nil {
			g.leaveFarm(connection)
			response.Err = errcode.Internal
			return response
		}
		if relation == "FRIEND" {
			stillFriends, err := g.refreshFriendLease(connection, ownerUID)
			if err != nil || !stillFriends {
				g.leaveFarm(connection)
				response.Err = errcode.NotFriend
				if err != nil {
					response.Err = errcode.Internal
				}
				return response
			}
		}
		response.Payload = marshalPayload(enterFarmResponse{
			Snapshot:           redactFarmSnapshot(remote.Snapshot, relation),
			FarmSeq:            clientjson.Uint64(remote.FarmSeq),
			ServerTime:         remote.ServerTime,
			TimeProfile:        remote.TimeProfile,
			TimeProfileMutable: g.allowDebug,
			Relation:           relation,
		})
		if relation == "FRIEND" {
			// 已经拿到快照并组好响应，串门任务计数失败不能让玩家进不去好友农场。
			if err := g.advanceVisitTask(connection.uid); err != nil {
				telemetry.L().Error("gateway advance visit task failed",
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
	if err := g.runtime.Do(ownerUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		changes := farmActor.Aggregate.AdvanceAllWithProfile(g.Now(), g.TimeProfile())
		enter = enterFarmResponse{
			Snapshot:           redactFarmSnapshot(farmActor.Aggregate.Snapshot(), relation),
			FarmSeq:            clientjson.Uint64(farmActor.Aggregate.FarmSeq),
			ServerTime:         g.Now(),
			TimeProfile:        g.TimeProfile(),
			TimeProfileMutable: g.allowDebug,
			Relation:           relation,
		}
		if err := g.enterRoom(connection, ownerUID); err != nil {
			return err
		}
		if relation == "FRIEND" {
			stillFriends, err := g.refreshFriendLease(connection, ownerUID)
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
			response.Err = errcode.NotFriend
			return response
		}
		response.Err = errcode.Internal
		return response
	}
	if refreshHint {
		g.writeStealHint(ownerUID, stealable)
	}

	response.Payload = marshalPayload(enter)
	connection.setRoomWatermark(ownerUID, uint64(enter.FarmSeq))
	if relation == "FRIEND" {
		if err := g.advanceVisitTask(connection.uid); err != nil {
			telemetry.L().Error("gateway advance visit task failed",
				"component", "gateway",
				"op", "advance_visit_task",
				"err", err.Error(),
			)
		}
	}
	return response
}

func (g *Gateway) handleSyncFarm(connection *wsConnection, request Envelope) Envelope {
	return g.handleSyncFarmCore(connection, request, nil, time.Time{})
}

func (g *Gateway) handleSyncFarmForWire(connection *wsConnection, request Envelope) (Envelope, gatewayPayloadFields) {
	return g.handleSyncFarmForWireAt(connection, request, time.Now())
}

func (g *Gateway) handleSyncFarmForWireAt(connection *wsConnection, request Envelope, observedAt time.Time) (Envelope, gatewayPayloadFields) {
	var fields gatewayPayloadFields
	response := g.handleSyncFarmCore(connection, request, &fields, observedAt)
	return response, fields
}

func (g *Gateway) handleSyncFarmCore(connection *wsConnection, request Envelope, wireFields *gatewayPayloadFields, observedAt time.Time) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	var payload syncFarmRequest
	if request.SyncFarmRequest != nil {
		payload.OwnerUID = clientjson.UID(request.SyncFarmRequest.OwnerUid)
		payload.FromSeq = clientjson.Uint64(request.SyncFarmRequest.FromSeq)
	} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
		response.Err = errcode.BadRequest
		return response
	}

	ownerUID, relation, code := g.farmAccess(connection.uid, uint64(payload.OwnerUID))
	if code != errcode.OK {
		response.Err = code
		return response
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if farmSeq, ok := connection.matchesFreshRoomWatermark(ownerUID, uint64(payload.FromSeq), observedAt); ok {
		serverTime := g.Now()
		timeProfile := g.TimeProfile()
		response.Payload = marshalPayload(syncFarmResponse{
			FarmSeq:            clientjson.Uint64(farmSeq),
			ServerTime:         serverTime,
			TimeProfile:        timeProfile,
			TimeProfileMutable: g.allowDebug,
		})
		if wireFields != nil {
			prepared, err := clientwire.MarshalSyncFarmCaughtUpPayload(farmSeq, serverTime, timeProfile, g.allowDebug)
			if err == nil {
				response.PreparedPayload = prepared
				response.PreparedField = clientwire.PreparedSyncFarmResponse
			}
		}
		return response
	}

	if g.farmRPC != nil {
		result, err := g.executeFarmRPC(context.Background(), ownerUID, farmrpc.CommandRequest{
			Operation:      farmrpc.OperationSyncFarm,
			Originator:     g.connectionRef(connection),
			Payload:        marshalPayload(farmrpc.SyncFarmRequest{FromSeq: uint64(payload.FromSeq)}),
			PreferPrepared: relation == "SELF" && wireFields != nil,
			ClientCommand:  request.Cmd,
			ClientRequest:  &publicv3.CommandRequest{FromSeq: uint64(payload.FromSeq)},
		})
		if err != nil {
			response.Err = errcode.Internal
			return response
		}
		response.Err = result.Err
		if result.Err == errcode.OK {
			connection.setRoomWatermark(ownerUID, result.FarmSeq)
			if relation == "SELF" {
				if wireFields != nil {
					wireFields.enabled = true
					wireFields.mutable = g.allowDebug
					response.Payload = result.Payload
					response.PreparedPayload = result.PreparedPayload
					response.PreparedField = result.PreparedField
					return response
				}
				payload, appendErr := appendTrustedGatewayPayloadFields(result.Payload, g.allowDebug, "")
				if appendErr != nil {
					response.Err = errcode.Internal
					return response
				}
				response.Payload = payload
				return response
			}
			var sync syncFarmResponse
			if err := unmarshalPayload(result.Payload, &sync); err != nil {
				response.Err = errcode.Internal
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
	if err := g.runtime.Do(ownerUID, func(farmActor *room.FarmActor) error {
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
		sync.FarmSeq = clientjson.Uint64(farmActor.Aggregate.FarmSeq)
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
		response.Err = errcode.Internal
		return response
	}

	if advancedDelta != nil {
		g.rooms.BroadcastExcept(*advancedDelta, connection.id)
	}
	if refreshHint {
		g.writeStealHint(ownerUID, stealable)
	}
	response.Payload = marshalPayload(sync)
	connection.setRoomWatermark(ownerUID, uint64(sync.FarmSeq))
	return response
}

func (connection *wsConnection) setRoomWatermark(ownerUID, farmSeq uint64) {
	if connection == nil || ownerUID == 0 {
		return
	}
	connection.roomMu.Lock()
	if connection.roomUID == ownerUID {
		connection.roomSeq = farmSeq
		connection.roomSeqKnown = true
		connection.roomSeqObservedAt = time.Now().UnixNano()
	}
	connection.roomMu.Unlock()
}

func (connection *wsConnection) matchesFreshRoomWatermark(ownerUID, fromSeq uint64, now time.Time) (uint64, bool) {
	if connection == nil || ownerUID == 0 {
		return 0, false
	}
	connection.roomMu.Lock()
	defer connection.roomMu.Unlock()
	fresh := connection.roomUID == ownerUID && connection.roomSeqKnown &&
		fromSeq == connection.roomSeq && now.UnixNano()-connection.roomSeqObservedAt <= roomWatermarkFreshness.Nanoseconds()
	return connection.roomSeq, fresh
}

func (connection *wsConnection) observeRoomDeltaLocked(farmSeq uint64) {
	if farmSeq == 0 || !connection.roomSeqKnown {
		connection.roomSeqKnown = false
		return
	}
	if farmSeq != connection.roomSeq+1 {
		connection.roomSeqKnown = false
		return
	}
	connection.roomSeq = farmSeq
	connection.roomSeqKnown = true
	connection.roomSeqObservedAt = time.Now().UnixNano()
}

func (connection *wsConnection) advanceRoomWatermark(ownerUID, farmSeq uint64) {
	if connection == nil || ownerUID == 0 || farmSeq == 0 {
		return
	}
	connection.roomMu.Lock()
	defer connection.roomMu.Unlock()
	if connection.roomUID != ownerUID || !connection.roomSeqKnown {
		return
	}
	if farmSeq != connection.roomSeq && farmSeq != connection.roomSeq+1 {
		connection.roomSeqKnown = false
		return
	}
	connection.roomSeq = farmSeq
	connection.roomSeqObservedAt = time.Now().UnixNano()
}

func (g *Gateway) farmAccess(viewerUID, requestedOwnerUID uint64) (ownerUID uint64, relation string, code errcode.Code) {
	ownerUID = requestedOwnerUID
	if ownerUID == 0 {
		ownerUID = viewerUID
	}
	if ownerUID == viewerUID {
		return ownerUID, "SELF", errcode.OK
	}
	if g.friends == nil {
		return 0, "", errcode.NotFriend
	}
	friends, err := g.friends.AreFriends(context.Background(), viewerUID, ownerUID)
	if err != nil {
		return 0, "", errcode.Internal
	}
	if !friends {
		return 0, "", errcode.NotFriend
	}
	return ownerUID, "FRIEND", errcode.OK
}

func redactFarmSnapshot(snap farm.FarmSnapshotJSON, relation string) farm.FarmSnapshotJSON {
	if relation == "FRIEND" {
		return farm.VisitorSafeFarmSnapshot(snap)
	}
	return snap
}

// appendGatewayPayloadFields validates an arbitrary Farm-produced JSON object
// before appending Gateway-owned fields. The production RPC path validates at
// executeFarmRPC and calls the trusted variant to avoid scanning it twice.
func appendGatewayPayloadFields(payload json.RawMessage, mutable bool, relation string) (json.RawMessage, error) {
	if err := clientwire.ValidatePayloadObject(payload); err != nil {
		return nil, errors.New("gateway: invalid Farm RPC payload")
	}
	return appendTrustedGatewayPayloadFields(payload, mutable, relation)
}

func appendTrustedGatewayPayloadFields(payload json.RawMessage, mutable bool, relation string) (json.RawMessage, error) {
	capacity := len(payload) + len(relation) + 64
	return appendTrustedGatewayPayloadFieldsTo(make([]byte, 0, capacity), payload, mutable, relation)
}

func appendTrustedGatewayPayloadFieldsTo(result []byte, payload json.RawMessage, mutable bool, relation string) (json.RawMessage, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
		return nil, errors.New("gateway: invalid Farm RPC payload")
	}
	contents := bytes.TrimSpace(payload[1 : len(payload)-1])
	result = append(result, '{')
	result = append(result, contents...)
	if len(contents) > 0 {
		result = append(result, ',')
	}
	result = append(result, `"time_profile_mutable":`...)
	result = strconv.AppendBool(result, mutable)
	if relation != "" {
		result = append(result, `,"relation":`...)
		result = strconv.AppendQuote(result, relation)
	}
	result = append(result, '}')
	return json.RawMessage(result), nil
}

// appendTrustedGatewayEnvelope writes the final client Envelope and injects
// Gateway-owned fields into a validated Farm payload in one destination buffer.
// This avoids allocating an intermediate augmented payload on every hot-path
// EnterFarm/SyncFarm response.
func appendTrustedGatewayEnvelope(dst []byte, envelope Envelope, fields gatewayPayloadFields) ([]byte, error) {
	dst = append(dst, `{"cmd":`...)
	dst = strconv.AppendUint(dst, uint64(envelope.Cmd), 10)
	dst = append(dst, `,"client_seq":`...)
	dst = strconv.AppendUint(dst, uint64(envelope.ClientSeq), 10)
	dst = append(dst, `,"err":`...)
	dst = strconv.AppendInt(dst, int64(envelope.Err), 10)
	dst = append(dst, `,"payload":`...)
	var err error
	dst, err = appendTrustedGatewayPayloadFieldsTo(dst, envelope.Payload, fields.mutable, fields.relation)
	if err != nil {
		return nil, err
	}
	dst = append(dst, '}')
	return dst, nil
}

func (g *Gateway) enterRoom(connection *wsConnection, ownerUID uint64) error {
	if connection == nil || connection.id == 0 || g.rooms == nil {
		return nil
	}
	// Re-entering the same room still starts a new snapshot/delta ordering
	// barrier, but it must not renew the distributed subscription on every
	// EnterFarm. The periodic connection lease renewal owns that responsibility.
	connection.roomMu.Lock()
	alreadySubscribed := connection.roomUID == ownerUID
	if alreadySubscribed {
		connection.holdFarmDeltas = true
		connection.heldFarmDeltas = nil
		connection.roomSeqKnown = false
	}
	connection.roomMu.Unlock()
	if alreadySubscribed {
		return nil
	}
	if g.connRegistry != nil {
		if err := g.connRegistry.Subscribe(context.Background(), ownerUID, connection.id, g.gatewayID); err != nil {
			return err
		}
	}

	connection.roomMu.Lock()
	previousOwnerUID := connection.roomUID
	connection.roomUID = ownerUID
	connection.roomSeq = 0
	connection.roomSeqKnown = false
	connection.roomSeqObservedAt = 0
	connection.friendLeaseUID = 0
	connection.friendLeaseRevision = 0
	connection.friendLeaseExpiresAt = 0
	connection.holdFarmDeltas = true
	connection.heldFarmDeltas = nil
	connection.roomMu.Unlock()
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
		if err := connection.pushFarmDelta(ownerUID, delta, encoded); err != nil {
			telemetry.L().Warn("gateway local FarmDelta push failed",
				"component", "gateway",
				"op", "push_local_farm_delta",
				"owner_uid", ownerUID,
				"farm_seq", delta.FarmSeq,
				"err", err.Error(),
			)
			connection.dropSlowConnection()
		}
	})
	return nil
}

func (g *Gateway) leaveFarm(connection *wsConnection) {
	if connection == nil {
		return
	}

	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomUID = 0
	connection.roomSeq = 0
	connection.roomSeqKnown = false
	connection.roomSeqObservedAt = 0
	connection.friendLeaseUID = 0
	connection.friendLeaseRevision = 0
	connection.friendLeaseExpiresAt = 0
	connection.holdFarmDeltas = false
	connection.heldFarmDeltas = nil
	connection.roomMu.Unlock()
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
