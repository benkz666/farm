package clientwire

import (
	"bytes"
	"encoding/json"
	"fmt"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/clientjson"
	"farm/server/shared/errcode"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	BinarySubprotocol = "farm.v3.pb"
	MaxBatchEnvelopes = 64

	commandEnterFarm uint32 = 200
	commandSyncFarm  uint32 = 204

	PreparedEnterFarmResponse uint32 = 13
	PreparedSyncFarmResponse  uint32 = 14
	PreparedCommandResponse   uint32 = 17
)

type enterFarmPayload struct {
	Snapshot           farm.FarmSnapshotJSON `json:"snapshot"`
	FarmSeq            clientjson.Uint64     `json:"farm_seq"`
	ServerTime         int64                 `json:"server_time"`
	TimeProfile        string                `json:"time_profile"`
	TimeProfileMutable bool                  `json:"time_profile_mutable"`
	Relation           string                `json:"relation"`
}

type syncFarmPayload struct {
	Deltas             []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot           *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq            clientjson.Uint64      `json:"farm_seq"`
	ServerTime         int64                  `json:"server_time"`
	TimeProfile        string                 `json:"time_profile"`
	TimeProfileMutable bool                   `json:"time_profile_mutable"`
}

type enterFarmRequestPayload struct {
	OwnerUID clientjson.UID `json:"owner_uid"`
}

type syncFarmRequestPayload struct {
	OwnerUID clientjson.UID    `json:"owner_uid"`
	FromSeq  clientjson.Uint64 `json:"from_seq"`
}

// EncodeBinaryBatch writes a real Protobuf WebSocket frame. Every public
// payload uses a typed oneof arm; JSON exists only as an in-process adapter for
// legacy Go handlers and tests.
func EncodeBinaryBatch(envelopes []Envelope) ([]byte, error) {
	return AppendBinaryBatch(nil, envelopes)
}

// AppendBinaryBatch appends a complete WireBatch directly to dst. Prepared
// Farm responses are written into their final frame in one pass: the cached
// snapshot and the small Gateway suffix are never materialized as an
// intermediate message or repeated-field record.
func AppendBinaryBatch(dst []byte, envelopes []Envelope) ([]byte, error) {
	if len(envelopes) == 0 || len(envelopes) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: protobuf batch size must be 1..%d", MaxBatchEnvelopes)
	}
	needed := 0
	for index := range envelopes {
		if len(envelopes[index].PreparedPayload) > 0 {
			needed += len(envelopes[index].PreparedPayload) + len(envelopes[index].PreparedSuffix) + 32
		} else {
			needed += len(envelopes[index].Payload) + 32
		}
	}
	dst = growBytes(dst, needed)
	for index := range envelopes {
		var err error
		if len(envelopes[index].PreparedPayload) > 0 {
			dst, err = appendPreparedProtoRecord(dst, envelopes[index])
		} else {
			dst, err = appendProtoRecord(dst, envelopes[index], false)
		}
		if err != nil {
			return nil, fmt.Errorf("wireenv: protobuf batch item %d: %w", index, err)
		}
	}
	return dst, nil
}

// EncodeBinaryRecord returns one encoded repeated-field record. Multiple
// immutable records can be concatenated into WireBatch without decoding them.
func EncodeBinaryRecord(envelope Envelope) ([]byte, error) {
	if !hasTypedPayload(envelope) {
		if err := ValidatePayloadObject(envelope.Payload); err != nil {
			return nil, err
		}
	}
	return encodeProtoRecord(envelope, false)
}

func hasTypedPayload(envelope Envelope) bool {
	return envelope.EnterFarmRequest != nil || envelope.SyncFarmRequest != nil ||
		envelope.CommandRequest != nil || envelope.CommandResponse != nil ||
		envelope.FarmDelta != nil || envelope.PlayerDelta != nil ||
		envelope.MailNotify != nil || envelope.SessionKick != nil || envelope.TaskNotify != nil
}

