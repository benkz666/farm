package clientwire

import (
	"bytes"
	"encoding/json"
	"testing"

	"farm/server/domain/farm"
	"farm/server/shared/errcode"
)

func TestEncodeFarmDeltaPublicEnvelopeShape(t *testing.T) {
	t.Parallel()

	delta := farm.FarmDelta{
		OwnerUID: 42,
		FarmSeq:  7,
		ActorUID: 9,
		Action:   212,
		Plots:    []farm.PlotChange{{Index: 1, State: 3}},
	}
	encoded, err := EncodeFarmDelta(delta)
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}

	payload, err := DecodeFarmDelta(encoded)
	if err != nil {
		t.Fatalf("decode protobuf FarmDelta: %v", err)
	}
	if payload.OwnerUID != 42 || payload.FarmSeq != 7 || payload.ActorUID != 9 || payload.Action != 212 {
		t.Fatalf("payload = %#v", payload)
	}
	if len(payload.Plots) != 1 || payload.Plots[0].Index != 1 {
		t.Fatalf("plots = %#v", payload.Plots)
	}
}

func TestAppendTrustedEnvelopeMatchesStrictEncoding(t *testing.T) {
	t.Parallel()

	envelope := Envelope{
		Cmd:       204,
		ClientSeq: 17,
		Err:       errcode.BadRequest,
		Payload:   json.RawMessage(` {"farm_seq":"9007199254740993"} `),
	}
	want, err := EncodeEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	buffer := make([]byte, 3, 128)
	copy(buffer, "pre")
	got, err := AppendTrustedEnvelope(buffer, envelope)
	if err != nil {
		t.Fatalf("AppendTrustedEnvelope: %v", err)
	}
	if string(got[:3]) != "pre" || !bytes.Equal(got[3:], want) {
		t.Fatalf("trusted encoding = %q, want prefix + %q", got, want)
	}
	if string(want) != `{"cmd":204,"client_seq":17,"err":1002,"payload":{"farm_seq":"9007199254740993"}}` {
		t.Fatalf("wire shape changed: %s", want)
	}
}

func TestEncodeEnvelopeStillRejectsMalformedTrustedShape(t *testing.T) {
	t.Parallel()

	if _, err := EncodeEnvelope(Envelope{Payload: json.RawMessage(`{"broken":}`)}); err == nil {
		t.Fatal("EncodeEnvelope accepted malformed JSON")
	}
	for _, payload := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`"scalar"`)} {
		if _, err := AppendTrustedEnvelope(nil, Envelope{Payload: payload}); err == nil {
			t.Fatalf("AppendTrustedEnvelope accepted payload %q", payload)
		}
	}
}

func TestDecodeFarmDeltaRoundTrip(t *testing.T) {
	t.Parallel()

	want := farm.FarmDelta{OwnerUID: 11, FarmSeq: 3, Action: 206}
	encoded, err := EncodeFarmDelta(want)
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	got, err := DecodeFarmDelta(encoded)
	if err != nil {
		t.Fatalf("DecodeFarmDelta: %v", err)
	}
	if got.OwnerUID != want.OwnerUID || got.FarmSeq != want.FarmSeq || got.Action != want.Action {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
