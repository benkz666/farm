package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/bus"
	"farm/server/internal/cross"
	"farm/server/internal/farm"
	"farm/server/internal/farmrpc"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
)

type crossPending struct {
	connection *wsConnection
	command    uint32
	clientSeq  uint32
	dayID      uint32
	rewarded   bool
	kind       farm.PlotActionKind
	steal      bool
	frozenCoin int64
	timer      *time.Timer
}

type crossActionResponse = cross.VisitorReward

type stealRequest struct {
	OwnerUID  uint64 `json:"owner_uid"`
	PlotIndex uint32 `json:"plot_index"`
	CropID    uint32 `json:"crop_id"`
}

// WithCrossEventBus enables the visitor-side half of cross-farm actions.
// Gateway construction subscribes to results so a returned CrossResult can
// settle the visitor reservation and answer the original WebSocket request.
func WithCrossEventBus(eventBus bus.EventBus) Option {
	return func(gateway *Gateway) {
		gateway.crossBus = eventBus
		gateway.crossVisitor = cross.NewVisitor()
	}
}

func (g *Gateway) startCrossResultConsumer() {
	if g.crossBus == nil || g.crossVisitor == nil {
		return
	}
	g.crossSubscribeErr = g.crossBus.Subscribe(context.Background(), bus.TopicCrossResult, g.handleCrossResult)
}

func (g *Gateway) handleVisitorMutualAid(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if g.crossBus == nil || g.crossVisitor == nil || g.crossSubscribeErr != nil ||
		(g.runtime == nil && g.farmRPC == nil) {
		response.Err = pkgerr.Internal
		return response
	}

	var payload plotActionRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PlotIndex > 255 || payload.Arg != 0 {
		response.Err = pkgerr.BadRequest
		return response
	}
	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomMu.Unlock()
	if ownerUID == 0 || ownerUID == connection.uid || payload.OwnerUID != ownerUID {
		response.Err = pkgerr.NotOwner
		return response
	}
	if g.friends == nil {
		response.Err = pkgerr.NotFriend
		return response
	}
	friends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
	if err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	if !friends {
		response.Err = pkgerr.NotFriend
		return response
	}
	kind, actionKind, ok := crossActionKind(request.Cmd)
	if !ok {
		response.Err = pkgerr.NotOwner
		return response
	}

	reqID := g.nextCrossReqID.Add(1)
	action := cross.CrossAction{
		ReqID:      reqID,
		Kind:       actionKind,
		VisitorUID: connection.uid,
		OwnerUID:   ownerUID,
		PlotIndex:  uint8(payload.PlotIndex),
	}
	pending := &crossPending{
		connection: connection,
		command:    request.Cmd,
		clientSeq:  request.ClientSeq,
		dayID:      logicalDayID(g.Now()),
		kind:       kind,
	}
	if _, err := g.crossVisitor.Reserve(action); err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	rewarded, reserveCode := g.reserveCrossVisitor(action, pending.dayID)
	if reserveCode != pkgerr.OK {
		_, _, _ = g.crossVisitor.Settle(cross.CrossResult{
			ReqID: action.ReqID, VisitorUID: action.VisitorUID, OwnerUID: action.OwnerUID, Code: reserveCode,
		})
		response.Err = reserveCode
		return response
	}
	pending.rewarded = rewarded

	g.crossPending.Store(reqID, pending)
	pending.timer = time.AfterFunc(cross.PendingTimeout, func() {
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Timeout,
		})
	})
	encoded, err := json.Marshal(action)
	if err != nil {
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Internal,
		})
		return Envelope{}
	}
	if err := g.crossBus.Publish(context.Background(), bus.TopicCrossAction, ownerKey(ownerUID), encoded); err != nil {
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Internal,
		})
	}

	// The original response is written by finishCrossResult after the owner
	// decision (or timeout). Cmd=0 tells serveWS not to emit a second response.
	return Envelope{}
}

