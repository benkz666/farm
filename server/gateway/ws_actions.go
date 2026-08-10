package gateway

import (
	"context"
	"errors"

	"farm/server/domain/farm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/farmsvr/room"
	"farm/server/shared/clientjson"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

type plotActionRequest struct {
	OwnerUID  clientjson.UID `json:"owner_uid"`
	PlotIndex uint32         `json:"plot_index"`
	Arg       uint32         `json:"arg"`
}

type shopRequest struct {
	ItemID   uint32 `json:"item_id"`
	Quantity uint32 `json:"quantity"`
}

type actionResponse struct {
	FarmSeq      clientjson.Uint64        `json:"farm_seq"`
	Patch        farm.PatchJSON           `json:"patch"`
	CodexRewards []farm.CodexRewardNotice `json:"codex_rewards,omitempty"`
}

func (g *Gateway) acquireWriteSlot() bool {
	if g == nil || g.writeSlots == nil {
		return true
	}
	select {
	case g.writeSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseWriteSlot() {
	if g == nil || g.writeSlots == nil {
		return
	}
	select {
	case <-g.writeSlots:
	default:
	}
}

func (g *Gateway) handlePlotOrShop(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	connection.roomMu.Lock()
	visiting := connection.roomUID != 0 && connection.roomUID != connection.uid
	connection.roomMu.Unlock()
	if visiting {
		switch request.Cmd {
		case CommandWater, CommandRemoveWeed, CommandRemovePest:
			return g.handleVisitorMutualAid(connection, request)
		case CommandSteal:
			return g.handleVisitorSteal(connection, request)
		case CommandSell:
			// 出售操作只修改 connection.uid 自己的仓库/金币，与当前查看的好友
			// 农场无关；继续进入下方 Shop 分支。购买仍保持拜访期间禁止。
		}
		if request.Cmd != CommandSell {
			response.Err = errcode.NotOwner
			return response
		}
	}
	if !g.acquireWriteSlot() {
		response.Err = errcode.RateLimited
		return response
	}
	defer g.releaseWriteSlot()

	switch request.Cmd {
	case CommandTill, CommandClear, CommandPlant, CommandWater,
		CommandRemoveWeed, CommandRemovePest, CommandFertilize, CommandHarvest:
		var payload plotActionRequest
		var decodeErr error
		if request.CommandRequest != nil {
			payload = plotActionRequest{OwnerUID: clientjson.UID(request.CommandRequest.OwnerUid), PlotIndex: request.CommandRequest.PlotIndex, Arg: request.CommandRequest.Arg}
		} else {
			decodeErr = unmarshalPayload(request.Payload, &payload)
		}
		if decodeErr != nil {
			response.Err = errcode.BadRequest
			return response
		}
		ownerUID := uint64(payload.OwnerUID)
		if ownerUID != 0 && ownerUID != connection.uid {
			response.Err = errcode.NotOwner
			return response
		}
		kind, ok := plotActionKind(request.Cmd)
		if !ok {
			response.Err = errcode.BadRequest
			return response
		}
		if payload.PlotIndex > 255 {
			response.Err = errcode.BadRequest
			return response
		}
		if payload.Arg > 0xFFFF {
			response.Err = errcode.BadRequest
			return response
		}
		if g.farmRPC != nil {
			rpcRequest := farmrpc.CommandRequest{
				Operation:     farmrpc.OperationPlotAction,
				Originator:    g.connectionRef(connection),
				ClientCommand: request.Cmd,
				ClientRequest: request.CommandRequest,
			}
			// Typed WebSocket commands stay typed across Gateway→Farm. JSON is
			// retained only for legacy in-process/unit seams.
			if request.CommandRequest == nil {
				rpcRequest.Payload = marshalPayload(farmrpc.PlotActionRequest{
					OwnerUID:  ownerUID,
					PlotIndex: payload.PlotIndex,
					Arg:       payload.Arg,
					Kind:      kind,
					Command:   request.Cmd,
				})
			}
			remote, err := g.executeFarmRPC(context.Background(), connection.uid, rpcRequest)
			if err != nil {
				response.Err = errcode.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == errcode.OK {
				if len(remote.PreparedPayload) > 0 {
					response.PreparedPayload = remote.PreparedPayload
					response.PreparedField = remote.PreparedField
				} else if remote.ClientResponse != nil {
					response.CommandResponse = remote.ClientResponse
				} else {
					response.Payload = remote.Payload
				}
				connection.advanceRoomWatermark(connection.uid, remote.FarmSeq)
			}
			return response
		}
		if g.runtime == nil {
			response.Err = errcode.Internal
			return response
		}

		var result farm.ActionResult
		var actionPayload actionResponse
		var farmSeq uint64
		var delta *farm.FarmDelta
		var stealable bool
		var stoleHint bool
		if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			beforeFarmSeq := farmActor.Aggregate.FarmSeq
			result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
				Kind:        kind,
				PlotIndex:   uint8(payload.PlotIndex),
				Arg:         uint16(payload.Arg),
				TimeProfile: g.TimeProfile(),
			}, g.Now())
			farmSeq = farmActor.Aggregate.FarmSeq
			if result.Err == errcode.OK {
				actionPayload = actionResponse{
					FarmSeq: clientjson.Uint64(farmSeq),
					Patch:   farmActor.Aggregate.PatchFromAction(result),
				}
			}
			if farmSeq != beforeFarmSeq {
				farmActor.MarkDirty()
				emitted := farm.FarmDelta{
					OwnerUID: connection.uid,
					FarmSeq:  farmSeq,
					Plots: []farm.PlotChange{plotChange(
						uint8(payload.PlotIndex),
						farmActor.Aggregate.Plots[payload.PlotIndex],
					)},
					ActorUID: connection.uid,
					Action:   request.Cmd,
				}
				farmActor.Deltas.Append(emitted)
				delta = &emitted
				if kind == farm.Harvest {
					stealable = farmActor.Aggregate.HasStealable()
					stoleHint = true
				}
			}
			return nil
		}); err != nil {
			response.Err = errcode.Internal
			return response
		}
		response.Err = result.Err
		if result.Err == errcode.OK {
			response.Payload = marshalPayload(actionPayload)
			connection.advanceRoomWatermark(connection.uid, uint64(actionPayload.FarmSeq))
		}
		if delta != nil {
			g.rooms.BroadcastExcept(*delta, connection.id)
		}
		if stoleHint {
			g.writeStealHint(connection.uid, stealable)
		}
		if result.Err == errcode.OK {
			// 同 farmrpc：动作已提交且 Delta 已广播，任务计数失败不能反过来把成功
			// 报成失败，否则客户端会回滚一次真实发生的变更。
			if err := g.advanceGameplayTask(connection.uid, kind); err != nil {
				telemetry.L().Error("gateway advance task failed",
					"component", "gateway",
					"op", "advance_task",
					"err", err.Error(),
				)
			}
		}
		return response

	case CommandBuy, CommandSell:
		var payload shopRequest
		var decodeErr error
		if request.CommandRequest != nil {
			payload = shopRequest{ItemID: request.CommandRequest.ItemId, Quantity: request.CommandRequest.Quantity}
		} else {
			decodeErr = unmarshalPayload(request.Payload, &payload)
		}
		if decodeErr != nil {
			response.Err = errcode.BadRequest
			return response
		}
		if payload.ItemID > 0xFFFF {
			response.Err = errcode.BadRequest
			return response
		}
		if g.farmRPC != nil {
			rpcRequest := farmrpc.CommandRequest{
				Operation:     farmrpc.OperationShop,
				Originator:    g.connectionRef(connection),
				ClientCommand: request.Cmd,
				ClientRequest: request.CommandRequest,
			}
			if request.CommandRequest == nil {
				rpcRequest.Payload = marshalPayload(farmrpc.ShopRequest{
					Buy:      request.Cmd == CommandBuy,
					ItemID:   payload.ItemID,
					Quantity: payload.Quantity,
					Command:  request.Cmd,
				})
			}
			remote, err := g.executeFarmRPC(context.Background(), connection.uid, rpcRequest)
			if err != nil {
				response.Err = errcode.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == errcode.OK {
				if len(remote.PreparedPayload) > 0 {
					response.PreparedPayload = remote.PreparedPayload
					response.PreparedField = remote.PreparedField
				} else if remote.ClientResponse != nil {
					response.CommandResponse = remote.ClientResponse
				} else {
					response.Payload = remote.Payload
				}
				connection.advanceRoomWatermark(connection.uid, remote.FarmSeq)
			}
			return response
		}
		if g.runtime == nil {
			response.Err = errcode.Internal
			return response
		}

		var result farm.ActionResult
		var delta *farm.FarmDelta
		var farmSeq uint64
		if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			beforeFarmSeq := farmActor.Aggregate.FarmSeq
			if request.Cmd == CommandBuy {
				result = farmActor.Aggregate.Buy(farm.BuyReq{
					ItemID:   uint16(payload.ItemID),
					Quantity: payload.Quantity,
				})
			} else {
				result = farmActor.Aggregate.Sell(farm.SellReq{
					ItemID:   uint16(payload.ItemID),
					Quantity: payload.Quantity,
				})
			}
			if result.Err == errcode.OK {
				// 买卖直接改动金币与背包，属于架构 5.3 节的 A 档：必须落盘成功才算
				// 成功，不能留在 30 秒写回窗口里等着被一次强杀抹掉。
				farmActor.RequireFlush()
				response.Payload = marshalPayload(actionResponse{
					FarmSeq: clientjson.Uint64(farmActor.Aggregate.FarmSeq),
					Patch:   farmActor.Aggregate.PatchFromAction(result),
				})
				farmSeq = farmActor.Aggregate.FarmSeq
			}
			if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
				emitted := farm.FarmDelta{
					OwnerUID: connection.uid,
					FarmSeq:  farmActor.Aggregate.FarmSeq,
					ActorUID: connection.uid,
					Action:   request.Cmd,
				}
				farmActor.Deltas.Append(emitted)
				delta = &emitted
			}
			return nil
		}); err != nil {
			response.Err = errcode.Internal
			return response
		}
		response.Err = result.Err
		if result.Err == errcode.OK {
			connection.advanceRoomWatermark(connection.uid, farmSeq)
		}
		if delta != nil {
			g.rooms.BroadcastExcept(*delta, connection.id)
		}
		if result.Err == errcode.OK && request.Cmd == CommandSell {
			if err := g.advanceTask(connection.uid, store.TaskSellID); err != nil {
				telemetry.L().Error("gateway advance sell task failed",
					"component", "gateway",
					"op", "advance_task",
					"err", err.Error(),
				)
			}
		}
		return response

	default:
		response.Err = errcode.BadRequest
		return response
	}
}

