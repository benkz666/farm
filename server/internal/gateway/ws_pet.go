package gateway

import (
	"errors"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
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
			result = farmActor.Aggregate.PetActivate(farm.DogType(payload.DogType))
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
	}
	return response
}