// PrepareCommandResponse converts an existing handler JSON result into the
// typed public response once. Hot handlers can install CommandResponse
// directly and skip this compatibility conversion.
func PrepareCommandResponse(envelope *Envelope) error {
	if envelope == nil || len(envelope.PreparedPayload) != 0 || hasTypedPayload(*envelope) {
		return nil
	}
	if envelope.Err != errcode.OK {
		envelope.CommandResponse = &publicv3.CommandResponse{}
		return nil
	}
	if envelope.Cmd == commandEnterFarm || envelope.Cmd == commandSyncFarm {
		return nil
	}
	response, err := CommandResponseFromJSON(envelope.Cmd, envelope.Payload)
	if err != nil {
		return err
	}
	envelope.CommandResponse = response
	return nil
}

func EncodeTrustedBinaryRecord(envelope Envelope) ([]byte, error) {
	if len(envelope.PreparedPayload) > 0 {
		return encodePreparedProtoRecord(envelope)
	}
	if !hasTypedPayload(envelope) {
		payload := bytes.TrimSpace(envelope.Payload)
		if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
			return nil, fmt.Errorf("wireenv: trusted protobuf payload must be a JSON object")
		}
	}
	return encodeProtoRecord(envelope, true)
}

func encodePreparedProtoRecord(envelope Envelope) ([]byte, error) {
	return appendPreparedProtoRecord(nil, envelope)
}

func appendPreparedProtoRecord(dst []byte, envelope Envelope) ([]byte, error) {
	if !IsPreparedResponseField(envelope.PreparedField) {
		return nil, fmt.Errorf("wireenv: invalid prepared payload field %d", envelope.PreparedField)
	}
	if envelope.Err != errcode.OK || len(envelope.PreparedPayload) == 0 {
		return nil, fmt.Errorf("wireenv: invalid prepared payload")
	}
	messageSize := protowire.SizeTag(1) + protowire.SizeVarint(uint64(envelope.Cmd))
	if envelope.Cmd == 0 {
		messageSize = 0
	}
	if envelope.ClientSeq != 0 {
		messageSize += protowire.SizeTag(2) + protowire.SizeVarint(uint64(envelope.ClientSeq))
	}
	payloadSize := len(envelope.PreparedPayload) + len(envelope.PreparedSuffix)
	messageSize += protowire.SizeTag(protowire.Number(envelope.PreparedField)) + protowire.SizeBytes(payloadSize)
	dst = protowire.AppendTag(dst, 1, protowire.BytesType)
	dst = protowire.AppendVarint(dst, uint64(messageSize))
	if envelope.Cmd != 0 {
		dst = protowire.AppendTag(dst, 1, protowire.VarintType)
		dst = protowire.AppendVarint(dst, uint64(envelope.Cmd))
	}
	if envelope.ClientSeq != 0 {
		dst = protowire.AppendTag(dst, 2, protowire.VarintType)
		dst = protowire.AppendVarint(dst, uint64(envelope.ClientSeq))
	}
	dst = protowire.AppendTag(dst, protowire.Number(envelope.PreparedField), protowire.BytesType)
	dst = protowire.AppendVarint(dst, uint64(payloadSize))
	dst = append(dst, envelope.PreparedPayload...)
	dst = append(dst, envelope.PreparedSuffix...)
	return dst, nil
}

// IsPreparedResponseField reports the server-direction oneof bodies that may
// be forwarded as already-marshaled Protobuf through Gateway.
func IsPreparedResponseField(field uint32) bool {
	return field == PreparedEnterFarmResponse ||
		field == PreparedSyncFarmResponse ||
		field == PreparedCommandResponse
}

// MarshalCommandResponsePayload prepares the hot action response once in Farm.
// Gateway embeds these bytes directly as WireEnvelope.command_response.
func MarshalCommandResponsePayload(response *publicv3.CommandResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("wireenv: nil command response")
	}
	return proto.Marshal(response)
}

