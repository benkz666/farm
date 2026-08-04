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

	var envelope Envelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope.Cmd != CommandFarmDelta || envelope.ClientSeq != 0 || envelope.Err != errcode.OK {
		t.Fatalf("envelope = %#v", envelope)
	}
	var payload farm.FarmDelta
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.OwnerUID != 42 || payload.FarmSeq != 7 || payload.ActorUID != 9 || payload.Action != 212 {
		t.Fatalf("payload = %#v", payload)
	}
	if len(payload.Plots) != 1 || payload.Plots[0].Index != 1 {
		t.Fatalf("plots = %#v", payload.Plots)
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

func TestEncodePushBatchEmbedsRawEnvelopes(t *testing.T) {
	t.Parallel()

	first, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta first: %v", err)
	}
	second, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 2})
	if err != nil {
		t.Fatalf("EncodeFarmDelta second: %v", err)
	}
	batched, err := EncodePushBatch([][]byte{first, second})
	if err != nil {
		t.Fatalf("EncodePushBatch: %v", err)
	}

	var envelope Envelope
	if err := json.Unmarshal(batched, &envelope); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}
	if envelope.Cmd != CommandPushBatch || envelope.ClientSeq != 0 || envelope.Err != errcode.OK {
		t.Fatalf("envelope = %#v", envelope)
	}
	var payload struct {
		Envelopes []json.RawMessage `json:"envelopes"`
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(payload.Envelopes) != 2 {
		t.Fatalf("envelopes len = %d", len(payload.Envelopes))
	}
	if !bytes.Equal(payload.Envelopes[0], first) {
		t.Fatalf("first envelope mutated:\n got %s\nwant %s", payload.Envelopes[0], first)
	}
	if !bytes.Equal(payload.Envelopes[1], second) {
		t.Fatalf("second envelope mutated:\n got %s\nwant %s", payload.Envelopes[1], second)
	}
}

func TestEncodePushBatchRejectsSingleOrOverMax(t *testing.T) {
	t.Parallel()

	one, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if _, err := EncodePushBatch([][]byte{one}); err == nil {
		t.Fatal("EncodePushBatch accepted a single envelope")
	}
	tooMany := make([][]byte, MaxPushBatchEnvelopes+1)
	for i := range tooMany {
		tooMany[i] = one
	}
	if _, err := EncodePushBatch(tooMany); err == nil {
		t.Fatal("EncodePushBatch accepted over-max envelopes")
	}
}
