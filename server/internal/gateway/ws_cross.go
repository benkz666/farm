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

type crossActionResponse struct {
	ReqID        uint64       `json:"req_id"`
	ExpGained    uint32       `json:"exp_gained"`
	CoinGained   int64        `json:"coin_gained"`
	CropID       uint16       `json:"crop_id,omitempty"`
	Amount       uint16       `json:"amount,omitempty"`
	Compensation int64        `json:"compensation,omitempty"`
	DogType      farm.DogType `json:"dog_type,omitempty"`
}

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
	if g.crossBus == nil || g.crossVisitor == nil || g.crossSubscribeErr != nil || g.runtime == nil {
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
	if err := g.runtime.Do(connection.uid, func(visitor *actor.FarmActor) error {
		if visitor == nil || visitor.Aggregate == nil {
			return errors.New("gateway: visitor actor aggregate is nil")
		}
		pending.rewarded = visitor.Aggregate.ReserveMaintenance(pending.dayID)
		if _, err := g.crossVisitor.Reserve(action); err != nil {
			visitor.Aggregate.RollbackMaintenance(pending.dayID, pending.rewarded)
			return err
		}
		return nil
	}); err != nil {
		response.Err = pkgerr.Internal
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
	if g.crossBus == nil || g.crossVisitor == nil || g.crossSubscribeErr != nil || g.runtime == nil {
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
	var reserveCode pkgerr.Code
	if err := g.runtime.Do(connection.uid, func(visitor *actor.FarmActor) error {
		if visitor == nil || visitor.Aggregate == nil {
			return errors.New("gateway: visitor actor aggregate is nil")
		}
		reserveCode = visitor.Aggregate.FreezeStealCompensation(compensation)
		if reserveCode != pkgerr.OK {
			return nil
		}
		if _, err := g.crossVisitor.Reserve(action); err != nil {
			visitor.Aggregate.ReleaseStealCompensation(compensation)
			reserveCode = pkgerr.Internal
		}
		return nil
	}); err != nil {
		response.Err = pkgerr.Internal
		return response
	}
	if reserveCode != pkgerr.OK {
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
	var reward crossActionResponse
	if err := g.runtime.Do(result.VisitorUID, func(visitor *actor.FarmActor) error {
		if visitor == nil || visitor.Aggregate == nil {
			return errors.New("gateway: visitor actor aggregate is nil")
		}
		if pending.steal {
			switch result.Code {
			case pkgerr.OK:
				if result.CropID == 0 || result.Amount == 0 {
					code = pkgerr.Internal
					visitor.Aggregate.ReleaseStealCompensation(pending.frozenCoin)
					return nil
				}
				visitor.Aggregate.ReleaseStealCompensation(pending.frozenCoin)
				visitor.Aggregate.Items[farm.FruitItem(result.CropID)] += uint32(result.Amount)
				reward.CropID = result.CropID
				reward.Amount = result.Amount
			case pkgerr.StealIntercepted:
				reward.Compensation = result.Compensation
				reward.DogType = result.DogType
			default:
				visitor.Aggregate.ReleaseStealCompensation(pending.frozenCoin)
			}
			return nil
		}
		if result.Code == pkgerr.OK {
			visitor.Aggregate.SettleMaintenance(pending.dayID, pending.rewarded, pending.kind)
			if pending.rewarded {
				reward.ExpGained = 2
				if pending.kind == farm.Weed || pending.kind == farm.Pest {
					reward.CoinGained = 5
				}
			}
		} else {
			visitor.Aggregate.RollbackMaintenance(pending.dayID, pending.rewarded)
		}
		return nil
	}); err != nil {
		code = pkgerr.Internal
	}
	_ = settled // Settle guarantees the result matched this visitor reservation.

	if pending.connection == nil {
		return
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
