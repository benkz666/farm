// Package clientwire encodes client-visible WebSocket Envelopes for internal push.
// It sits below gateway and farmrpc so both can share one codec without an import cycle.
package clientwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

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
	// CommandPushBatch packs multiple already-encoded push Envelopes into one
	// WebSocket text frame. client_seq is always 0; responses never use this cmd.
	CommandPushBatch uint32 = 9010

	// MaxPushBatchEnvelopes caps how many inner Envelopes one PushBatch may carry.
	MaxPushBatchEnvelopes = 64
)

// Envelope is the public JSON shape written on the WebSocket wire.
type Envelope struct {
	Cmd       uint32          `json:"cmd"`
	ClientSeq uint32          `json:"client_seq"`
	Err       errcode.Code    `json:"err"`
	Payload   json.RawMessage `json:"payload"`
}

// EncodeFarmDelta builds the FarmDelta push Envelope bytes once for fan-out.
func EncodeFarmDelta(delta farm.FarmDelta) ([]byte, error) {
	payload, err := json.Marshal(delta)
	if err != nil {
		return nil, fmt.Errorf("wireenv: marshal FarmDelta: %w", err)
	}
	return EncodeEnvelope(Envelope{
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
	if err := validatePayloadObject(envelope.Payload); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("wireenv: encode envelope: %w", err)
	}
	return encoded, nil
}

// EncodePushBatch builds one cmd=9010 frame that embeds pre-encoded push
// Envelope JSON objects via json.RawMessage — no per-envelope decode/re-encode.
// Caller must pass 2..MaxPushBatchEnvelopes envelopes; a single push should be
// written raw to avoid the wrapper bytes.
func EncodePushBatch(envelopes [][]byte) ([]byte, error) {
	if len(envelopes) < 2 {
		return nil, fmt.Errorf("wireenv: PushBatch requires at least 2 envelopes")
	}
	if len(envelopes) > MaxPushBatchEnvelopes {
		return nil, fmt.Errorf("wireenv: PushBatch exceeds max %d envelopes", MaxPushBatchEnvelopes)
	}
	raws := make([]json.RawMessage, len(envelopes))
	for i, envelope := range envelopes {
		envelope = bytes.TrimSpace(envelope)
		if len(envelope) == 0 || envelope[0] != '{' {
			return nil, fmt.Errorf("wireenv: PushBatch item %d must be a JSON object", i)
		}
		raws[i] = json.RawMessage(envelope)
	}
	payload, err := json.Marshal(struct {
		Envelopes []json.RawMessage `json:"envelopes"`
	}{Envelopes: raws})
	if err != nil {
		return nil, fmt.Errorf("wireenv: marshal PushBatch payload: %w", err)
	}
	return EncodeEnvelope(Envelope{
		Cmd:       CommandPushBatch,
		ClientSeq: 0,
		Err:       errcode.OK,
		Payload:   payload,
	})
}

// DecodeEnvelope decodes exactly one Envelope JSON value.
// Trailing objects, scalars, or non-empty garbage after the first value are rejected.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := DecodeStrictJSON(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("wireenv: decode envelope: %w", err)
	}
	if err := validatePayloadObject(envelope.Payload); err != nil {
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

func validatePayloadObject(payload json.RawMessage) error {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return fmt.Errorf("wireenv: envelope payload must be a JSON object")
	}
	var object map[string]json.RawMessage
	if err := DecodeStrictJSON(payload, &object); err != nil {
		return fmt.Errorf("wireenv: invalid envelope payload: %w", err)
	}
	return nil
}
