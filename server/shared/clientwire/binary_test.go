package clientwire

import (
	"encoding/json"
	"testing"

	"farm/server/domain/farm"
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

func TestPreparedEnterFarmResponseRoundTrip(t *testing.T) {
	snapshot, err := MarshalFarmSnapshotPayload(farm.FarmSnapshotJSON{
		OwnerUID: 18446744073709551615,
		Nickname: "满精度",
		Plots:    []farm.PlotSnapshot{{Index: 1, MatureAt: 123}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalEnterFarmResponsePayload(snapshot, 9007199254740993, 123, "normal")
	if err != nil {
		t.Fatal(err)
	}
	payload, err = AppendEnterFarmGatewayFields(payload, true, "SELF")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeBinaryBatch([]Envelope{{
		Cmd:             200,
		ClientSeq:       8,
		Payload:         json.RawMessage(`{}`),
		PreparedPayload: payload,
		PreparedField:   PreparedEnterFarmResponse,
	}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBinaryBatch(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var response enterFarmPayload
	if len(decoded) != 1 || json.Unmarshal(decoded[0].Payload, &response) != nil {
		t.Fatalf("decoded = %#v", decoded)
	}
	if response.Snapshot.OwnerUID != 18446744073709551615 || uint64(response.FarmSeq) != 9007199254740993 || response.Relation != "SELF" || !response.TimeProfileMutable {
		t.Fatalf("response = %#v", response)
	}
}

func TestPreparedSuffixAppendsIntoFinalFrame(t *testing.T) {
	snapshot, err := MarshalFarmSnapshotPayload(farm.FarmSnapshotJSON{
		OwnerUID: 42,
		Nickname: "owner",
		Plots:    []farm.PlotSnapshot{{Index: 1, MatureAt: 123}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := MarshalEnterFarmResponsePayload(snapshot, 7, 123, "normal")
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := MarshalEnterFarmGatewaySuffix(true, "SELF")
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("reserved")
	frame, err := AppendBinaryBatch(prefix[:0], []Envelope{{
		Cmd:             200,
		ClientSeq:       8,
		PreparedPayload: payload,
		PreparedField:   PreparedEnterFarmResponse,
		PreparedSuffix:  suffix,
	}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBinaryBatch(frame)
	if err != nil {
		t.Fatal(err)
	}
	var response enterFarmPayload
	if len(decoded) != 1 || json.Unmarshal(decoded[0].Payload, &response) != nil {
		t.Fatalf("decoded = %#v", decoded)
	}
	if response.Snapshot.OwnerUID != 42 || uint64(response.FarmSeq) != 7 || response.Relation != "SELF" || !response.TimeProfileMutable {
		t.Fatalf("response = %#v", response)
	}
}

func TestPreparedCommandResponseRoundTrip(t *testing.T) {
	response := NewActionCommandResponse(9007199254740993, farm.PatchJSON{
		PlotIndex: 2,
		Plot:      &farm.PlotSnapshot{Index: 2, State: uint8(farm.StateGrowing), CropID: 1},
		Coin:      123,
		FarmSeq:   9007199254740993,
	}, nil)
	payload, err := MarshalCommandResponsePayload(response)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := EncodeBinaryBatch([]Envelope{{
		Cmd:             212,
		ClientSeq:       17,
		PreparedPayload: payload,
		PreparedField:   PreparedCommandResponse,
	}})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBinaryBatch(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].CommandResponse == nil || decoded[0].CommandResponse.Action == nil {
		t.Fatalf("decoded=%#v", decoded)
	}
	action := decoded[0].CommandResponse.Action
	if action.FarmSeq != 9007199254740993 || action.Patch == nil || action.Patch.Plot == nil || action.Patch.Plot.Index != 2 {
		t.Fatalf("action=%#v", action)
	}
}

func TestAppendBinaryBatchRetainsMixedEnvelopeOrder(t *testing.T) {
	prepared, err := MarshalSyncFarmCaughtUpPayload(9, 123, "normal", false)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := AppendBinaryBatch(make([]byte, 0, 256), []Envelope{
		{Cmd: 102, ClientSeq: 1, Payload: json.RawMessage(`{"client_time":1}`)},
		{Cmd: 204, ClientSeq: 2, PreparedPayload: prepared, PreparedField: PreparedSyncFarmResponse},
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

func BenchmarkAppendPreparedEnterFarmResponse(b *testing.B) {
	snapshot, err := MarshalFarmSnapshotPayload(farm.FarmSnapshotJSON{
		OwnerUID: 42,
		Nickname: string(make([]byte, 4800)),
		Plots:    []farm.PlotSnapshot{{Index: 1, MatureAt: 123}},
	})
	if err != nil {
		b.Fatal(err)
	}
	payload, err := MarshalEnterFarmResponsePayload(snapshot, 7, 123, "normal")
	if err != nil {
		b.Fatal(err)
	}
	suffix, err := MarshalEnterFarmGatewaySuffix(false, "SELF")
	if err != nil {
		b.Fatal(err)
	}
	envelopes := []Envelope{{
		Cmd:             200,
		ClientSeq:       8,
		PreparedPayload: payload,
		PreparedField:   PreparedEnterFarmResponse,
		PreparedSuffix:  suffix,
	}}
	buffer := make([]byte, 0, 8<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		if _, err := AppendBinaryBatch(buffer[:0], envelopes); err != nil {
			b.Fatal(err)
		}
	}
}