func (g *Gateway) advanceGameplayTask(uid uint64, kind farm.PlotActionKind) error {
	var taskID uint32
	switch kind {
	case farm.Plant:
		taskID = store.TaskPlantID
	case farm.Harvest:
		taskID = store.TaskHarvestID
	case farm.Water:
		taskID = store.TaskWaterID
	case farm.Fertilize:
		taskID = store.TaskFertilizeID
	case farm.Till:
		taskID = store.TaskTillID
	case farm.Weed:
		taskID = store.TaskWeedID
	case farm.Pest:
		taskID = store.TaskPestID
	default:
		return nil
	}
	return g.advanceTask(uid, taskID)
}

func (g *Gateway) advanceVisitTask(uid uint64) error {
	return g.advanceTask(uid, store.TaskVisitID)
}

func (g *Gateway) advanceTask(uid uint64, taskID uint32) error {
	if g.farmRPC != nil {
		remote, err := g.executeFarmRPC(context.Background(), uid, farmrpc.CommandRequest{
			Operation: farmrpc.OperationAdvanceTask,
			Payload:   marshalPayload(farmrpc.AdvanceTaskRequest{TaskID: taskID, Amount: 1}),
		})
		if err != nil {
			return err
		}
		if remote.Err != errcode.OK {
			return errors.New("gateway: advance task rejected")
		}
		return nil
	}
	if g.taskMail == nil {
		return nil
	}
	result, err := g.taskMail.AdvanceTask(context.Background(), uid, gameconfig.LocalDayKey(g.Now()), taskID, 1)
	if err != nil {
		return err
	}
	if result.Changed {
		g.publishTaskNotify(uid, result.Task)
	}
	return nil
}

