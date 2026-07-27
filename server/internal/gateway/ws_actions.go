package gateway

import (
	"context"
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/farmrpc"
	"farm/server/internal/pkgerr"
)

type plotActionRequest struct {
	OwnerUID  uint64 `json:"owner_uid"`
	PlotIndex uint32 `json:"plot_index"`
	Arg       uint32 `json:"arg"`
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
		if payload.OwnerUID != 0 && payload.OwnerUID != connection.uid {
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
		if request.Cmd == CommandTill && g.farmRPC != nil {
			remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
				Operation:        farmrpc.OperationTill,
				OriginatorConnID: connection.id,
				Payload:          request.Payload,
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
		var farmSeq uint64
		var delta *farm.FarmDelta
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

func plotChange(index uint8, plot farm.Plot) farm.PlotChange {
	snapshot := farm.PlotSnapshotOf(index, plot)
	return farm.PlotChange{
		Index:          snapshot.Index,
		State:          snapshot.State,
		CropID:         snapshot.CropID,
		SeasonIndex:    snapshot.SeasonIndex,
		SeasonTotal:    snapshot.SeasonTotal,
		MatureAt:       snapshot.MatureAt,
		SeasonDuration: snapshot.SeasonDuration,
		FinalYield:     snapshot.FinalYield,
		LastWaterAt:    snapshot.LastWaterAt,
		WeedSince:      snapshot.WeedSince,
		PestSince:      snapshot.PestSince,
	}
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