func encodeProtoRecord(envelope Envelope, trusted bool) ([]byte, error) {
	return appendProtoRecord(nil, envelope, trusted)
}

func appendProtoRecord(dst []byte, envelope Envelope, trusted bool) ([]byte, error) {
	wire, err := envelopeToProto(envelope, trusted)
	if err != nil {
		return nil, err
	}
	size := proto.Size(wire)
	dst = protowire.AppendTag(dst, 1, protowire.BytesType)
	dst = protowire.AppendVarint(dst, uint64(size))
	dst, err = proto.MarshalOptions{}.MarshalAppend(dst, wire)
	if err != nil {
		return nil, fmt.Errorf("wireenv: marshal protobuf envelope: %w", err)
	}
	return dst, nil
}

func FrameBinaryRecords(records [][]byte) ([]byte, error) {
	return AppendBinaryRecords(nil, records)
}

// AppendBinaryRecords concatenates immutable, frame-ready records directly
// into dst so Gateway push flushes can reuse their final WebSocket buffer.
func AppendBinaryRecords(dst []byte, records [][]byte) ([]byte, error) {
	if len(records) == 0 || len(records) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: protobuf batch size must be 1..%d", MaxBatchEnvelopes)
	}
	size := 0
	for index, record := range records {
		if len(record) == 0 || record[0] != 0x0a {
			return nil, fmt.Errorf("wireenv: invalid protobuf record %d", index)
		}
		size += len(record)
	}
	dst = growBytes(dst, size)
	for _, record := range records {
		dst = append(dst, record...)
	}
	return dst, nil
}

func growBytes(dst []byte, additional int) []byte {
	if additional <= cap(dst)-len(dst) {
		return dst
	}
	capacity := len(dst) + additional
	if doubled := cap(dst) * 2; doubled > capacity {
		capacity = doubled
	}
	result := make([]byte, len(dst), capacity)
	copy(result, dst)
	return result
}

func DecodeBinaryBatch(data []byte) ([]Envelope, error) {
	var batch publicv3.WireBatch
	if len(data) == 0 || proto.Unmarshal(data, &batch) != nil || len(batch.Envelopes) == 0 || len(batch.Envelopes) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: invalid protobuf batch")
	}
	if len(batch.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("wireenv: unknown protobuf batch field")
	}
	result := make([]Envelope, 0, len(batch.Envelopes))
	for index, wire := range batch.Envelopes {
		envelope, err := envelopeFromProto(wire)
		if err != nil {
			return nil, fmt.Errorf("wireenv: protobuf batch item %d: %w", index, err)
		}
		result = append(result, envelope)
	}
	return result, nil
}

