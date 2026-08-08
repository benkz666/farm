package gateway

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/gateway/presence"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/sharding"
)

type countingRoomBackend struct {
	*connectionRegistryBackend
	upserts atomic.Int64
}

func (backend *countingRoomBackend) Upsert(ctx context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, keyTTL time.Duration) error {
	backend.upserts.Add(1)
	return backend.connectionRegistryBackend.Upsert(ctx, key, member, expiresAtUnixMilli, nowUnixMilli, keyTTL)
}

func TestAppendGatewayPayloadFieldsPreservesSnapshotBytesAndIDStrings(t *testing.T) {
	t.Parallel()

	input := json.RawMessage(`{"snapshot":{"owner_uid":"9007199254740993","plots":[]},"farm_seq":"9007199254740995","server_time":7,"time_profile":"demo"}`)
	got, err := appendGatewayPayloadFields(input, true, "SELF")
	if err != nil {
		t.Fatalf("appendGatewayPayloadFields: %v", err)
	}
	const want = `{"snapshot":{"owner_uid":"9007199254740993","plots":[]},"farm_seq":"9007199254740995","server_time":7,"time_profile":"demo","time_profile_mutable":true,"relation":"SELF"}`
	if string(got) != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestAppendTrustedGatewayEnvelopeMatchesMaterializedResponse(t *testing.T) {
	t.Parallel()

	envelope := Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 17,
		Err:       errcode.OK,
		Payload:   json.RawMessage(`{"snapshot":{"owner_uid":"9007199254740993"},"farm_seq":"9"}`),
	}
	fields := gatewayPayloadFields{enabled: true, mutable: true, relation: "SELF"}
	got, err := appendTrustedGatewayEnvelope(nil, envelope, fields)
	if err != nil {
		t.Fatalf("appendTrustedGatewayEnvelope: %v", err)
	}
	materializedPayload, err := appendTrustedGatewayPayloadFields(envelope.Payload, fields.mutable, fields.relation)
	if err != nil {
		t.Fatalf("appendTrustedGatewayPayloadFields: %v", err)
	}
	envelope.Payload = materializedPayload
	want, err := clientwire.EncodeEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("wire response = %s, want %s", got, want)
	}
}

func TestExecuteFarmRPCRejectsMalformedSuccessfulPayloadBeforeTrustedEncoding(t *testing.T) {
	t.Parallel()

	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRouteTable: %v", err)
	}
	client := &farmRPCStub{responses: []farmrpc.CommandResponse{{
		Err:     errcode.OK,
		Payload: json.RawMessage(`{"snapshot":}`),
	}}}
	gateway := New(nil, nil, nil, WithFarmRPC(client, routes))
	_, err = gateway.executeFarmRPC(context.Background(), 42, farmrpc.CommandRequest{Operation: farmrpc.OperationEnterFarm})
	if err == nil {
		t.Fatal("executeFarmRPC accepted malformed successful payload")
	}
}

func TestSelfFarmRPCHotPathsRelayPayloadAndAppendGatewayFields(t *testing.T) {
	t.Parallel()

	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRouteTable: %v", err)
	}
	client := &farmRPCStub{responses: []farmrpc.CommandResponse{
		{
			Err:     errcode.OK,
			Payload: json.RawMessage(`{"snapshot":{"owner_uid":"9007199254740993","plots":[]},"farm_seq":"7","server_time":8,"time_profile":"demo"}`),
		},
		{
			Err:     errcode.OK,
			Payload: json.RawMessage(`{"farm_seq":"7","server_time":9,"time_profile":"demo"}`),
		},
	}}
	gateway := New(nil, nil, nil, WithFarmRPC(client, routes))
	gateway.EnableDebugTime()
	connection := &wsConnection{id: 3, uid: 42, authed: true}

	enter := gateway.handleEnterFarm(connection, Envelope{
		Cmd:     CommandEnterFarm,
		Payload: json.RawMessage(`{"owner_uid":"0"}`),
	})
	if enter.Err != errcode.OK {
		t.Fatalf("EnterFarm err = %d", enter.Err)
	}
	const wantEnter = `{"snapshot":{"owner_uid":"9007199254740993","plots":[]},"farm_seq":"7","server_time":8,"time_profile":"demo","time_profile_mutable":true,"relation":"SELF"}`
	if string(enter.Payload) != wantEnter {
		t.Fatalf("EnterFarm payload = %s, want %s", enter.Payload, wantEnter)
	}

	syncResponse := gateway.handleSyncFarm(connection, Envelope{
		Cmd:     CommandSyncFarm,
		Payload: json.RawMessage(`{"owner_uid":"0","from_seq":"7"}`),
	})
	if syncResponse.Err != errcode.OK {
		t.Fatalf("SyncFarm err = %d", syncResponse.Err)
	}
	const wantSync = `{"farm_seq":"7","server_time":9,"time_profile":"demo","time_profile_mutable":true}`
	if string(syncResponse.Payload) != wantSync {
		t.Fatalf("SyncFarm payload = %s, want %s", syncResponse.Payload, wantSync)
	}
}