func (g *Gateway) publishTaskNotify(uid uint64, task store.Task) {
	if g == nil || uid == 0 {
		return
	}
	if g.taskNotifyFanout == nil {
		if err := g.PublishTaskNotify(context.Background(), uid, task); err != nil {
			telemetry.L().Error("gateway TaskNotify publish failed",
				"component", "gateway",
				"op", "publish_task_notify",
				"uid", uid,
				"task_id", task.ID,
				"err", err.Error(),
			)
		}
		return
	}
	if err := g.taskNotifyFanout.PublishTaskNotify(context.Background(), uid, task); err != nil {
		telemetry.L().Error("gateway TaskNotify fan-out failed",
			"component", "gateway",
			"op", "fanout_task_notify",
			"uid", uid,
			"task_id", task.ID,
			"err", err.Error(),
		)
	}
}

func plotChange(index uint8, plot farm.Plot) farm.PlotChange {
	return farm.PlotChangeOf(index, plot)
}

func plotActionKind(cmd uint32) (farm.PlotActionKind, bool) {
	switch cmd {
	case CommandTill:
		return farm.Till, true
	case CommandClear:
		return farm.Clear, true
	case CommandPlant:
		return farm.Plant, true
	case CommandWater:
		return farm.Water, true
	case CommandRemoveWeed:
		return farm.Weed, true
	case CommandRemovePest:
		return farm.Pest, true
	case CommandFertilize:
		return farm.Fertilize, true
	case CommandHarvest:
		return farm.Harvest, true
	default:
		return 0, false
	}
}
