package gateway

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
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
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/pkgjson"
)

// crossPending 只保存「这次请求该回给谁」的传输态。
//
// 资源预占本身记在访客聚合里（farm.CrossReservation），所以这份内存丢失的唯一
// 后果是客户端收不到本次应答，不会让冻结的金币失去回滚的责任人。
type crossPending struct {
	connection *wsConnection
	command    uint32
	clientSeq  uint32
	steal      bool
	timer      *time.Timer
}

type crossActionResponse = cross.VisitorReward

type stealRequest struct {
	OwnerUID  pkgjson.UID `json:"owner_uid"`
	PlotIndex uint32      `json:"plot_index"`
	CropID    uint32      `json:"crop_id"`
}

// WithCrossEventBus enables the visitor-side half of cross-farm actions.
// Gateway construction subscribes to results so a returned CrossResult can
// settle the visitor reservation and answer the original WebSocket request.
func WithCrossEventBus(eventBus bus.EventBus) Option {
	return func(gateway *Gateway) {
		gateway.crossBus = eventBus
		gateway.crossEnabled = true
		gateway.nextCrossReqID.Store(randomReqIDSeed())
	}
}

// randomReqIDSeed 让每个 Gateway 进程的 req_id 从一个随机起点递增。
//
// 从 1 开始递增会让不同 Gateway 为不同访客生成同一个 req_id，而主人侧的幂等表
// 以 (owner_uid, req_id) 为键——撞键会把前一个访客的结果返回给后一个。
func randomReqIDSeed() uint64 {
	var buf [8]byte
	if _, err := crand.Read(buf[:]); err != nil {
		// 熵不可用时退回时间戳：仍然让不同进程几乎不可能从同一点开始。
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(buf[:])
}

func (g *Gateway) startCrossResultConsumer() {
	if g.crossBus == nil || !g.crossEnabled {
		return
	}
	g.crossSubscribeErr = g.crossBus.Subscribe(context.Background(), bus.TopicCrossResult, g.handleCrossResult)
}

// crossReady 判断跨农场链路是否可用：事件总线、订阅、以及一条能触达访客聚合的
// 权威路径（本进程 Runtime 或远端 Farm RPC）三者缺一不可。
func (g *Gateway) crossReady() bool {
	return g.crossBus != nil && g.crossEnabled && g.crossSubscribeErr == nil &&
		(g.runtime != nil || g.farmRPC != nil)
}

// resolveCrossTarget 校验当前连接确实在 claimedOwnerUID 的房间里并且互为好友。
func (g *Gateway) resolveCrossTarget(connection *wsConnection, claimedOwnerUID uint64) (uint64, pkgerr.Code) {
	connection.roomMu.Lock()
	ownerUID := connection.roomUID
	connection.roomMu.Unlock()
	if ownerUID == 0 || ownerUID == connection.uid || claimedOwnerUID != ownerUID {
		return 0, pkgerr.NotOwner
	}
	if g.friends == nil {
		return 0, pkgerr.NotFriend
	}
	friends, err := g.friends.AreFriends(context.Background(), connection.uid, ownerUID)
	if err != nil {
		return 0, pkgerr.Internal
	}
	if !friends {
		return 0, pkgerr.NotFriend
	}
	return ownerUID, pkgerr.OK
}

func (g *Gateway) handleVisitorMutualAid(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if !g.crossReady() {
		response.Err = pkgerr.Internal
		return response
	}

	var payload plotActionRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PlotIndex > 255 || payload.Arg != 0 {
		response.Err = pkgerr.BadRequest
		return response
	}
	ownerUID, code := g.resolveCrossTarget(connection, uint64(payload.OwnerUID))
	if code != pkgerr.OK {
		response.Err = code
		return response
	}
	_, actionKind, ok := crossActionKind(request.Cmd)
	if !ok {
		response.Err = pkgerr.NotOwner
		return response
	}

	action := cross.CrossAction{
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
		response.Err = pkgerr.Internal
		return response
	}

	var payload stealRequest
	if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PlotIndex > 255 || payload.CropID > 0xFFFF {
		response.Err = pkgerr.BadRequest
		return response
	}
	ownerUID, code := g.resolveCrossTarget(connection, uint64(payload.OwnerUID))
	if code != pkgerr.OK {
		response.Err = code
		return response
	}
	crop, ok := gameconf.CropByID(uint16(payload.CropID))
	if !ok {
		response.Err = pkgerr.BadRequest
		return response
	}

	action := cross.CrossAction{
		ReqID:        g.nextCrossReqID.Add(1),
		Kind:         cross.Steal,
		VisitorUID:   connection.uid,
		OwnerUID:     ownerUID,
		PlotIndex:    uint8(payload.PlotIndex),
		CropID:       uint16(payload.CropID),
		Compensation: gameconf.StealCompensation(crop),
	}
	return g.dispatchCrossAction(connection, request, action, 0)
}

// dispatchCrossAction 走完访客侧预占并把动作投递给主人分片。
//
// 返回空 Envelope（Cmd=0）告诉 serveWS 不要立即应答：本次请求的应答由
// finishCrossResult 在拿到主人决定后写出，或由超时定时器写出。
func (g *Gateway) dispatchCrossAction(
	connection *wsConnection,
	request Envelope,
	action cross.CrossAction,
	dayID uint32,
) Envelope {
	if code := g.reserveCrossVisitor(action, dayID); code != pkgerr.OK {
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
		steal:      action.Kind == cross.Steal,
	}
	g.crossPending.Store(action.ReqID, pending)
	pending.timer = time.AfterFunc(cross.PendingTimeout, func() {
		g.timeoutCrossAction(action.ReqID)
	})

	encoded, err := json.Marshal(action)
	if err == nil {
		err = g.crossBus.Publish(context.Background(), bus.TopicCrossAction, ownerKey(action.OwnerUID), encoded)
	}
	if err != nil {
		obs.L().Error("cross action publish failed",
			"component", "gateway",
			"op", "publish_cross_action",
			"err", err.Error(),
		)
		// 动作从未离开本进程，立即回滚预占而不是等 10 秒惰性过期。
		g.finishCrossResult(cross.CrossResult{
			ReqID:      action.ReqID,
			VisitorUID: action.VisitorUID,
			OwnerUID:   action.OwnerUID,
			Code:       pkgerr.Internal,
		})
	}
	return Envelope{}
}

// timeoutCrossAction 到点应答客户端超时，但不动访客侧预占。
//
// 主人侧可能只是慢了：预占继续留在访客聚合里，5 到 10 秒之间到达的迟到回执仍会
// 正确结算并通过 PlayerDelta 补推给客户端；真正丢失的请求由聚合的惰性过期兜底。
// 到点就回滚会制造「主人侧已扣果实、访客侧已退款」这类对不上的账。
func (g *Gateway) timeoutCrossAction(reqID uint64) {
	obs.L().Debug("cross action timed out",
		"component", "gateway",
		"op", "timeout_cross_action",
	)
	raw, ok := g.crossPending.LoadAndDelete(reqID)
	if !ok {
		return
	}
	pending := raw.(*crossPending)
	if pending.connection == nil {
		return
	}
	_ = pending.connection.respond(Envelope{
		Cmd:       pending.command,
		ClientSeq: pending.clientSeq,
		Err:       pkgerr.Timeout,
		Payload:   emptyPayload,
	})
}

func (g *Gateway) handleCrossResult(_ string, payload []byte) error {
	var result cross.CrossResult
	if err := json.Unmarshal(payload, &result); err != nil {
		// Corrupt messages cannot be recovered by Kafka retry.
		return nil
	}
	obs.L().Debug("cross result received",
		"component", "gateway",
		"op", "handle_cross_result",
		"code", int(result.Code),
	)
	g.finishCrossResult(result)
	return nil
}

func (g *Gateway) finishCrossResult(result cross.CrossResult) {
	var pending *crossPending
	if raw, ok := g.crossPending.LoadAndDelete(result.ReqID); ok {
		pending = raw.(*crossPending)
		if pending.timer != nil {
			pending.timer.Stop()
		}
	}
	obs.L().Debug("cross result finished",
		"component", "gateway",
		"op", "finish_cross_result",
		"code", int(result.Code),
		"pending_found", pending != nil,
	)

	// 即便客户端已被超时应答、或本进程根本没有这条传输态（Gateway 重启后重放），
	// 也必须结算：访客聚合里的那条预占只有这里能取走。重复投递会因为预占已被删除
	// 而落到 pkgerr.Timeout 分支，不会二次发奖。
	reward, playerDelta, code := g.settleCrossVisitor(result)

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
	if code == pkgerr.OK || (pending.steal && code == pkgerr.StealIntercepted) {
		response.Payload = marshalPayload(reward)
	}
	_ = pending.connection.respond(response)
}

func (g *Gateway) reserveCrossVisitor(action cross.CrossAction, dayID uint32) pkgerr.Code {
	reservation := cross.VisitorReservation{Action: action, DayID: dayID}
	if g.runtime != nil {
		var code pkgerr.Code
		if err := g.runtime.Do(action.VisitorUID, func(visitor *actor.FarmActor) error {
			if visitor == nil || visitor.Aggregate == nil {
				return errors.New("gateway: visitor actor aggregate is nil")
			}
			code = cross.ReserveVisitor(visitor.Aggregate, reservation, g.Now())
			return nil
		}); err != nil {
			return pkgerr.Internal
		}
		return code
	}
	remote, err := g.executeFarmRPC(context.Background(), action.VisitorUID, farmrpc.CommandRequest{
		Operation: farmrpc.OperationCrossReserve,
		Payload:   marshalPayload(reservation),
	})
	if err != nil {
		return pkgerr.Internal
	}
	return remote.Err
}

func (g *Gateway) settleCrossVisitor(
	result cross.CrossResult,
) (cross.VisitorReward, *farm.PlayerDelta, pkgerr.Code) {
	if g.runtime != nil {
		var reward cross.VisitorReward
		var playerDelta *farm.PlayerDelta
		var code pkgerr.Code
		if err := g.runtime.Do(result.VisitorUID, func(visitor *actor.FarmActor) error {
			if visitor == nil || visitor.Aggregate == nil {
				return errors.New("gateway: visitor actor aggregate is nil")
			}
			reward, playerDelta, code = cross.SettleVisitor(visitor.Aggregate, result, g.Now())
			return nil
		}); err != nil {
			return cross.VisitorReward{ReqID: result.ReqID}, nil, pkgerr.Internal
		}
		return reward, playerDelta, code
	}
	remote, err := g.executeFarmRPC(context.Background(), result.VisitorUID, farmrpc.CommandRequest{
		Operation: farmrpc.OperationCrossSettle,
		Payload:   marshalPayload(result),
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
	// 分片模式下同样要把 PlayerDelta 交回调用方，否则偷菜到手的果实与解冻的金币
	// 只存在于权威分片，客户端界面要到下一次进农场才对得上。
	return response.Reward, response.PlayerDelta, remote.Err
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

// logicDayID 返回 Gateway 当前时钟所属的逻辑日，与 farm 侧共用 gameconf 口径。
func (g *Gateway) logicDayID() uint32 {
	return gameconf.LogicDayID(gameconf.TimeProfileDemo, g.Now())
}
