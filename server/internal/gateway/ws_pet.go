package gateway

import (
	"context"
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/farmrpc"
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

type petActivateRequest struct {
	DogType uint32 `json:"dog_type"`
}

type petFeedRequest struct {
	Grams uint32 `json:"grams"`
}

func (g *Gateway) handlePet(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if g.farmRPC != nil {
		var payload farmrpc.PetRequest
		switch request.Cmd {
		case CommandPetStatus:
			if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
				response.Err = pkgerr.BadRequest
				return response
			}
			payload.Kind = farmrpc.PetStatus
		case CommandPetActivate:
			var activate petActivateRequest
			if err := unmarshalPayload(request.Payload, &activate); err != nil || activate.DogType > 0xFF {
				response.Err = pkgerr.BadRequest
				return response
			}
			payload.Kind = farmrpc.PetActivate
			payload.DogType = farm.DogType(activate.DogType)
		case CommandPetFeed:
			var feed petFeedRequest
			if err := unmarshalPayload(request.Payload, &feed); err != nil {
				response.Err = pkgerr.BadRequest
				return response
			}
			payload.Kind = farmrpc.PetFeed
			payload.Grams = feed.Grams
		default:
			response.Err = pkgerr.BadRequest
			return response
		}
		remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
			Operation:  farmrpc.OperationPet,
			Originator: g.connectionRef(connection),
			Payload:    marshalPayload(payload),
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
	var status farm.PetStatus
	if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		switch request.Cmd {
		case CommandPetStatus:
			if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
				return err
			}
			result.Err = pkgerr.OK
		case CommandPetActivate:
			var payload petActivateRequest
			if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.DogType > 0xFF {
				return errors.New("gateway: invalid pet activate payload")
			}
			result = farmActor.Aggregate.PetActivate(farm.DogType(payload.DogType), g.Now())
		case CommandPetFeed:
			var payload petFeedRequest
			if err := unmarshalPayload(request.Payload, &payload); err != nil {
				return err
			}
			result = farmActor.Aggregate.PetFeed(farm.PetFeedReq{Grams: payload.Grams}, g.Now())
		default:
			return errors.New("gateway: unsupported pet command")
		}
		status = farmActor.Aggregate.PetStatus(g.Now())
		return nil
	}); err != nil {
		response.Err = pkgerr.BadRequest
		return response
	}
	response.Err = result.Err
	if result.Err == pkgerr.OK {
		response.Payload = marshalPayload(status)
		if request.Cmd == CommandPetFeed {
			if err := g.advanceTask(connection.uid, store.TaskFeedDogID); err != nil {
				obs.L().Error("gateway advance pet feed task failed",
					"component", "gateway",
					"op", "advance_task",
					"err", err.Error(),
				)
			}
		}
	}
	return response
}
