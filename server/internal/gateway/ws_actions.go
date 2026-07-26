package gateway

import (
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
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

	switch request.Cmd {
	case CommandTill, CommandClear, CommandPlant, CommandWater,
		CommandRemoveWeed, CommandRemovePest, CommandHarvest:
		var payload plotActionRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		if payload.OwnerUID != 0 && payload.OwnerUID != connection.uid {
			response.Err = pkgerr.NotFriend
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

		var result farm.ActionResult
		var farmSeq uint64
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
				Kind:      kind,
				PlotIndex: uint8(payload.PlotIndex),
				Arg:       uint16(payload.Arg),
			}, g.now())
			farmSeq = farmActor.Aggregate.FarmSeq
			if result.Err == pkgerr.OK || (kind == farm.Clear && result.Err == pkgerr.PlotNotCleanable) {
				response.Payload = marshalPayload(actionResponse{
					FarmSeq: farmSeq,
					Patch:   farmActor.Aggregate.PatchFromAction(result),
				})
			}
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Err = result.Err
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
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
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
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Err = result.Err
		return response

	default:
		response.Err = pkgerr.BadRequest
		return response
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
	case CommandHarvest:
		return farm.Harvest, true
	default:
		return 0, false
	}
}
