// Package clientwire encodes client-visible WebSocket Envelopes for internal push.
// It sits below gateway and farmrpc so both can share one codec without an import cycle.
package clientwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"farm/server/domain/farm"
	"farm/server/shared/errcode"
)

// Protocol push command numbers (docs/design/protocol.md §5).
const (
	CommandFarmDelta   uint32 = 9000
	CommandPlayerDelta uint32 = 9002
	CommandMailNotify  uint32 = 9004 // 新邮件/邻里申请等个人通知（只推提示，不推全文）
	CommandSessionKick uint32 = 9006
	CommandTaskNotify  uint32 = 9008
)

// Envelope is the public JSON shape written on the WebSocket wire.
type Envelope struct {
	Cmd       uint32          `json:"cmd"`
	ClientSeq uint32          `json:"client_seq"`
	Err       errcode.Code    `json:"err"`
	Payload   json.RawMessage `json:"payload"`
	// PreparedPayload is an already-marshaled typed Protobuf oneof body. It is
	// never accepted from clients and lets Farm snapshots cross Gateway without
	// a JSON decode/re-encode. PreparedField is the WireEnvelope oneof field.
	PreparedPayload []byte `json:"-"`
	PreparedField   uint32 `json:"-"`
	// PreparedSuffix contains trusted protobuf fields appended to the prepared
	// oneof body by Gateway. Keeping it separate avoids copying a large Farm
	// snapshot merely to add the small relation/mutable fields.
	PreparedSuffix []byte `json:"-"`
}

// EncodeFarmDelta builds the FarmDelta push Envelope bytes once for fan-out.
func EncodeFarmDelta(delta farm.FarmDelta) ([]byte, error) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return nil, fmt.Errorf("wireenv: marshal FarmDelta: %w", err)
	}
	return AppendTrustedEnvelope(nil, Envelope{
		Cmd:       CommandFarmDelta,
		ClientSeq: 0,
		Err:       errcode.OK,
		Payload:   payload,
	})
}

// EncodeFarmDeltaRecord builds a frame-ready binary record for Gateway-local
// fan-out. Unlike EncodeFarmDelta it is not a complete public frame.
func EncodeFarmDeltaRecord(delta farm.FarmDelta) ([]byte, error) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return nil, fmt.Errorf("wireenv: marshal FarmDelta: %w", err)
	}
	return EncodeTrustedBinaryRecord(Envelope{
		Cmd:       CommandFarmDelta,
		ClientSeq: 0,
		Err:       errcode.OK,
		Payload:   payload,
	})
}

// DecodeFarmDelta recovers the structured delta from pre-encoded Envelope bytes.
// It requires the public push metadata (cmd=9000, client_seq=0, err=OK) and a
// single strictly-decoded FarmDelta payload object.
func DecodeFarmDelta(encoded []byte) (farm.FarmDelta, error) {
	envelope, err := DecodeEnvelope(encoded)
	if err != nil {
		return farm.FarmDelta{}, err
	}
	if envelope.Cmd != CommandFarmDelta {
		return farm.FarmDelta{}, fmt.Errorf("wireenv: unexpected cmd %d", envelope.Cmd)
	}
	if envelope.ClientSeq != 0 {
		return farm.FarmDelta{}, fmt.Errorf("wireenv: FarmDelta client_seq must be 0")
	}
	if envelope.Err != errcode.OK {
		return farm.FarmDelta{}, fmt.Errorf("wireenv: FarmDelta err must be OK")
	}
	var delta farm.FarmDelta
	if err := DecodeStrictJSON(envelope.Payload, &delta); err != nil {
		return farm.FarmDelta{}, fmt.Errorf("wireenv: decode FarmDelta payload: %w", err)
	}
	return delta, nil
}

// EncodeEnvelope serializes one client Envelope frame.
func EncodeEnvelope(envelope Envelope) ([]byte, error) {
	if err := ValidatePayloadObject(envelope.Payload); err != nil {
		return nil, err
	}
	return AppendTrustedEnvelope(nil, envelope)
}

// AppendTrustedEnvelope appends one Envelope whose payload was produced by a
// trusted server-side JSON encoder. Unlike encoding/json's RawMessage path it
// does not compact and rescan the payload. Callers accepting browser or remote
// arbitrary bytes must use EncodeEnvelope, which performs complete validation.
//
// The first/last byte guard catches accidental empty/scalar payloads without
// adding an O(payload size) scan to every server response.
func AppendTrustedEnvelope(dst []byte, envelope Envelope) ([]byte, error) {
	payload := bytes.TrimSpace(envelope.Payload)
	if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
		return nil, fmt.Errorf("wireenv: trusted envelope payload must be a JSON object")
	}
	dst = append(dst, `{"cmd":`...)
	dst = strconv.AppendUint(dst, uint64(envelope.Cmd), 10)
	dst = append(dst, `,"client_seq":`...)
	dst = strconv.AppendUint(dst, uint64(envelope.ClientSeq), 10)
	dst = append(dst, `,"err":`...)
	dst = strconv.AppendInt(dst, int64(envelope.Err), 10)
	dst = append(dst, `,"payload":`...)
	dst = append(dst, payload...)
	dst = append(dst, '}')
	return dst, nil
}

// DecodeEnvelope decodes exactly one Envelope JSON value.
// Trailing objects, scalars, or non-empty garbage after the first value are rejected.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := DecodeStrictJSON(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("wireenv: decode envelope: %w", err)
	}
	if err := ValidatePayloadObject(envelope.Payload); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// DecodeStrictJSON decodes exactly one JSON value into target.
// After a successful decode, a second decode must yield io.EOF (no trailing value).
// Shared by gateway inbound frames and FarmDelta push validation.
func DecodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

// ValidatePayloadObject validates an untrusted Envelope payload once at an
// ingress boundary so downstream trusted encoders can avoid rescanning it.
func ValidatePayloadObject(payload json.RawMessage) error {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return fmt.Errorf("wireenv: envelope payload must be a JSON object")
	}
	// The previous implementation decoded into map[string]RawMessage solely to
	// validate the JSON shape. That reparsed every inbound and outbound payload
	// and allocated a map on the hottest Gateway path. json.Valid performs the
	// same complete-value syntax check without materializing the object.
	if !json.Valid(payload) {
		return fmt.Errorf("wireenv: invalid envelope payload")
	}
	return nil
}
