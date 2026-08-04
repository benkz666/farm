package gateway

import (
	"strings"
	"testing"
)

func TestDecodeEnvelopeRejectsTrailingSecondObject(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{}} {"extra":1}`)
	_, err := DecodeEnvelope(raw)
	if err == nil {
		t.Fatal("DecodeEnvelope accepted trailing second JSON object")
	}
	if !strings.HasPrefix(err.Error(), "gateway:") {
		t.Fatalf("error %q missing gateway: prefix", err)
	}
}

func TestDecodeEnvelopeRejectsTrailingScalar(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{}} 0`)
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("DecodeEnvelope accepted trailing scalar")
	}
}

func TestDecodeEnvelopeRejectsTrailingGarbage(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{}} junk`)
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("DecodeEnvelope accepted trailing garbage")
	}
}

func TestDecodeEnvelopeRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{},"surprise":true}`)
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("DecodeEnvelope accepted unknown envelope field")
	}
}

func TestDecodeEnvelopeRejectsPayloadTrailingObject(t *testing.T) {
	t.Parallel()

	// Invalid nested payload with a second object; must not be accepted as a client frame.
	raw := []byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{"owner_uid":1}{"x":1}}`)
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("DecodeEnvelope accepted payload with trailing second object")
	}
}

func TestDecodeEnvelopeAcceptsValidClientFrame(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":100,"client_seq":1,"err":0,"payload":{"token":"t"}}`)
	got, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got.Cmd != CommandHandshake || got.ClientSeq != 1 || string(got.Payload) != `{"token":"t"}` {
		t.Fatalf("got %#v", got)
	}
}
