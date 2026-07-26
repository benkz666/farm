package gateway

import (
	"encoding/json"
	"testing"

	"farm/server/internal/pkgerr"
)

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 7,
		Err:       pkgerr.OK,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	}

	encoded, err := EncodeEnvelope(want)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	const wantJSON = `{"cmd":200,"client_seq":7,"err":0,"payload":{"owner_uid":42}}`
	if got := string(encoded); got != wantJSON {
		t.Fatalf("encoded Envelope = %s, want %s", got, wantJSON)
	}

	got, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got.Cmd != want.Cmd || got.ClientSeq != want.ClientSeq || got.Err != want.Err {
		t.Fatalf("decoded header = %#v, want %#v", got, want)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Fatalf("decoded payload = %s, want %s", got.Payload, want.Payload)
	}
}

func TestDecodeEnvelopeRejectsNonObjectPayload(t *testing.T) {
	t.Parallel()

	if _, err := DecodeEnvelope([]byte(`{"cmd":100,"client_seq":1,"err":0,"payload":[]}`)); err == nil {
		t.Fatal("DecodeEnvelope accepted an array payload")
	}
}

func TestDecodeEnvelopeRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	if _, err := DecodeEnvelope([]byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{}} {}`)); err == nil {
		t.Fatal("DecodeEnvelope accepted a second JSON value")
	}
}