func TestSelfSyncFarmWirePathDefersGatewayFieldsUntilFinalEncoding(t *testing.T) {
	t.Parallel()

	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("ParseRouteTable: %v", err)
	}
	raw := json.RawMessage(`{"farm_seq":"9007199254740993","server_time":9,"time_profile":"demo"}`)
	gateway := New(nil, nil, nil, WithFarmRPC(&farmRPCStub{responses: []farmrpc.CommandResponse{{
		Err:     errcode.OK,
		Payload: raw,
	}}}, routes))
	gateway.EnableDebugTime()

	response, fields := gateway.handleSyncFarmForWire(&wsConnection{id: 3, uid: 42, authed: true}, Envelope{
		Cmd:       CommandSyncFarm,
		ClientSeq: 19,
		Payload:   json.RawMessage(`{"owner_uid":"0","from_seq":"7"}`),
	})
	if response.Err != errcode.OK {
		t.Fatalf("SyncFarm err = %d", response.Err)
	}
	if string(response.Payload) != string(raw) {
		t.Fatalf("wire path materialized payload early: %s", response.Payload)
	}
	if !fields.enabled || !fields.mutable || fields.relation != "" {
		t.Fatalf("wire fields = %+v", fields)
	}
	encoded, err := appendTrustedGatewayEnvelope(nil, response, fields)
	if err != nil {
		t.Fatalf("appendTrustedGatewayEnvelope: %v", err)
	}
	const want = `{"cmd":204,"client_seq":19,"err":0,"payload":{"farm_seq":"9007199254740993","server_time":9,"time_profile":"demo","time_profile_mutable":true}}`
	if string(encoded) != want {
		t.Fatalf("encoded = %s, want %s", encoded, want)
	}
}

func TestSyncFarmUsesFreshGatewayWatermarkWithoutFarmRPC(t *testing.T) {
	t.Parallel()
	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	client := &farmRPCStub{responses: []farmrpc.CommandResponse{{
		Err:     errcode.OK,
		Payload: json.RawMessage(`{"snapshot":{"owner_uid":"42"},"farm_seq":"7","server_time":8,"time_profile":"demo"}`),
		FarmSeq: 7,
	}}}
	gateway := New(nil, nil, nil, WithFarmRPC(client, routes))
	connection := &wsConnection{id: 3, uid: 42, authed: true}
	enter := gateway.handleEnterFarm(connection, Envelope{
		Cmd:     CommandEnterFarm,
		Payload: json.RawMessage(`{"owner_uid":"0"}`),
	})
	if enter.Err != errcode.OK {
		t.Fatalf("EnterFarm err=%d", enter.Err)
	}
	sync := gateway.handleSyncFarm(connection, Envelope{
		Cmd:     CommandSyncFarm,
		Payload: json.RawMessage(`{"owner_uid":"0","from_seq":"7"}`),
	})
	if sync.Err != errcode.OK {
		t.Fatalf("SyncFarm err=%d", sync.Err)
	}
	if len(client.calls) != 1 {
		t.Fatalf("Farm RPC calls=%d, want EnterFarm only", len(client.calls))
	}
	var payload syncFarmResponse
	if err := json.Unmarshal(sync.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if uint64(payload.FarmSeq) != 7 || payload.Snapshot != nil || len(payload.Deltas) != 0 {
		t.Fatalf("fast SyncFarm payload=%#v", payload)
	}
}

func TestRoomWatermarkDropsOnDeltaGap(t *testing.T) {
	t.Parallel()
	connection := &wsConnection{roomUID: 42, roomSeq: 7, roomSeqKnown: true, roomSeqObservedAt: time.Now().UnixNano()}
	connection.roomMu.Lock()
	connection.observeRoomDeltaLocked(9)
	connection.roomMu.Unlock()
	if _, ok := connection.matchesFreshRoomWatermark(42, 9, time.Now()); ok {
		t.Fatal("gapped delta remained eligible for local SyncFarm")
	}
}

func TestAppendGatewayPayloadFieldsRejectsMalformedUpstreamJSON(t *testing.T) {
	t.Parallel()

	for _, payload := range []json.RawMessage{
		json.RawMessage(`{"snapshot":}`),
		json.RawMessage(`[]`),
		json.RawMessage(`{} trailing`),
	} {
		if _, err := appendGatewayPayloadFields(payload, false, "SELF"); err == nil {
			t.Fatalf("accepted malformed payload %q", payload)
		}
	}
}

func TestEnterRoomSameOwnerSkipsDistributedSubscribeAndRestartsBarrier(t *testing.T) {
	t.Parallel()

	backend := &countingRoomBackend{connectionRegistryBackend: newConnectionRegistryBackend()}
	registry := presence.NewWithBackend(backend)
	gateway := New(nil, nil, nil, WithConnectionRegistry(registry, "gateway-0"))
	connection := &wsConnection{
		id:             8,
		uid:            42,
		roomUID:        42,
		heldFarmDeltas: make([]farm.FarmDelta, 1),
		holdFarmDeltas: false,
	}

	if err := gateway.enterRoom(connection, 42); err != nil {
		t.Fatalf("enterRoom: %v", err)
	}
	upserts := backend.upserts.Load()
	if upserts != 0 {
		t.Fatalf("same-room EnterFarm performed %d distributed upserts", upserts)
	}
	connection.roomMu.Lock()
	holding := connection.holdFarmDeltas
	held := len(connection.heldFarmDeltas)
	connection.roomMu.Unlock()
	if !holding || held != 0 {
		t.Fatalf("snapshot barrier = (holding=%v, held=%d), want (true, 0)", holding, held)
	}
}
