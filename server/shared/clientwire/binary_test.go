package clientwire

import (
	"encoding/json"
	"testing"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/errcode"
)

func TestBinaryBatchRoundTripAndUint64String(t *testing.T) {
	want := []Envelope{
		{Cmd: 100, ClientSeq: 7, Payload: json.RawMessage(`{"uid":"18446744073709551615"}`)},
		{Cmd: 204, ClientSeq: 8, Err: errcode.BadRequest, Payload: json.RawMessage(`{}`)},
	}
	encoded, err := EncodeBinaryBatch(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBinaryBatch(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Cmd != 100 || got[0].ClientSeq != 7 || got[1].Err != errcode.BadRequest {
		t.Fatalf("unexpected round trip: %#v", got)
	}
	if string(got[0].Payload) != `{"uid":"18446744073709551615"}` {
		t.Fatalf("uint64 string changed: %s", got[0].Payload)
	}
}

func TestAppendBinaryBatchRetainsMixedEnvelopeOrder(t *testing.T) {
	frame, err := AppendBinaryBatch(make([]byte, 0, 256), []Envelope{
		{Cmd: 102, ClientSeq: 1, Payload: json.RawMessage(`{"client_time":1}`)},
		{Cmd: 204, ClientSeq: 2, SyncFarmRequest: &publicv3.SyncFarmRequest{OwnerUid: 42, FromSeq: 9}},
		{Cmd: 202, ClientSeq: 3, Payload: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBinaryBatch(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 || decoded[0].Cmd != 102 || decoded[1].Cmd != 204 || decoded[2].Cmd != 202 {
		t.Fatalf("mixed order = %#v", decoded)
	}
}

func TestBinaryBatchRejectsTrailingAndInvalidPayload(t *testing.T) {
	encoded, err := EncodeBinaryBatch([]Envelope{{Cmd: 100, ClientSeq: 1, Payload: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBinaryBatch(append(encoded, 0)); err == nil {
		t.Fatal("accepted trailing byte")
	}
	if _, err := EncodeBinaryBatch([]Envelope{{Cmd: 100, Payload: json.RawMessage(`[]`)}}); err == nil {
		t.Fatal("accepted non-object payload")
	}
}

func TestFrameBinaryRecordsRoundTrip(t *testing.T) {
	want := []Envelope{
		{Cmd: 9000, Payload: json.RawMessage(`{"farm_seq":"7"}`)},
		{Cmd: 9002, Err: errcode.BadRequest, Payload: json.RawMessage(`{}`)},
	}
	records := make([][]byte, 0, len(want))
	for _, envelope := range want {
		record, err := EncodeTrustedBinaryRecord(envelope)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	frame, err := FrameBinaryRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBinaryBatch(frame)
	if err != nil {
		t.Fatal(err)
	}
	var delta farm.FarmDelta
	if len(got) != len(want) || got[0].Cmd != 9000 || json.Unmarshal(got[0].Payload, &delta) != nil || delta.FarmSeq != 7 || got[1].Err != errcode.BadRequest {
		t.Fatalf("round trip = %#v", got)
	}
}
