package clientwire

import (
	"fmt"

	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/errcode"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	PreparedEnterFarmResponse uint32 = 13
	PreparedSyncFarmResponse  uint32 = 14
	PreparedCommandResponse   uint32 = 17
)

// WireResponse is Gateway's transport-only response carrier. Envelope is the
// canonical path. PreparedPayload is the already-marshaled public oneof body
// used by trusted Farm/Social responses to avoid a decode/re-encode cycle.
type WireResponse struct {
	Envelope        *publicv3.WireEnvelope
	PreparedPayload []byte
	PreparedField   uint32
}

func (response *WireResponse) GetErr() int32 {
	if response == nil || response.Envelope == nil {
		return int32(errcode.Internal)
	}
	return response.Envelope.Err
}

func (response *WireResponse) GetCommandResponse() *publicv3.CommandResponse {
	if response == nil || response.Envelope == nil {
		return nil
	}
	return response.Envelope.GetCommandResponse()
}

func (response *WireResponse) GetEnvelope() *publicv3.WireEnvelope {
	if response == nil {
		return nil
	}
	return response.Envelope
}

// AppendWireResponses writes typed and prepared responses into one WireBatch.
// A prepared response is never decoded in Gateway: only its envelope metadata
// and legal oneof field number are inspected.
func AppendWireResponses(dst []byte, responses []*WireResponse) ([]byte, error) {
	if len(responses) == 0 || len(responses) > MaxBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: protobuf batch size must be 1..%d", MaxBatchEnvelopes)
	}
	needed := 0
	for _, response := range responses {
		if response == nil || response.Envelope == nil {
			return nil, fmt.Errorf("wireenv: nil response")
		}
		if len(response.PreparedPayload) != 0 {
			needed += len(response.PreparedPayload) + 32
		} else {
			// Do not call proto.Size here: appendTypedWireRecord needs the exact
			// size once, and traversing a large fallback snapshot twice costs more
			// than allowing MarshalAppend to grow the pooled buffer when needed.
			needed += 32
		}
	}
	dst = growBytes(dst, needed)
	for index, response := range responses {
		var err error
		if len(response.PreparedPayload) != 0 {
			dst, err = appendPreparedWireRecord(dst, response)
		} else {
			dst, err = appendTypedWireRecord(dst, response.Envelope)
		}
		if err != nil {
			return nil, fmt.Errorf("wireenv: response %d: %w", index, err)
		}
	}
	return dst, nil
}

func appendTypedWireRecord(dst []byte, envelope *publicv3.WireEnvelope) ([]byte, error) {
	if err := validateWireEnvelope(envelope); err != nil {
		return nil, err
	}
	size := proto.Size(envelope)
	dst = protowire.AppendTag(dst, 1, protowire.BytesType)
	dst = protowire.AppendVarint(dst, uint64(size))
	return proto.MarshalOptions{}.MarshalAppend(dst, envelope)
}

func appendPreparedWireRecord(dst []byte, response *WireResponse) ([]byte, error) {
	envelope := response.Envelope
	if envelope == nil || envelope.Cmd == 0 || envelope.ClientSeq == 0 ||
		envelope.Err != int32(errcode.OK) || envelope.Payload != nil ||
		len(response.PreparedPayload) == 0 ||
		!IsPreparedResponseFieldForCommand(response.PreparedField, envelope.Cmd) {
		return nil, fmt.Errorf("wireenv: invalid prepared response")
	}
	messageSize := protowire.SizeTag(1) + protowire.SizeVarint(uint64(envelope.Cmd))
	messageSize += protowire.SizeTag(2) + protowire.SizeVarint(uint64(envelope.ClientSeq))
	messageSize += protowire.SizeTag(protowire.Number(response.PreparedField)) + protowire.SizeBytes(len(response.PreparedPayload))

	dst = protowire.AppendTag(dst, 1, protowire.BytesType)
	dst = protowire.AppendVarint(dst, uint64(messageSize))
	dst = protowire.AppendTag(dst, 1, protowire.VarintType)
	dst = protowire.AppendVarint(dst, uint64(envelope.Cmd))
	dst = protowire.AppendTag(dst, 2, protowire.VarintType)
	dst = protowire.AppendVarint(dst, uint64(envelope.ClientSeq))
	dst = protowire.AppendTag(dst, protowire.Number(response.PreparedField), protowire.BytesType)
	dst = protowire.AppendBytes(dst, response.PreparedPayload)
	return dst, nil
}

func IsPreparedResponseField(field uint32) bool {
	return field == PreparedEnterFarmResponse || field == PreparedSyncFarmResponse || field == PreparedCommandResponse
}

func IsPreparedResponseFieldForCommand(field, command uint32) bool {
	switch field {
	case PreparedEnterFarmResponse:
		return command == commandEnterFarm
	case PreparedSyncFarmResponse:
		return command == commandSyncFarm
	case PreparedCommandResponse:
		return command != commandEnterFarm && command != commandSyncFarm
	default:
		return false
	}
}

// MarshalEnterFarmResponsePayload embeds the Actor-cached snapshot bytes in a
// complete public response without decoding or traversing the snapshot again.
func MarshalEnterFarmResponsePayload(
	snapshot []byte,
	farmSeq uint64,
	serverTime int64,
	timeProfile string,
	mutable bool,
	relation string,
) ([]byte, error) {
	if len(snapshot) == 0 || relation == "" {
		return nil, fmt.Errorf("wireenv: invalid EnterFarm payload")
	}
	result := make([]byte, 0, len(snapshot)+len(timeProfile)+len(relation)+40)
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
	if mutable {
		result = protowire.AppendTag(result, 5, protowire.VarintType)
		result = protowire.AppendVarint(result, 1)
	}
	result = protowire.AppendTag(result, 6, protowire.BytesType)
	result = protowire.AppendString(result, relation)
	return result, nil
}

func MarshalSyncFarmCaughtUpPayload(farmSeq uint64, serverTime int64, timeProfile string, mutable bool) []byte {
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
	return result
}

func MarshalSyncFarmSnapshotPayload(
	snapshot []byte,
	farmSeq uint64,
	serverTime int64,
	timeProfile string,
	mutable bool,
) ([]byte, error) {
	if len(snapshot) == 0 {
		return nil, fmt.Errorf("wireenv: invalid SyncFarm snapshot payload")
	}
	result := make([]byte, 0, len(snapshot)+len(timeProfile)+36)
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
	if mutable {
		result = protowire.AppendTag(result, 6, protowire.VarintType)
		result = protowire.AppendVarint(result, 1)
	}
	return result, nil
}

func MarshalSyncFarmResponsePayload(response *publicv3.SyncFarmResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("wireenv: nil SyncFarm response")
	}
	return proto.Marshal(response)
}

func MarshalCommandResponsePayload(response *publicv3.CommandResponse) ([]byte, error) {
	if response == nil {
		return nil, fmt.Errorf("wireenv: nil command response")
	}
	return proto.Marshal(response)
}

// VisitorSafeFarmSnapshotProto projects only fields visible to a visitor. It
// reuses immutable plot/dog children and avoids a proto -> domain -> proto
// round trip on friend Enter/Sync.
func VisitorSafeFarmSnapshotProto(full *publicv3.FarmSnapshot) *publicv3.FarmSnapshot {
	if full == nil {
		return nil
	}
	return &publicv3.FarmSnapshot{
		OwnerUid: full.OwnerUid, Nickname: full.Nickname, Level: full.Level,
		UnlockedPlots: full.UnlockedPlots, Plots: full.Plots, GuardDog: full.GuardDog,
	}
}
