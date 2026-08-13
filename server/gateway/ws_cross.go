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

// colocatedCrossExecutor collapses the three cross-farm RPC hops when the
// visitor and owner are routed to the same Farm instance.
type colocatedCrossExecutor interface {
	CanExecuteCrossAction(action crossfarm.CrossAction) bool
	ExecuteCrossAction(ctx context.Context, action crossfarm.CrossAction, dayID uint32) (crossfarm.CrossExecution, error)
}

type crossResultAckEnqueuer interface {
	EnqueueCrossResultAck(ownerUID, visitorUID, reqID uint64) error
}

type friendshipRevisionSource interface {
	Revision() uint64
}

const friendRoomLeaseTTL = 30 * time.Second

// crossPending 只保存「这次请求该回给谁」的传输态。
type crossPending struct {
	connection *wsConnection
	command    uint32
	clientSeq  uint32
	steal      bool
	deadline   time.Time
}

func (g *Gateway) acquireCrossSlot() bool {
	if g == nil {
		return true
	}
	return acquireBoundedSlot(g.crossSlots)
}

func (g *Gateway) releaseCrossSlot() {
	if g == nil {
		return
	}
	releaseBoundedSlot(g.crossSlots)
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
	leaseUID := connection.friendLeaseUID
	leaseRevision := connection.friendLeaseRevision
	leaseExpiresAt := connection.friendLeaseExpiresAt
	connection.roomMu.Unlock()
	if ownerUID == 0 || ownerUID == connection.uid || claimedOwnerUID != ownerUID {
		return 0, errcode.NotOwner
	}
	if g.friends == nil {
		return 0, errcode.NotFriend
	}
	currentRevision, tracksRevision := g.friendshipRevision()
	if leaseUID == ownerUID && time.Now().UnixNano() < leaseExpiresAt &&
		(!tracksRevision || leaseRevision == currentRevision) {
		return ownerUID, errcode.OK
	}
	friends, err := g.refreshFriendLease(connection, ownerUID)
	if err != nil {
		return 0, errcode.Internal
	}
	if !friends {
		return 0, errcode.NotFriend
	}
	return ownerUID, errcode.OK
}

func (g *Gateway) friendshipRevision() (uint64, bool) {
	if g == nil || g.friends == nil {
		return 0, false
	}
	source, ok := g.friends.(friendshipRevisionSource)
	if !ok {
		return 0, false
	}
	return source.Revision(), true
}

func (g *Gateway) refreshFriendLease(connection *wsConnection, ownerUID uint64) (bool, error) {
	if g == nil || g.friends == nil || connection == nil || ownerUID == 0 {
		return false, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		before, tracksRevision := g.friendshipRevision()
		friends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
		if err != nil || !friends {
			return friends, err
		}
		after, _ := g.friendshipRevision()
		if tracksRevision && before != after {
			continue
		}
		connection.roomMu.Lock()
		if connection.roomUID == ownerUID {
			connection.friendLeaseUID = ownerUID
			connection.friendLeaseRevision = after
			connection.friendLeaseExpiresAt = time.Now().Add(friendRoomLeaseTTL).UnixNano()
		}
		connection.roomMu.Unlock()
		return true, nil
	}
	return false, errors.New("gateway: friendship changed while granting lease")
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
		ReqID:              g.nextCrossReqID.Add(1),
		Kind:               actionKind,
		VisitorUID:         connection.uid,
		OwnerUID:           ownerUID,
		PlotIndex:          uint8(payload.PlotIndex),
		FriendshipVerified: true,
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
		ReqID:              g.nextCrossReqID.Add(1),
		Kind:               crossfarm.Steal,
		VisitorUID:         connection.uid,
		OwnerUID:           ownerUID,
		PlotIndex:          uint8(payload.PlotIndex),
		CropID:             uint16(payload.CropID),
		Compensation:       gameconfig.StealCompensation(crop),
		FriendshipVerified: true,
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
		if g.metrics != nil {
			g.metrics.ObserveWSRateLimited("cross_slot")
		}
		return Envelope{
			Cmd: request.Cmd, ClientSeq: request.ClientSeq,
			Err: errcode.RateLimited, Payload: emptyPayload,
		}
	}
	if executor, ok := g.crossClient.(colocatedCrossExecutor); ok && executor.CanExecuteCrossAction(action) {
		action.Originator = g.connectionRef(connection)
		g.registerCrossPending(connection, request, action)
		if !gatewayCrossScheduler.submit(func() { g.executeColocatedCrossAction(executor, action, dayID) }) {
			g.discardCrossPending(action.ReqID)
			return Envelope{Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: errcode.RateLimited, Payload: emptyPayload}
		}
		return Envelope{}
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

	g.registerCrossPending(connection, request, action)
	if !gatewayCrossScheduler.submit(func() { g.applyCrossActionAsync(action) }) {
		g.discardCrossPending(action.ReqID)
		return Envelope{Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: errcode.RateLimited, Payload: emptyPayload}
	}
	return Envelope{}
}

