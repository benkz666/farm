package wireenv

import (
	"encoding/json"
	"testing"

	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
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
	if envelope.Cmd != CommandFarmDelta || envelope.ClientSeq != 0 || envelope.Err != pkgerr.OK {
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