func (g *Gateway) handleVisitorSteal(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if g.crossBus == nil || g.crossVisitor == nil || g.crossSubscribeErr != nil ||
		(g.runtime == nil && g.farmRPC == nil) {
		response.Err = pkgerr.Internal
		return response
	}

	var payload stealRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PlotIndex > 255 || payload.CropID > 0xFFFF {
		response.Err = pkgerr.BadRequest
		return response
	}
	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomMu.Unlock()
	if ownerUID == 0 || ownerUID == connection.uid || payload.OwnerUID != ownerUID {
		response.Err = pkgerr.NotOwner
		return response
	}
	if g.friends == nil {
		response.Err = pkgerr.NotFriend
		return response
	}
	friends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
	if err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	if !friends {
		response.Err = pkgerr.NotFriend
		return response
	}
	crop, ok := gameconf.CropByID(uint16(payload.CropID))
	if !ok {
		response.Err = pkgerr.BadRequest
		return response
	}
	compensation := int64(crop.FruitPrice) * 10
	reqID := g.nextCrossReqID.Add(1)
	action := cross.CrossAction{
		ReqID:        reqID,
		Kind:         cross.Steal,
		VisitorUID:   connection.uid,
		OwnerUID:     ownerUID,
		PlotIndex:    uint8(payload.PlotIndex),
		CropID:       uint16(payload.CropID),
		Compensation: compensation,
	}
	pending := &crossPending{
		connection: connection,
		command:    request.Cmd,
		clientSeq:  request.ClientSeq,
		steal:      true,
		frozenCoin: compensation,
	}
	if _, err := g.crossVisitor.Reserve(action); err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	_, reserveCode := g.reserveCrossVisitor(action, 0)
	if reserveCode != pkgerr.OK {
		_, _, _ = g.crossVisitor.Settle(cross.CrossResult{
			ReqID: action.ReqID, VisitorUID: action.VisitorUID, OwnerUID: action.OwnerUID, Code: reserveCode,
		})
		response.Err = reserveCode
		return response
	}

	g.crossPending.Store(reqID, pending)
	pending.timer = time.AfterFunc(cross.PendingTimeout, func() {
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Timeout,
		})
	})
	encoded, err := json.Marshal(action)
	if err != nil {
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Internal,
		})
		return Envelope{}
	}
	if err := g.crossBus.Publish(context.Background(), bus.TopicCrossAction, ownerKey(ownerUID), encoded); err != nil {
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Internal,
		})
	}
	return Envelope{}
}

func (g *Gateway) handleCrossResult(_ string, payload []byte) error {
	var result cross.CrossResult
	if err := json.Unmarshal(payload, &result); err != nil {
		// Corrupt messages cannot be recovered by Kafka retry.
		return nil
	}
	g.finishCrossResult(result)
	return nil
}

func (g *Gateway) finishCrossResult(result cross.CrossResult) {
	settled, applied, err := g.crossVisitor.Settle(result)
	if err != nil || !applied {
		return
	}
	raw, ok := g.crossPending.LoadAndDelete(result.ReqID)
	if !ok {
		return
	}
	pending := raw.(*crossPending)
	if pending.timer != nil {
		pending.timer.Stop()
	}

	code := result.Code
	reward, playerDelta, settledCode := g.settleCrossVisitor(result, pending)
	code = settledCode
	_ = settled // Settle guarantees the result matched this visitor reservation.

	if pending.connection == nil {
		return
	}
	if playerDelta != nil {
		pending.connection.pushPlayerDelta(*playerDelta)
	}
	reward.ReqID = result.ReqID
	response := Envelope{
		Cmd:       pending.command,
		ClientSeq: pending.clientSeq,
		Err:       code,
		Payload:   emptyPayload,
	}
	if code == pkgerr.OK || (pending.steal && code == pkgerr.StealIntercepted) {
		response.Payload = marshalPayload(reward)
	}
	_ = pending.connection.respond(response)
}

