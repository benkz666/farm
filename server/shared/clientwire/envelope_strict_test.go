package clientwire

import (
	"testing"

	"farm/server/domain/farm"
	"farm/server/shared/errcode"
)

func TestDecodeEnvelopeRejectsTrailingSecondObject(t *testing.T) {
	t.Parallel()

	valid, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	malformed := append(append([]byte(nil), valid...), []byte(` {"extra":1}`)...)
	if _, err := DecodeEnvelope(malformed); err == nil {
		t.Fatal("DecodeEnvelope accepted trailing second JSON object")
	}
}

func TestDecodeEnvelopeRejectsTrailingScalar(t *testing.T) {
	t.Parallel()

	valid, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	malformed := append(append([]byte(nil), valid...), []byte(` 0`)...)
	if _, err := DecodeEnvelope(malformed); err == nil {
		t.Fatal("DecodeEnvelope accepted trailing scalar")
	}
}

func TestDecodeEnvelopeRejectsTrailingGarbage(t *testing.T) {
	t.Parallel()

	valid, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 1, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	malformed := append(append([]byte(nil), valid...), []byte(` junk`)...)
	if _, err := DecodeEnvelope(malformed); err == nil {
		t.Fatal("DecodeEnvelope accepted trailing non-empty garbage")
	}
}

func TestDecodeEnvelopeRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":9000,"client_seq":0,"err":0,"payload":{"owner_uid":1},"surprise":true}`)
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("DecodeEnvelope accepted unknown envelope field")
	}
}

func TestDecodeFarmDeltaRejectsNonZeroClientSeq(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":9000,"client_seq":1,"err":0,"payload":{"owner_uid":42,"farm_seq":1}}`)
	if _, err := DecodeFarmDelta(raw); err == nil {
		t.Fatal("DecodeFarmDelta accepted non-zero client_seq")
	}
}

func TestDecodeFarmDeltaRejectsNonZeroErr(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":9000,"client_seq":0,"err":1001,"payload":{"owner_uid":42,"farm_seq":1}}`)
	if _, err := DecodeFarmDelta(raw); err == nil {
		t.Fatal("DecodeFarmDelta accepted non-zero err")
	}
}

func TestDecodeFarmDeltaRejectsUnknownPayloadField(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":9000,"client_seq":0,"err":0,"payload":{"owner_uid":42,"farm_seq":1,"nope":1}}`)
	if _, err := DecodeFarmDelta(raw); err == nil {
		t.Fatal("DecodeFarmDelta accepted unknown payload field")
	}
}

func TestDecodeFarmDeltaRejectsWrongCmd(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"cmd":9002,"client_seq":0,"err":0,"payload":{"owner_uid":42,"farm_seq":1}}`)
	if _, err := DecodeFarmDelta(raw); err == nil {
		t.Fatal("DecodeFarmDelta accepted non-FarmDelta cmd")
	}
}

func TestDecodeFarmDeltaAcceptsValidPushMetadata(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeFarmDelta(farm.FarmDelta{OwnerUID: 42, FarmSeq: 3})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	got, err := DecodeFarmDelta(encoded)
	if err != nil {
		t.Fatalf("DecodeFarmDelta: %v", err)
	}
	if got.OwnerUID != 42 || got.FarmSeq != 3 {
		t.Fatalf("got %#v", got)
	}
	if errcode.OK != 0 {
		t.Fatalf("errcode.OK = %d", errcode.OK)
	}
}
