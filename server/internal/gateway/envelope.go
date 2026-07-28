// Package gateway implements the HTTP and JSON WebSocket boundary.
package gateway

import (
	"errors"
	"strings"

	"farm/server/internal/wireenv"
)

const (
	CommandHandshake uint32 = 100
	CommandPing      uint32 = 102
	CommandEnterFarm uint32 = 200
	CommandLeaveFarm uint32 = 202
	CommandSyncFarm  uint32 = 204

	CommandTill            uint32 = 206
	CommandClear           uint32 = 208
	CommandPlant           uint32 = 210
	CommandWater           uint32 = 212
	CommandRemoveWeed      uint32 = 214
	CommandRemovePest      uint32 = 216
	CommandFertilize       uint32 = 218
	CommandHarvest         uint32 = 220
	CommandSteal           uint32 = 222
	CommandBuy             uint32 = 302
	CommandSell            uint32 = 304
	CommandFriendList      uint32 = 400
	CommandGenShareLink    uint32 = 402
	CommandAcceptInvite    uint32 = 404
	CommandRemoveFriend    uint32 = 406
	CommandAddFriendByUID  uint32 = 408
	CommandSearchUser      uint32 = 410
	CommandPetStatus       uint32 = 500
	CommandPetActivate     uint32 = 502
	CommandPetFeed         uint32 = 504
	CommandTaskList        uint32 = 600
	CommandTaskClaim       uint32 = 602
	CommandMailList        uint32 = 604
	CommandMailClaim       uint32 = 608
	CommandClaimDailyLogin uint32 = 614

	CommandFarmDelta   = wireenv.CommandFarmDelta
	CommandPlayerDelta = wireenv.CommandPlayerDelta

	JSONSubprotocol = "farm.v1.json"
)

// Envelope is the JSON representation of protocol Envelope.
// Alias keeps the public gateway API stable while sharing wireenv's codec type.
type Envelope = wireenv.Envelope

// EncodeEnvelope serializes an envelope for a single WebSocket frame.
func EncodeEnvelope(envelope Envelope) ([]byte, error) {
	encoded, err := wireenv.EncodeEnvelope(envelope)
	if err != nil {
		return nil, remapWireenvError(err)
	}
	return encoded, nil
}

// DecodeEnvelope decodes one client WebSocket frame.
// Exactly one JSON value is accepted; trailing values/garbage are rejected via wireenv.
func DecodeEnvelope(data []byte) (Envelope, error) {
	envelope, err := wireenv.DecodeEnvelope(data)
	if err != nil {
		return Envelope{}, remapWireenvError(err)
	}
	return envelope, nil
}

func remapWireenvError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), "wireenv:", "gateway:"))
}