func (g *Gateway) registerCrossPending(connection *wsConnection, request Envelope, action crossfarm.CrossAction) {
	pending := &crossPending{
		connection: connection,
		command:    request.Cmd,
		clientSeq:  request.ClientSeq,
		steal:      action.Kind == crossfarm.Steal,
		deadline:   time.Now().Add(crossfarm.PendingTimeout),
	}
	g.crossPending.Store(action.ReqID, pending)
	gatewayCrossScheduler.scheduleTimeout(g, action.ReqID, pending.deadline)
}

func (g *Gateway) discardCrossPending(reqID uint64) {
	if _, ok := g.crossPending.LoadAndDelete(reqID); ok {
		g.releaseCrossSlot()
	}
}

func (g *Gateway) executeColocatedCrossAction(
	executor colocatedCrossExecutor,
	action crossfarm.CrossAction,
	dayID uint32,
) {
	ctx, cancel := context.WithTimeout(context.Background(), crossfarm.PendingTimeout)
	execution, err := executor.ExecuteCrossAction(ctx, action, dayID)
	cancel()
	if err != nil {
		telemetry.L().Error("colocated cross action failed",
			"component", "gateway",
			"op", "execute_colocated_cross_action",
			"err", err.Error(),
		)
		g.failCrossAction(action.ReqID, errcode.Internal)
		return
	}
	g.finishColocatedCrossExecution(execution)
}

func (g *Gateway) failCrossAction(reqID uint64, code errcode.Code) {
	raw, ok := g.crossPending.LoadAndDelete(reqID)
	if !ok {
		return
	}
	pending := raw.(*crossPending)
	g.releaseCrossSlot()
	if pending.connection != nil {
		_ = pending.connection.respond(Envelope{
			Cmd: pending.command, ClientSeq: pending.clientSeq,
			Err: code, Payload: emptyPayload,
		})
	}
}

func (g *Gateway) finishColocatedCrossExecution(execution crossfarm.CrossExecution) {
	raw, ok := g.crossPending.LoadAndDelete(execution.Result.ReqID)
	if !ok {
		return
	}
	pending := raw.(*crossPending)
	g.releaseCrossSlot()
	if execution.OwnerCommitted && execution.AckRequired && execution.Code != errcode.Internal {
		g.ackCrossResult(execution.Result)
	}
	if pending.connection == nil {
		if execution.PlayerDelta != nil {
			_ = g.PublishPlayerDelta(context.Background(), execution.Result.VisitorUID, *execution.PlayerDelta)
		}
		return
	}
	if execution.PlayerDelta != nil {
		pending.connection.pushPlayerDelta(*execution.PlayerDelta)
	}
	if execution.FarmDelta != nil {
		if err := pending.connection.pushFarmDelta(execution.Result.OwnerUID, *execution.FarmDelta, nil); err != nil {
			telemetry.L().Debug("colocated FarmDelta delivery failed",
				"component", "gateway", "op", "deliver_colocated_farm_delta", "err", err.Error())
		}
	}
	execution.Reward.ReqID = execution.Result.ReqID
	response := Envelope{
		Cmd:       pending.command,
		ClientSeq: pending.clientSeq,
		Err:       execution.Code,
		Payload:   emptyPayload,
	}
	if execution.Code == errcode.OK || (pending.steal && execution.Code == errcode.StealIntercepted) {
		response.CommandResponse = clientwire.NewVisitorRewardCommandResponse(
			execution.Reward.ReqID, execution.Reward.ExpGained, execution.Reward.CoinGained,
			uint32(execution.Reward.CropID), uint32(execution.Reward.Amount),
			execution.Reward.Compensation, uint32(execution.Reward.DogType),
		)
	}
	_ = pending.connection.respond(response)
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
	raw, ok := g.crossPending.LoadAndDelete(reqID)
	if !ok {
		return
	}
	telemetry.L().Debug("cross action timed out",
		"component", "gateway",
		"op", "timeout_cross_action",
	)
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
	if batcher, ok := g.crossClient.(crossResultAckEnqueuer); ok {
		if err := batcher.EnqueueCrossResultAck(result.OwnerUID, result.VisitorUID, result.ReqID); err != nil {
			telemetry.L().Debug("cross result ack enqueue failed",
				"component", "gateway",
				"op", "enqueue_cross_result_ack",
				"err", err.Error(),
			)
		}
		return
	}
	if !gatewayCrossScheduler.submit(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := g.crossClient.AcknowledgeCrossResult(ctx, result.OwnerUID, result.VisitorUID, result.ReqID); err != nil {
			telemetry.L().Debug("cross result ack failed",
				"component", "gateway",
				"op", "ack_cross_result",
				"err", err.Error(),
			)
		}
	}) {
		telemetry.L().Debug("cross result ack queue full",
			"component", "gateway", "op", "ack_cross_result")
	}
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