func envelopeToProto(envelope Envelope, trusted bool) (*publicv3.WireEnvelope, error) {
	if envelope.Err < 0 {
		return nil, fmt.Errorf("negative err")
	}
	if !trusted && !hasTypedPayload(envelope) {
		if err := ValidatePayloadObject(envelope.Payload); err != nil {
			return nil, err
		}
	}
	wire := &publicv3.WireEnvelope{Cmd: envelope.Cmd, ClientSeq: envelope.ClientSeq, Err: int32(envelope.Err)}
	if envelope.EnterFarmRequest != nil {
		wire.Payload = &publicv3.WireEnvelope_EnterFarmRequest{EnterFarmRequest: envelope.EnterFarmRequest}
		return wire, nil
	}
	if envelope.SyncFarmRequest != nil {
		wire.Payload = &publicv3.WireEnvelope_SyncFarmRequest{SyncFarmRequest: envelope.SyncFarmRequest}
		return wire, nil
	}
	if envelope.CommandRequest != nil {
		wire.Payload = &publicv3.WireEnvelope_CommandRequest{CommandRequest: envelope.CommandRequest}
		return wire, nil
	}
	if envelope.CommandResponse != nil {
		wire.Payload = &publicv3.WireEnvelope_CommandResponse{CommandResponse: envelope.CommandResponse}
		return wire, nil
	}
	if envelope.FarmDelta != nil {
		wire.Payload = &publicv3.WireEnvelope_FarmDelta{FarmDelta: envelope.FarmDelta}
		return wire, nil
	}
	if envelope.PlayerDelta != nil {
		wire.Payload = &publicv3.WireEnvelope_PlayerDelta{PlayerDelta: envelope.PlayerDelta}
		return wire, nil
	}
	if envelope.MailNotify != nil {
		wire.Payload = &publicv3.WireEnvelope_MailNotify{MailNotify: envelope.MailNotify}
		return wire, nil
	}
	if envelope.SessionKick != nil {
		wire.Payload = &publicv3.WireEnvelope_SessionKick{SessionKick: envelope.SessionKick}
		return wire, nil
	}
	if envelope.TaskNotify != nil {
		wire.Payload = &publicv3.WireEnvelope_TaskNotify{TaskNotify: envelope.TaskNotify}
		return wire, nil
	}
	if envelope.Err != errcode.OK {
		wire.Payload = &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}}
		return wire, nil
	}
	if envelope.Err == errcode.OK {
		switch envelope.Cmd {
		case commandEnterFarm:
			if bytes.Contains(envelope.Payload, []byte(`"snapshot"`)) {
				var payload enterFarmPayload
				if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
					return nil, err
				}
				wire.Payload = &publicv3.WireEnvelope_EnterFarmResponse{EnterFarmResponse: enterFarmResponseToProto(payload)}
				return wire, nil
			}
			var payload enterFarmRequestPayload
			if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
				return nil, err
			}
			wire.Payload = &publicv3.WireEnvelope_EnterFarmRequest{EnterFarmRequest: &publicv3.EnterFarmRequest{OwnerUid: uint64(payload.OwnerUID)}}
			return wire, nil
		case commandSyncFarm:
			if bytes.Contains(envelope.Payload, []byte(`"farm_seq"`)) {
				var payload syncFarmPayload
				if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
					return nil, err
				}
				wire.Payload = &publicv3.WireEnvelope_SyncFarmResponse{SyncFarmResponse: syncFarmResponseToProto(payload)}
				return wire, nil
			}
			var payload syncFarmRequestPayload
			if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
				return nil, err
			}
			wire.Payload = &publicv3.WireEnvelope_SyncFarmRequest{SyncFarmRequest: &publicv3.SyncFarmRequest{OwnerUid: uint64(payload.OwnerUID), FromSeq: uint64(payload.FromSeq)}}
			return wire, nil
		case CommandFarmDelta:
			var delta farm.FarmDelta
			if err := json.Unmarshal(envelope.Payload, &delta); err != nil {
				return nil, err
			}
			wire.Payload = &publicv3.WireEnvelope_FarmDelta{FarmDelta: FarmDeltaToProto(delta)}
			return wire, nil
		}
	}
	request, err := CommandRequestFromJSON(envelope.Cmd, envelope.Payload)
	if err == nil {
		wire.Payload = &publicv3.WireEnvelope_CommandRequest{CommandRequest: request}
		return wire, nil
	}
	response, responseErr := CommandResponseFromJSON(envelope.Cmd, envelope.Payload)
	if responseErr != nil {
		return nil, err
	}
	wire.Payload = &publicv3.WireEnvelope_CommandResponse{CommandResponse: response}
	return wire, nil
}

