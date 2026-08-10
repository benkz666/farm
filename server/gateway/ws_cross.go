package gateway

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/crossfarm"
	"farm/server/farmsvr/room"
	"farm/server/shared/clientjson"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/telemetry"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CrossFarmClient applies owner adjudication and acknowledges outbox over typed gRPC.
type CrossFarmClient interface {
	ReserveCrossVisitor(ctx context.Context, action crossfarm.CrossAction, dayID uint32) (errcode.Code, error)
	ApplyCrossAction(ctx context.Context, action crossfarm.CrossAction) (crossfarm.CrossResult, error)
	DeliverCrossResult(ctx context.Context, result crossfarm.CrossResult) (crossfarm.VisitorReward, *farm.PlayerDelta, errcode.Code, error)
	AcknowledgeCrossResult(ctx context.Context, ownerUID, visitorUID, reqID uint64) error
}

// crossPending 只保存「这次请求该回给谁」的传输态。
type crossPending struct {
	connection *wsConnection
	command    uint32
	clientSeq  uint32
	steal      bool
	timer      *time.Timer
}

func (g *Gateway) acquireCrossSlot() bool {
	if g == nil || g.crossSlots == nil {
		return true
	}
	select {
	case g.crossSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseCrossSlot() {
	if g == nil || g.crossSlots == nil {
		return
	}
	select {
	case <-g.crossSlots:
	default:
	}
}

type stealRequest struct {
	OwnerUID  clientjson.UID `json:"owner_uid"`
	PlotIndex uint32         `json:"plot_index"`
	CropID    uint32         `json:"crop_id"`
}

// WithCrossFarmClient enables the visitor-side half of cross-farm actions.
func WithCrossFarmClient(client CrossFarmClient) Option {
	return func(gateway *Gateway) {
		gateway.crossClient = client
		gateway.crossEnabled = true
		gateway.nextCrossReqID.Store(randomReqIDSeed())
	}
}

func randomReqIDSeed() uint64 {
	var buf [8]byte
	if _, err := crand.Read(buf[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(buf[:])
}

func (g *Gateway) crossReady() bool {
	return g.crossClient != nil && g.crossEnabled && (g.runtime != nil || g.farmRPC != nil)
}

func (g *Gateway) resolveCrossTarget(connection *wsConnection, claimedOwnerUID uint64) (uint64, errcode.Code) {
	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomMu.Unlock()
	if ownerUID == 0 || ownerUID == connection.uid || claimedOwnerUID != ownerUID {
		return 0, errcode.NotOwner
	}
	if g.friends == nil {
		return 0, errcode.NotFriend
	}
	friends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
	if err != nil {
		return 0, errcode.Internal
	}
	if !friends {
		return 0, errcode.NotFriend
	}
	return ownerUID, errcode.OK
}

func (g *Gateway) handleVisitorMutualAid(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if !g.crossReady() {
		response.Err = errcode.Internal
		return response
	}

	var payload plotActionRequest
	var decodeErr error
	if request.CommandRequest != nil {
		payload = plotActionRequest{OwnerUID: clientjson.UID(request.CommandRequest.OwnerUid), PlotIndex: request.CommandRequest.PlotIndex, Arg: request.CommandRequest.Arg}
	} else {
		decodeErr = unmarshalPayload(request.Payload, &payload)
	}
	if decodeErr != nil || payload.PlotIndex > 255 || payload.Arg != 0 {
		response.Err = errcode.BadRequest
		return response
	}
	ownerUID, code := g.resolveCrossTarget(connection, uint64(payload.OwnerUID))
	if code != errcode.OK {
		response.Err = code
		return response
	}
	_, actionKind, ok := crossActionKind(request.Cmd)
	if !ok {
		response.Err = errcode.NotOwner
		return response
	}

	action := crossfarm.CrossAction{
		ReqID:      g.nextCrossReqID.Add(1),
		Kind:       actionKind,
		VisitorUID: connection.uid,
		OwnerUID:   ownerUID,
		PlotIndex:  uint8(payload.PlotIndex),
	}
	return g.dispatchCrossAction(connection, request, action, g.logicDayID())
}

func (g *Gateway) handleVisitorSteal(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if !g.crossReady() {
		response.Err = errcode.Internal
		return response
	}

	var payload stealRequest
	var decodeErr error
	if request.CommandRequest != nil {
		payload = stealRequest{OwnerUID: clientjson.UID(request.CommandRequest.OwnerUid), PlotIndex: request.CommandRequest.PlotIndex, CropID: request.CommandRequest.CropId}
	} else {
		decodeErr = unmarshalPayload(request.Payload, &payload)
	}
	if decodeErr != nil || payload.PlotIndex > 255 || payload.CropID > 0xFFFF {
		response.Err = errcode.BadRequest
		return response
	}
	ownerUID, code := g.resolveCrossTarget(connection, uint64(payload.OwnerUID))
	if code != errcode.OK {
		response.Err = code
		return response
	}
	crop, ok := gameconfig.CropByID(uint16(payload.CropID))
	if !ok {
		response.Err = errcode.BadRequest
		return response
	}

	action := crossfarm.CrossAction{
		ReqID:        g.nextCrossReqID.Add(1),
		Kind:         crossfarm.Steal,
		VisitorUID:   connection.uid,
		OwnerUID:     ownerUID,
		PlotIndex:    uint8(payload.PlotIndex),
		CropID:       uint16(payload.CropID),
		Compensation: gameconfig.StealCompensation(crop),
	}
	return g.dispatchCrossAction(connection, request, action, 0)
}

func (g *Gateway) dispatchCrossAction(
	connection *wsConnection,
	request Envelope,
	action crossfarm.CrossAction,
	dayID uint32,
) Envelope {
	if !g.acquireCrossSlot() {
		return Envelope{
			Cmd: request.Cmd, ClientSeq: request.ClientSeq,
			Err: errcode.RateLimited, Payload: emptyPayload,
		}
	}
	if code := g.reserveCrossVisitor(action, dayID); code != errcode.OK {
		g.releaseCrossSlot()
		return Envelope{
			Cmd:       request.Cmd,
			ClientSeq: request.ClientSeq,
			Err:       code,
			Payload:   emptyPayload,
		}
	}

	pending := &crossPending{
		connection: connection,
		command:    request.Cmd,
		clientSeq:  request.ClientSeq,
		steal:      action.Kind == crossfarm.Steal,
	}
	pending.timer = time.AfterFunc(crossfarm.PendingTimeout, func() {
		g.timeoutCrossAction(action.ReqID)
	})
	g.crossPending.Store(action.ReqID, pending)

	go g.applyCrossActionAsync(action)
	return Envelope{}
}

func (g *Gateway) applyCrossActionAsync(action crossfarm.CrossAction) {
	const maxAttempts = 3
	backoff := 50 * time.Millisecond
	var result crossfarm.CrossResult
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		result, err = g.crossClient.ApplyCrossAction(ctx, action)
		cancel()
		if err == nil {
			g.finishCrossResult(result)
			return
		}
		if !retryableCrossApplyError(err) || attempt == maxAttempts-1 {
			break
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	telemetry.L().Error("cross action apply failed",
		"component", "gateway",
		"op", "apply_cross_action",
		"err", err.Error(),
	)
}

func retryableCrossApplyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Internal:
			return true
		}
	}
	return false
}

func (g *Gateway) timeoutCrossAction(reqID uint64) {
	telemetry.L().Debug("cross action timed out",
		"component", "gateway",
		"op", "timeout_cross_action",
	)
	raw, ok := g.crossPending.LoadAndDelete(reqID)
	if !ok {
		return
	}
	pending := raw.(*crossPending)
	g.releaseCrossSlot()
	if pending.connection == nil {
		return
	}
	_ = pending.connection.respond(Envelope{
		Cmd:       pending.command,
		ClientSeq: pending.clientSeq,
		Err:       errcode.Timeout,
		Payload:   emptyPayload,
	})
}

func (g *Gateway) finishCrossResult(result crossfarm.CrossResult) {
	var pending *crossPending
	if raw, ok := g.crossPending.LoadAndDelete(result.ReqID); ok {
		pending = raw.(*crossPending)
		g.releaseCrossSlot()
		if pending.timer != nil {
			pending.timer.Stop()
		}
	}
	telemetry.L().Debug("cross result finished",
		"component", "gateway",
		"op", "finish_cross_result",
		"code", int(result.Code),
		"pending_found", pending != nil,
	)

	reward, playerDelta, code := g.settleCrossVisitor(result)

	if code != errcode.Internal {
		g.ackCrossResult(result)
	}

	if pending == nil || pending.connection == nil {
		if playerDelta != nil {
			_ = g.PublishPlayerDelta(context.Background(), result.VisitorUID, *playerDelta)
		}
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
	if code == errcode.OK || (pending.steal && code == errcode.StealIntercepted) {
		response.CommandResponse = clientwire.NewVisitorRewardCommandResponse(
			reward.ReqID, reward.ExpGained, reward.CoinGained, uint32(reward.CropID),
			uint32(reward.Amount), reward.Compensation, uint32(reward.DogType),
		)
	}
	_ = pending.connection.respond(response)
}

func (g *Gateway) reserveCrossVisitor(action crossfarm.CrossAction, dayID uint32) errcode.Code {
	reservation := crossfarm.VisitorReservation{Action: action, DayID: dayID}
	if g.runtime != nil {
		var code errcode.Code
		if err := g.runtime.Do(action.VisitorUID, func(visitor *room.FarmActor) error {
			if visitor == nil || visitor.Aggregate == nil {
				return errors.New("gateway: visitor actor aggregate is nil")
			}
			code = crossfarm.ReserveVisitor(visitor.Aggregate, reservation, g.Now())
			if code == errcode.OK {
				visitor.RequireFlush()
			}
			return nil
		}); err != nil {
			return errcode.Internal
		}
		return code
	}
	if g.crossClient == nil {
		return errcode.Internal
	}
	code, err := g.crossClient.ReserveCrossVisitor(context.Background(), action, dayID)
	if err != nil {
		return errcode.Internal
	}
	return code
}

func (g *Gateway) settleCrossVisitor(
	result crossfarm.CrossResult,
) (crossfarm.VisitorReward, *farm.PlayerDelta, errcode.Code) {
	if g.runtime != nil {
		var reward crossfarm.VisitorReward
		var playerDelta *farm.PlayerDelta
		var code errcode.Code
		if err := g.runtime.Do(result.VisitorUID, func(visitor *room.FarmActor) error {
			if visitor == nil || visitor.Aggregate == nil {
				return errors.New("gateway: visitor actor aggregate is nil")
			}
			reward, playerDelta, code = crossfarm.SettleVisitor(visitor.Aggregate, result, g.Now())
			// 与远端 Settler 一致：Timeout 重投仍需确认当前聚合已经 durable。
			visitor.RequireFlush()
			return nil
		}); err != nil {
			return crossfarm.VisitorReward{ReqID: result.ReqID}, nil, errcode.Internal
		}
		return reward, playerDelta, code
	}
	if g.crossClient == nil {
		return crossfarm.VisitorReward{ReqID: result.ReqID}, nil, errcode.Internal
	}
	reward, playerDelta, code, err := g.crossClient.DeliverCrossResult(context.Background(), result)
	if err != nil {
		return crossfarm.VisitorReward{ReqID: result.ReqID}, nil, errcode.Internal
	}
	return reward, playerDelta, code
}

func (g *Gateway) ackCrossResult(result crossfarm.CrossResult) {
	if g.crossClient == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := g.crossClient.AcknowledgeCrossResult(ctx, result.OwnerUID, result.VisitorUID, result.ReqID); err != nil {
			telemetry.L().Debug("cross result ack failed",
				"component", "gateway",
				"op", "ack_cross_result",
				"err", err.Error(),
			)
		}
	}()
}

func crossActionKind(command uint32) (farm.PlotActionKind, crossfarm.ActionKind, bool) {
	switch command {
	case CommandWater:
		return farm.Water, crossfarm.Water, true
	case CommandRemoveWeed:
		return farm.Weed, crossfarm.RemoveWeed, true
	case CommandRemovePest:
		return farm.Pest, crossfarm.RemovePest, true
	default:
		return 0, "", false
	}
}

func (g *Gateway) logicDayID() uint32 {
	return gameconfig.LogicDayID(g.TimeProfile(), g.Now())
}