func (g *Gateway) reserveCrossVisitor(action cross.CrossAction, dayID uint32) (bool, pkgerr.Code) {
	reservation := cross.VisitorReservation{Action: action, DayID: dayID}
	if g.runtime != nil {
		var rewarded bool
		var code pkgerr.Code
		if err := g.runtime.Do(action.VisitorUID, func(visitor *actor.FarmActor) error {
			if visitor == nil || visitor.Aggregate == nil {
				return errors.New("gateway: visitor actor aggregate is nil")
			}
			rewarded, code = cross.ReserveVisitor(visitor.Aggregate, reservation)
			return nil
		}); err != nil {
			return false, pkgerr.Internal
		}
		return rewarded, code
	}
	remote, err := g.executeFarmRPC(context.Background(), action.VisitorUID, farmrpc.CommandRequest{
		Operation: farmrpc.OperationCrossReserve,
		Payload:   marshalPayload(reservation),
	})
	if err != nil {
		return false, pkgerr.Internal
	}
	if remote.Err != pkgerr.OK {
		return false, remote.Err
	}
	var response farmrpc.CrossReserveResponse
	if err := unmarshalPayload(remote.Payload, &response); err != nil {
		return false, pkgerr.Internal
	}
	return response.Rewarded, pkgerr.OK
}

func (g *Gateway) settleCrossVisitor(
	result cross.CrossResult,
	pending *crossPending,
) (cross.VisitorReward, *farm.PlayerDelta, pkgerr.Code) {
	settlement := cross.VisitorSettlement{
		Result:          result,
		DayID:           pending.dayID,
		Rewarded:        pending.rewarded,
		MaintenanceKind: pending.kind,
		Steal:           pending.steal,
		FrozenCoin:      pending.frozenCoin,
	}
	if g.runtime != nil {
		var reward cross.VisitorReward
		var playerDelta *farm.PlayerDelta
		var code pkgerr.Code
		if err := g.runtime.Do(result.VisitorUID, func(visitor *actor.FarmActor) error {
			if visitor == nil || visitor.Aggregate == nil {
				return errors.New("gateway: visitor actor aggregate is nil")
			}
			reward, playerDelta, code = cross.SettleVisitor(visitor.Aggregate, settlement)
			return nil
		}); err != nil {
			return cross.VisitorReward{ReqID: result.ReqID}, nil, pkgerr.Internal
		}
		return reward, playerDelta, code
	}
	remote, err := g.executeFarmRPC(context.Background(), result.VisitorUID, farmrpc.CommandRequest{
		Operation: farmrpc.OperationCrossSettle,
		Payload:   marshalPayload(settlement),
	})
	if err != nil {
		return cross.VisitorReward{ReqID: result.ReqID}, nil, pkgerr.Internal
	}
	var response farmrpc.CrossSettleResponse
	if len(remote.Payload) > 0 {
		if err := unmarshalPayload(remote.Payload, &response); err != nil {
			return cross.VisitorReward{ReqID: result.ReqID}, nil, pkgerr.Internal
		}
	}
	return response.Reward, nil, remote.Err
}

func crossActionKind(command uint32) (farm.PlotActionKind, cross.ActionKind, bool) {
	switch command {
	case CommandWater:
		return farm.Water, cross.Water, true
	case CommandRemoveWeed:
		return farm.Weed, cross.RemoveWeed, true
	case CommandRemovePest:
		return farm.Pest, cross.RemovePest, true
	default:
		return 0, "", false
	}
}

func ownerKey(uid uint64) string {
	return "uid:" + strconv.FormatUint(uid, 10)
}

func logicalDayID(now int64) uint32 {
	const demoLogicalDayMs int64 = 5 * 60 * 1000
	if now <= 0 {
		return 0
	}
	return uint32(now / demoLogicalDayMs)
}
