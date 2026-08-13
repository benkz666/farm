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

// AppendBinaryBatch appends a complete WireBatch directly to dst. It exists
// for compatibility tooling; production transports use AppendWireBatch.
func AppendBinaryBatch(dst []byte, envelopes []Envelope) ([]byte, error) {
	if len(envelopes) == 0 || len(envelopes) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: protobuf batch size must be 1..%d", MaxBatchEnvelopes)
	}
	needed := 0
	for index := range envelopes {
		needed += len(envelopes[index].Payload) + 32
	}
	dst = growBytes(dst, needed)
	for index := range envelopes {
		var err error
		dst, err = appendProtoRecord(dst, envelopes[index], false)
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
	if envelope == nil || hasTypedPayload(*envelope) {
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
	if !hasTypedPayload(envelope) {
		payload := bytes.TrimSpace(envelope.Payload)
		if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
			return nil, fmt.Errorf("wireenv: trusted protobuf payload must be a JSON object")
		}
	}
	return encodeProtoRecord(envelope, true)
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

// DecodeWireBatch decodes the public Protobuf frame without creating the
// historical JSON-compatible Envelope adapter. Gateway uses this path so the
// client payload stays strongly typed from ingress to the destination service.
func DecodeWireBatch(data []byte) ([]*publicv3.WireEnvelope, error) {
	var batch publicv3.WireBatch
	if len(data) == 0 || proto.Unmarshal(data, &batch) != nil || len(batch.Envelopes) == 0 || len(batch.Envelopes) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: invalid protobuf batch")
	}
	if len(batch.ProtoReflect().GetUnknown()) != 0 {
		return nil, fmt.Errorf("wireenv: unknown protobuf batch field")
	}
	for index, envelope := range batch.Envelopes {
		if err := validateWireEnvelope(envelope); err != nil {
			return nil, fmt.Errorf("wireenv: protobuf batch item %d: %w", index, err)
		}
	}
	return batch.Envelopes, nil
}

// EncodeWireBatch marshals complete typed public responses in one frame.
func EncodeWireBatch(envelopes []*publicv3.WireEnvelope) ([]byte, error) {
	return AppendWireBatch(nil, envelopes)
}

// AppendWireBatch appends canonical typed envelopes without crossing the
// legacy JSON-compatible adapter.
func AppendWireBatch(dst []byte, envelopes []*publicv3.WireEnvelope) ([]byte, error) {
	if len(envelopes) == 0 || len(envelopes) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: protobuf batch size must be 1..%d", MaxBatchEnvelopes)
	}
	for index, envelope := range envelopes {
		if err := validateWireEnvelope(envelope); err != nil {
			return nil, fmt.Errorf("wireenv: invalid protobuf envelope %d", index)
		}
		size := proto.Size(envelope)
		dst = protowire.AppendTag(dst, 1, protowire.BytesType)
		dst = protowire.AppendVarint(dst, uint64(size))
		var err error
		dst, err = proto.MarshalOptions{}.MarshalAppend(dst, envelope)
		if err != nil {
			return nil, fmt.Errorf("wireenv: marshal protobuf envelope %d: %w", index, err)
		}
	}
	return dst, nil
}

// EncodeWireRecord creates one frame-ready repeated-field record for the push
// coalescer. Records may be concatenated into a WireBatch without decoding.
func EncodeWireRecord(envelope *publicv3.WireEnvelope) ([]byte, error) {
	if err := validateWireEnvelope(envelope); err != nil {
		return nil, err
	}
	size := proto.Size(envelope)
	result := make([]byte, 0, size+protowire.SizeTag(1)+protowire.SizeVarint(uint64(size)))
	result = protowire.AppendTag(result, 1, protowire.BytesType)
	result = protowire.AppendVarint(result, uint64(size))
	return proto.MarshalOptions{}.MarshalAppend(result, envelope)
}

func validateWireEnvelope(envelope *publicv3.WireEnvelope) error {
	if envelope == nil || envelope.Err < 0 || len(envelope.ProtoReflect().GetUnknown()) != 0 || envelope.Payload == nil {
		return fmt.Errorf("wireenv: invalid protobuf envelope")
	}
	switch payload := envelope.Payload.(type) {
	case *publicv3.WireEnvelope_EnterFarmRequest:
		if envelope.Cmd != commandEnterFarm || payload.EnterFarmRequest == nil ||
			len(payload.EnterFarmRequest.ProtoReflect().GetUnknown()) != 0 {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_SyncFarmRequest:
		if envelope.Cmd != commandSyncFarm || payload.SyncFarmRequest == nil ||
			len(payload.SyncFarmRequest.ProtoReflect().GetUnknown()) != 0 {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_CommandRequest:
		if err := ValidateCommandRequest(envelope.Cmd, payload.CommandRequest); err != nil {
			return err
		}
	case *publicv3.WireEnvelope_EnterFarmResponse:
		if envelope.Cmd != commandEnterFarm || payload.EnterFarmResponse == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_SyncFarmResponse:
		if envelope.Cmd != commandSyncFarm || payload.SyncFarmResponse == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_CommandResponse:
		if payload.CommandResponse == nil {
			return fmt.Errorf("wireenv: missing command response")
		}
	case *publicv3.WireEnvelope_FarmDelta:
		if envelope.Cmd != CommandFarmDelta || payload.FarmDelta == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_PlayerDelta:
		if envelope.Cmd != CommandPlayerDelta || payload.PlayerDelta == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_MailNotify:
		if envelope.Cmd != CommandMailNotify || payload.MailNotify == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_SessionKick:
		if envelope.Cmd != CommandSessionKick || payload.SessionKick == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	case *publicv3.WireEnvelope_TaskNotify:
		if envelope.Cmd != CommandTaskNotify || payload.TaskNotify == nil {
			return fmt.Errorf("wireenv: payload/cmd mismatch")
		}
	default:
		return fmt.Errorf("wireenv: missing protobuf payload")
	}
	return nil
}

// EnvelopeToProto converts the remaining in-process adapter to the canonical
// public contract. New service boundaries should use WireEnvelope directly.
func EnvelopeToProto(envelope Envelope) (*publicv3.WireEnvelope, error) {
	return envelopeToProto(envelope, true)
}

// EnvelopeFromProto is kept as a narrow migration seam for internal handlers.
func EnvelopeFromProto(envelope *publicv3.WireEnvelope) (Envelope, error) {
	return envelopeFromProto(envelope)
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

// FarmSnapshotToProto converts the domain read model to its public contract.
func FarmSnapshotToProto(value farm.FarmSnapshotJSON) *publicv3.FarmSnapshot {
	return snapshotToProto(value)
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

// FarmSnapshotFromProto converts the public snapshot to the domain read model.
func FarmSnapshotFromProto(value *publicv3.FarmSnapshot) farm.FarmSnapshotJSON {
	return snapshotFromProto(value)
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
