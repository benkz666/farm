package gateway

import (
	"context"
	"errors"

	"farm/server/domain/farm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/farmsvr/room"
	"farm/server/shared/errcode"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
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
			if request.CommandRequest == nil {
				if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
					response.Err = errcode.BadRequest
					return response
				}
			}
			payload.Kind = farmrpc.PetStatus
		case CommandPetActivate:
			var activate petActivateRequest
			if request.CommandRequest != nil {
				activate.DogType = request.CommandRequest.DogType
			} else if err := unmarshalPayload(request.Payload, &activate); err != nil {
				response.Err = errcode.BadRequest
				return response
			}
			if activate.DogType > 0xFF {
				response.Err = errcode.BadRequest
				return response
			}
			payload.Kind = farmrpc.PetActivate
			payload.DogType = farm.DogType(activate.DogType)
		case CommandPetFeed:
			var feed petFeedRequest
			if request.CommandRequest != nil {
				feed.Grams = request.CommandRequest.Grams
			} else if err := unmarshalPayload(request.Payload, &feed); err != nil {
				response.Err = errcode.BadRequest
				return response
			}
			payload.Kind = farmrpc.PetFeed
			payload.Grams = feed.Grams
		default:
			response.Err = errcode.BadRequest
			return response
		}
		remote, err := g.executeFarmRPC(context.Background(), connection.uid, farmrpc.CommandRequest{
			Operation:     farmrpc.OperationPet,
			Originator:    g.connectionRef(connection),
			ClientCommand: request.Cmd,
			ClientRequest: request.CommandRequest,
			Payload:       marshalPayload(payload),
		})
		if err != nil {
			response.Err = errcode.Internal
			return response
		}
		response.Err = remote.Err
		if remote.Err == errcode.OK {
			response.Payload = remote.Payload
			response.CommandResponse = remote.ClientResponse
		}
		return response
	}
	if g.runtime == nil {
		response.Err = errcode.Internal
		return response
	}

	var result farm.ActionResult
	var status farm.PetStatus
	var delta *farm.FarmDelta
	if err := g.runtime.Do(connection.uid, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("gateway: actor aggregate is nil")
		}
		beforeFarmSeq := farmActor.Aggregate.FarmSeq
		switch request.Cmd {
		case CommandPetStatus:
			if request.CommandRequest == nil {
				if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
					return err
				}
			}
			result.Err = errcode.OK
		case CommandPetActivate:
			var payload petActivateRequest
			if request.CommandRequest != nil {
				payload.DogType = request.CommandRequest.DogType
			} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
				return errors.New("gateway: invalid pet activate payload")
			}
			if payload.DogType > 0xFF {
				return errors.New("gateway: invalid pet activate payload")
			}
			result = farmActor.Aggregate.PetActivateWithProfile(farm.DogType(payload.DogType), g.Now(), g.TimeProfile())
		case CommandPetFeed:
			var payload petFeedRequest
			if request.CommandRequest != nil {
				payload.Grams = request.CommandRequest.Grams
			} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
				return err
			}
			result = farmActor.Aggregate.PetFeedWithProfile(farm.PetFeedReq{Grams: payload.Grams}, g.Now(), g.TimeProfile())
		default:
			return errors.New("gateway: unsupported pet command")
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			farmActor.MarkDirty()
			guardDog := farm.GuardDogSnapshotOf(farmActor.Aggregate.Pet)
			emitted := farm.FarmDelta{
				OwnerUID: connection.uid,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				GuardDog: &guardDog,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
		}
		status = farmActor.Aggregate.PetStatus(g.Now())
		return nil
	}); err != nil {
		response.Err = errcode.BadRequest
		return response
	}
	if delta != nil {
		g.rooms.BroadcastExcept(*delta, connection.id)
	}
	response.Err = result.Err
	if result.Err == errcode.OK {
		response.Payload = marshalPayload(status)
		if request.Cmd == CommandPetFeed {
			if err := g.advanceTask(connection.uid, store.TaskFeedDogID); err != nil {
				telemetry.L().Error("gateway advance pet feed task failed",
					"component", "gateway",
					"op", "advance_task",
					"err", err.Error(),
				)
			}
		}
	}
	return response
}