func envelopeFromProto(wire *publicv3.WireEnvelope) (Envelope, error) {
	if wire == nil || wire.Err < 0 || len(wire.ProtoReflect().GetUnknown()) != 0 {
		return Envelope{}, fmt.Errorf("invalid protobuf envelope")
	}
	envelope := Envelope{Cmd: wire.Cmd, ClientSeq: wire.ClientSeq, Err: errcode.Code(wire.Err)}
	var payload any
	switch value := wire.Payload.(type) {
	case *publicv3.WireEnvelope_EnterFarmRequest:
		if wire.Cmd != commandEnterFarm || value.EnterFarmRequest == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		envelope.EnterFarmRequest = value.EnterFarmRequest
		envelope.Payload = json.RawMessage(`{}`)
		return envelope, nil
	case *publicv3.WireEnvelope_SyncFarmRequest:
		if wire.Cmd != commandSyncFarm || value.SyncFarmRequest == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		envelope.SyncFarmRequest = value.SyncFarmRequest
		envelope.Payload = json.RawMessage(`{}`)
		return envelope, nil
	case *publicv3.WireEnvelope_EnterFarmResponse:
		if wire.Cmd != commandEnterFarm || value.EnterFarmResponse == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		payload = enterFarmResponseFromProto(value.EnterFarmResponse)
		envelope.ServerPayload = true
	case *publicv3.WireEnvelope_SyncFarmResponse:
		if wire.Cmd != commandSyncFarm || value.SyncFarmResponse == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		payload = syncFarmResponseFromProto(value.SyncFarmResponse)
		envelope.ServerPayload = true
	case *publicv3.WireEnvelope_FarmDelta:
		if wire.Cmd != CommandFarmDelta || value.FarmDelta == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		payload = FarmDeltaFromProto(value.FarmDelta)
		envelope.FarmDelta = value.FarmDelta
		envelope.ServerPayload = true
	case *publicv3.WireEnvelope_CommandRequest:
		if value.CommandRequest == nil {
			return Envelope{}, fmt.Errorf("missing command request")
		}
		if err := ValidateCommandRequest(wire.Cmd, value.CommandRequest); err != nil {
			return Envelope{}, err
		}
		envelope.CommandRequest = value.CommandRequest
		envelope.Payload = json.RawMessage(`{}`)
		return envelope, nil
	case *publicv3.WireEnvelope_CommandResponse:
		if value.CommandResponse == nil {
			return Envelope{}, fmt.Errorf("missing command response")
		}
		envelope.CommandResponse = value.CommandResponse
		envelope.ServerPayload = true
		if envelope.Err != errcode.OK {
			envelope.Payload = json.RawMessage(`{}`)
			return envelope, nil
		}
		encoded, err := CommandResponseToJSON(wire.Cmd, value.CommandResponse)
		if err != nil {
			return Envelope{}, err
		}
		envelope.Payload = encoded
		return envelope, nil
	case *publicv3.WireEnvelope_PlayerDelta:
		if wire.Cmd != CommandPlayerDelta || value.PlayerDelta == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		envelope.PlayerDelta = value.PlayerDelta
		envelope.ServerPayload = true
		payload = PlayerDeltaFromProto(value.PlayerDelta)
	case *publicv3.WireEnvelope_MailNotify:
		if wire.Cmd != CommandMailNotify || value.MailNotify == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		envelope.MailNotify = value.MailNotify
		envelope.ServerPayload = true
		payload = struct {
			Kind string `json:"kind"`
		}{Kind: value.MailNotify.Kind}
	case *publicv3.WireEnvelope_SessionKick:
		if wire.Cmd != CommandSessionKick || value.SessionKick == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		envelope.SessionKick = value.SessionKick
		envelope.ServerPayload = true
		payload = struct {
			Reason int32 `json:"reason"`
		}{Reason: value.SessionKick.Reason}
	case *publicv3.WireEnvelope_TaskNotify:
		if wire.Cmd != CommandTaskNotify || value.TaskNotify == nil {
			return Envelope{}, fmt.Errorf("payload/cmd mismatch")
		}
		envelope.TaskNotify = value.TaskNotify
		envelope.ServerPayload = true
		payload = taskJSONFromProto(value.TaskNotify)
	default:
		return Envelope{}, fmt.Errorf("missing protobuf payload")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	envelope.Payload = encoded
	return envelope, nil
}

func enterFarmResponseToProto(value enterFarmPayload) *publicv3.EnterFarmResponse {
	return &publicv3.EnterFarmResponse{Snapshot: snapshotToProto(value.Snapshot), FarmSeq: uint64(value.FarmSeq), ServerTime: value.ServerTime, TimeProfile: value.TimeProfile, TimeProfileMutable: value.TimeProfileMutable, Relation: value.Relation}
}

func enterFarmResponseFromProto(value *publicv3.EnterFarmResponse) enterFarmPayload {
	return enterFarmPayload{Snapshot: snapshotFromProto(value.Snapshot), FarmSeq: clientjson.Uint64(value.FarmSeq), ServerTime: value.ServerTime, TimeProfile: value.TimeProfile, TimeProfileMutable: value.TimeProfileMutable, Relation: value.Relation}
}

func syncFarmResponseToProto(value syncFarmPayload) *publicv3.SyncFarmResponse {
	result := &publicv3.SyncFarmResponse{FarmSeq: uint64(value.FarmSeq), ServerTime: value.ServerTime, TimeProfile: value.TimeProfile, TimeProfileMutable: value.TimeProfileMutable}
	if value.Snapshot != nil {
		result.Snapshot = snapshotToProto(*value.Snapshot)
	}
	for _, delta := range value.Deltas {
		result.Deltas = append(result.Deltas, FarmDeltaToProto(delta))
	}
	return result
}

func syncFarmResponseFromProto(value *publicv3.SyncFarmResponse) syncFarmPayload {
	result := syncFarmPayload{FarmSeq: clientjson.Uint64(value.FarmSeq), ServerTime: value.ServerTime, TimeProfile: value.TimeProfile, TimeProfileMutable: value.TimeProfileMutable}
	if value.Snapshot != nil {
		snapshot := snapshotFromProto(value.Snapshot)
		result.Snapshot = &snapshot
	}
	for _, delta := range value.Deltas {
		result.Deltas = append(result.Deltas, FarmDeltaFromProto(delta))
	}
	return result
}

func snapshotToProto(value farm.FarmSnapshotJSON) *publicv3.FarmSnapshot {
	result := &publicv3.FarmSnapshot{OwnerUid: value.OwnerUID, Nickname: value.Nickname, Level: uint32(value.Level), Exp: value.Exp, Coin: value.Coin, UnlockedPlots: uint32(value.UnlockedPlots), Bag: value.Bag, Warehouse: value.Warehouse, GuardDog: &publicv3.GuardDog{ActiveDog: uint32(value.GuardDog.ActiveDog), BowlEmptyAt: value.GuardDog.BowlEmptyAt}}
	for _, plot := range value.Plots {
		result.Plots = append(result.Plots, plotToProto(plot))
	}
	return result
}

func snapshotFromProto(value *publicv3.FarmSnapshot) farm.FarmSnapshotJSON {
	if value == nil {
		return farm.FarmSnapshotJSON{}
	}
	result := farm.FarmSnapshotJSON{OwnerUID: value.OwnerUid, Nickname: value.Nickname, Level: uint16(value.Level), Exp: value.Exp, Coin: value.Coin, UnlockedPlots: uint8(value.UnlockedPlots), Bag: value.Bag, Warehouse: value.Warehouse}
	if value.GuardDog != nil {
		result.GuardDog = farm.GuardDogSnapshot{ActiveDog: farm.DogType(value.GuardDog.ActiveDog), BowlEmptyAt: value.GuardDog.BowlEmptyAt}
	}
	for _, plot := range value.Plots {
		result.Plots = append(result.Plots, plotFromProto(plot))
	}
	return result
}

func plotToProto(value farm.PlotSnapshot) *publicv3.PlotSnapshot {
	return &publicv3.PlotSnapshot{Index: uint32(value.Index), State: uint32(value.State), CropId: uint32(value.CropID), SeasonIndex: uint32(value.SeasonIndex), SeasonTotal: uint32(value.SeasonTotal), SeasonStartAt: value.SeasonStartAt, MatureAt: value.MatureAt, SeasonDuration: value.SeasonDuration, FinalYield: uint32(value.FinalYield), LastSettleAt: value.LastSettleAt, LastWaterAt: value.LastWaterAt, WeedSince: value.WeedSince, PestSince: value.PestSince, Health: uint32(value.Health), StolenCount: uint32(value.StolenCount), FertMask: uint32(value.FertMask)}
}

func plotFromProto(value *publicv3.PlotSnapshot) farm.PlotSnapshot {
	if value == nil {
		return farm.PlotSnapshot{}
	}
	return farm.PlotSnapshot{Index: uint8(value.Index), State: uint8(value.State), CropID: uint16(value.CropId), SeasonIndex: uint8(value.SeasonIndex), SeasonTotal: uint8(value.SeasonTotal), SeasonStartAt: value.SeasonStartAt, MatureAt: value.MatureAt, SeasonDuration: value.SeasonDuration, FinalYield: uint16(value.FinalYield), LastSettleAt: value.LastSettleAt, LastWaterAt: value.LastWaterAt, WeedSince: value.WeedSince, PestSince: value.PestSince, Health: uint8(value.Health), StolenCount: uint16(value.StolenCount), FertMask: uint8(value.FertMask)}
}

func FarmDeltaToProto(value farm.FarmDelta) *publicv3.FarmDelta {
	result := &publicv3.FarmDelta{OwnerUid: value.OwnerUID, FarmSeq: value.FarmSeq, ActorUid: value.ActorUID, Action: value.Action}
	for _, plot := range value.Plots {
		result.Plots = append(result.Plots, plotToProto(farm.PlotSnapshot(plot)))
	}
	if value.GuardDog != nil {
		result.GuardDog = &publicv3.GuardDog{ActiveDog: uint32(value.GuardDog.ActiveDog), BowlEmptyAt: value.GuardDog.BowlEmptyAt}
	}
	return result
}

func FarmDeltaFromProto(value *publicv3.FarmDelta) farm.FarmDelta {
	if value == nil {
		return farm.FarmDelta{}
	}
	result := farm.FarmDelta{OwnerUID: value.OwnerUid, FarmSeq: value.FarmSeq, ActorUID: value.ActorUid, Action: value.Action}
	for _, plot := range value.Plots {
		result.Plots = append(result.Plots, farm.PlotChange(plotFromProto(plot)))
	}
	if value.GuardDog != nil {
		result.GuardDog = &farm.GuardDogSnapshot{ActiveDog: farm.DogType(value.GuardDog.ActiveDog), BowlEmptyAt: value.GuardDog.BowlEmptyAt}
	}
	return result
}

// MarshalFarmSnapshotPayload caches the typed FarmSnapshot submessage at the
// Actor version boundary. The returned bytes are immutable.
func MarshalFarmSnapshotPayload(snapshot farm.FarmSnapshotJSON) ([]byte, error) {
	return proto.Marshal(snapshotToProto(snapshot))
}

// MarshalEnterFarmResponsePayload embeds an already-marshaled snapshot without
// decoding it. Gateway-owned fields are appended later.
func MarshalEnterFarmResponsePayload(snapshot []byte, farmSeq uint64, serverTime int64, timeProfile string) ([]byte, error) {
	if len(snapshot) == 0 {
		return nil, fmt.Errorf("wireenv: invalid EnterFarm payload")
	}
	result := make([]byte, 0, len(snapshot)+len(timeProfile)+32)
	result = protowire.AppendTag(result, 1, protowire.BytesType)
	result = protowire.AppendBytes(result, snapshot)
	result = protowire.AppendTag(result, 2, protowire.VarintType)
	result = protowire.AppendVarint(result, farmSeq)
	result = protowire.AppendTag(result, 3, protowire.VarintType)
	result = protowire.AppendVarint(result, uint64(serverTime))
	if timeProfile != "" {
		result = protowire.AppendTag(result, 4, protowire.BytesType)
		result = protowire.AppendString(result, timeProfile)
	}
	return result, nil
}

func MarshalSyncFarmCaughtUpPayload(farmSeq uint64, serverTime int64, timeProfile string, mutable bool) ([]byte, error) {
	result := make([]byte, 0, len(timeProfile)+32)
	result = protowire.AppendTag(result, 3, protowire.VarintType)
	result = protowire.AppendVarint(result, farmSeq)
	result = protowire.AppendTag(result, 4, protowire.VarintType)
	result = protowire.AppendVarint(result, uint64(serverTime))
	if timeProfile != "" {
		result = protowire.AppendTag(result, 5, protowire.BytesType)
		result = protowire.AppendString(result, timeProfile)
	}
	if mutable {
		result = protowire.AppendTag(result, 6, protowire.VarintType)
		result = protowire.AppendVarint(result, 1)
	}
	return result, nil
}

func MarshalSyncFarmSnapshotPayload(snapshot []byte, farmSeq uint64, serverTime int64, timeProfile string) ([]byte, error) {
	if len(snapshot) == 0 {
		return nil, fmt.Errorf("wireenv: invalid SyncFarm snapshot payload")
	}
	result := make([]byte, 0, len(snapshot)+len(timeProfile)+32)
	result = protowire.AppendTag(result, 2, protowire.BytesType)
	result = protowire.AppendBytes(result, snapshot)
	result = protowire.AppendTag(result, 3, protowire.VarintType)
	result = protowire.AppendVarint(result, farmSeq)
	result = protowire.AppendTag(result, 4, protowire.VarintType)
	result = protowire.AppendVarint(result, uint64(serverTime))
	if timeProfile != "" {
		result = protowire.AppendTag(result, 5, protowire.BytesType)
		result = protowire.AppendString(result, timeProfile)
	}
	return result, nil
}

func AppendEnterFarmGatewayFields(payload []byte, mutable bool, relation string) ([]byte, error) {
	if len(payload) == 0 || relation == "" {
		return nil, fmt.Errorf("wireenv: invalid Gateway EnterFarm fields")
	}
	result := make([]byte, 0, len(payload)+len(relation)+8)
	result = append(result, payload...)
	if mutable {
		result = protowire.AppendTag(result, 5, protowire.VarintType)
		result = protowire.AppendVarint(result, 1)
	}
	result = protowire.AppendTag(result, 6, protowire.BytesType)
	result = protowire.AppendString(result, relation)
	return result, nil
}

// MarshalEnterFarmGatewaySuffix encodes only Gateway-owned fields. The suffix
// is concatenated with Farm's immutable prepared payload while writing the
// final frame, avoiding a full snapshot copy inside Gateway.
func MarshalEnterFarmGatewaySuffix(mutable bool, relation string) ([]byte, error) {
	if relation == "" {
		return nil, fmt.Errorf("wireenv: invalid Gateway EnterFarm fields")
	}
	result := make([]byte, 0, len(relation)+8)
	if mutable {
		result = protowire.AppendTag(result, 5, protowire.VarintType)
		result = protowire.AppendVarint(result, 1)
	}
	result = protowire.AppendTag(result, 6, protowire.BytesType)
	result = protowire.AppendString(result, relation)
	return result, nil
}

func AppendSyncFarmGatewayFields(payload []byte, mutable bool) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("wireenv: invalid Gateway SyncFarm fields")
	}
	result := append([]byte(nil), payload...)
	if mutable {
		result = protowire.AppendTag(result, 6, protowire.VarintType)
		result = protowire.AppendVarint(result, 1)
	}
	return result, nil
}

func MarshalSyncFarmGatewaySuffix(mutable bool) []byte {
	if !mutable {
		return nil
	}
	result := make([]byte, 0, 2)
	result = protowire.AppendTag(result, 6, protowire.VarintType)
	return protowire.AppendVarint(result, 1)
}
