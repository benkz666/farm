package gateway

import (
	"context"
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/farmrpc"
	"farm/server/internal/gameconf"
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/pkgjson"
	"farm/server/internal/store"
)

type plotActionRequest struct {
	OwnerUID  pkgjson.UID `json:"owner_uid"`
	PlotIndex uint32      `json:"plot_index"`
	Arg       uint32      `json:"arg"`
}

type shopRequest struct {
	ItemID   uint32 `json:"item_id"`
	Quantity uint32 `json:"quantity"`
}

type actionResponse struct {
	FarmSeq uint64         `json:"farm_seq"`
	Patch   farm.PatchJSON `json:"patch"`
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
		}
		response.Err = pkgerr.NotOwner
		return response
	}

	switch request.Cmd {
	case CommandTill, CommandClear, CommandPlant, CommandWater,
		CommandRemoveWeed, CommandRemovePest, CommandFertilize, CommandHarvest:
		var payload plotActionRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		ownerUID := uint64(payload.OwnerUID)
		if ownerUID != 0 && ownerUID != connection.uid {
			response.Err = pkgerr.NotOwner
			return response
		}
		kind, ok := plotActionKind(request.Cmd)
		if !ok {
			response.Err = pkgerr.BadRequest
			return response
		}
		if payload.PlotIndex > 255 {
			response.Err = pkgerr.BadRequest
			return response
		}
		if payload.Arg > 0xFFFF {
			response.Err = pkgerr.BadRequest
			return response
		}
		if g.farmRPC != nil {
			remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
				Operation:  farmrpc.OperationPlotAction,
				Originator: g.connectionRef(connection),
				Payload: marshalPayload(farmrpc.PlotActionRequest{
					OwnerUID:  ownerUID,
					PlotIndex: payload.PlotIndex,
					Arg:       payload.Arg,
					Kind:      kind,
					Command:   request.Cmd,
				}),
			})
			if err != nil {
				response.Err = pkgerr.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == pkgerr.OK || (kind == farm.Clear && remote.Err == pkgerr.PlotNotCleanable) {
				response.Payload = remote.Payload
			}
			return response
		}
		if g.runtime == nil {
			response.Err = pkgerr.Internal
			return response
		}

		var result farm.ActionResult
		var farmSeq uint64
		var delta *farm.FarmDelta
		var stealable bool
		var stoleHint bool
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			beforeFarmSeq := farmActor.Aggregate.FarmSeq
			result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
				Kind:      kind,
				PlotIndex: uint8(payload.PlotIndex),
				Arg:       uint16(payload.Arg),
			}, g.Now())
			farmSeq = farmActor.Aggregate.FarmSeq
			if result.Err == pkgerr.OK || (kind == farm.Clear && result.Err == pkgerr.PlotNotCleanable) {
				response.Payload = marshalPayload(actionResponse{
					FarmSeq: farmSeq,
					Patch:   farmActor.Aggregate.PatchFromAction(result),
				})
			}
			if farmSeq != beforeFarmSeq {
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
				stealable = farmActor.Aggregate.HasStealable()
				stoleHint = true
			}
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Err = result.Err
		if delta != nil {
			g.rooms.BroadcastExcept(*delta, connection.id)
		}
		if stoleHint {
			g.writeStealHint(connection.uid, stealable)
		}
		if result.Err == pkgerr.OK {
			// 同 farmrpc：动作已提交且 Delta 已广播，任务计数失败不能反过来把成功
			// 报成失败，否则客户端会回滚一次真实发生的变更。
			if err := g.advanceGameplayTask(connection.uid, kind); err != nil {
				obs.L().Error("gateway advance task failed",
					"component", "gateway",
					"op", "advance_task",
					"err", err.Error(),
				)
			}
		}
		return response

	case CommandBuy, CommandSell:
		var payload shopRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		if payload.ItemID > 0xFFFF {
			response.Err = pkgerr.BadRequest
			return response
		}
		if g.farmRPC != nil {
			remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
				Operation:  farmrpc.OperationShop,
				Originator: g.connectionRef(connection),
				Payload: marshalPayload(farmrpc.ShopRequest{
					Buy:      request.Cmd == CommandBuy,
					ItemID:   payload.ItemID,
					Quantity: payload.Quantity,
					Command:  request.Cmd,
				}),
			})
			if err != nil {
				response.Err = pkgerr.Internal
				return response
			}
			response.Err = remote.Err
			if remote.Err == pkgerr.OK {
				response.Payload = remote.Payload
			}
			return response
		}
		if g.runtime == nil {
			response.Err = pkgerr.Internal
			return response
		}

		var result farm.ActionResult
		var delta *farm.FarmDelta
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
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
			if result.Err == pkgerr.OK {
				// 买卖直接改动金币与背包，属于架构 5.3 节的 A 档：必须落盘成功才算
				// 成功，不能留在 30 秒写回窗口里等着被一次强杀抹掉。
				farmActor.RequireFlush()
				response.Payload = marshalPayload(actionResponse{
					FarmSeq: farmActor.Aggregate.FarmSeq,
					Patch:   farmActor.Aggregate.PatchFromAction(result),
				})
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
			response.Err = pkgerr.Internal
			return response
		}
		response.Err = result.Err
		if delta != nil {
			g.rooms.BroadcastExcept(*delta, connection.id)
		}
		return response

	default:
		response.Err = pkgerr.BadRequest
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
	default:
		return nil
	}
	return g.advanceTask(uid, taskID)
}

func (g *Gateway) advanceVisitTask(uid uint64) error {
	return g.advanceTask(uid, store.TaskVisitID)
}

func (g *Gateway) advanceTask(uid uint64, taskID uint32) error {
	if g.taskMail == nil {
		return nil
	}
	result, err := g.taskMail.AdvanceTask(context.Background(), uid, gameconf.LocalDayKey(g.Now()), taskID, 1)
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
			obs.L().Error("gateway TaskNotify publish failed",
				"component", "gateway",
				"op", "publish_task_notify",
				"uid", uid,
				"task_id", task.ID,
				"err", err.Error(),
			)
		}
		return
	}
	publisher := g.taskNotifyFanout
	go func() {
		if err := publisher.PublishTaskNotify(context.Background(), uid, task); err != nil {
			obs.L().Error("gateway TaskNotify fan-out failed",
				"component", "gateway",
				"op", "fanout_task_notify",
				"uid", uid,
				"task_id", task.ID,
				"err", err.Error(),
			)
		}
	}()
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
