// Package gateway implements the HTTP and JSON WebSocket boundary.
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"

	"farm/server/internal/pkgerr"
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

	CommandFarmDelta uint32 = 9000

	JSONSubprotocol = "farm.v1.json"
)

// Envelope is the JSON representation of protocol Envelope.
// Payload must be a JSON object; protocol messages never use scalar payloads.
type Envelope struct {
	Cmd       uint32          `json:"cmd"`
	ClientSeq uint32          `json:"client_seq"`
	Err       pkgerr.Code     `json:"err"`
	Payload   json.RawMessage `json:"payload"`
}

// EncodeEnvelope serializes an envelope for a single WebSocket frame.
func EncodeEnvelope(envelope Envelope) ([]byte, error) {
	if err := validatePayload(envelope.Payload); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("gateway: encode envelope: %w", err)
	}
	return encoded, nil
}

// DecodeEnvelope decodes one client WebSocket frame.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("gateway: decode envelope: %w", err)
	}
	if decoder.More() {
		return Envelope{}, fmt.Errorf("gateway: decode envelope: trailing JSON value")
	}
	if err := validatePayload(envelope.Payload); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func validatePayload(payload json.RawMessage) error {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return fmt.Errorf("gateway: envelope payload must be a JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return fmt.Errorf("gateway: invalid envelope payload: %w", err)
	}
	return nil
}
