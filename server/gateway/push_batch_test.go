package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
)

func TestReceiveFarmDeltaBatchSkipsInvalidConnections(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		viewerUID   = uint64(7)
		viewerToken = "viewer-token"
		pushToken   = "batch-token"
	)
	registry := presence.NewWithBackend(newConnectionRegistryBackend())
	friends := newFriendStoreStub()
	friends.add(ownerUID, viewerUID)
	gateway := New(
		authStub{},
		sessionMapStub{viewerToken: viewerUID},
		multiRuntimeStub{actors: map[uint64]*room.FarmActor{
			ownerUID: {Aggregate: farm.NewAggregate(ownerUID, "owner")},
		}},
		WithFriendStore(friends),
		WithConnectionRegistry(registry, "gateway-1"),
	)
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)

	viewer := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, viewer, viewerToken)
	writeEnvelope(t, viewer, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	if got := readEnvelope(t, viewer); got.Err != errcode.OK {
		t.Fatalf("EnterFarm = %#v", got)
	}
	refs, err := registry.LookupSubscribers(context.Background(), ownerUID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("LookupSubscribers = %#v err=%v", refs, err)
	}

	envelope, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 5})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := gateway.applyFarmDeltaBatch([]uint64{refs[0].ConnID, 999999, 0}, envelope); err != nil {
		t.Fatalf("applyFarmDeltaBatch: %v", err)
	}

	delta := readEnvelope(t, viewer)
	if delta.Cmd != CommandFarmDelta || delta.ClientSeq != 0 || delta.Err != errcode.OK {
		t.Fatalf("delta = %#v", delta)
	}
	var payload farm.FarmDelta
	if err := json.Unmarshal(delta.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.OwnerUID != ownerUID || payload.FarmSeq != 5 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestReceiveFarmDeltaBatchRejectsUnsubscribedConnection(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		viewerUID   = uint64(7)
		viewerToken = "viewer-unsub-token"
		pushToken   = "batch-unsub-token"
	)
	registry := presence.NewWithBackend(newConnectionRegistryBackend())
	friends := newFriendStoreStub()
	friends.add(ownerUID, viewerUID)
	gateway := New(
		authStub{},
		sessionMapStub{viewerToken: viewerUID},
		multiRuntimeStub{actors: map[uint64]*room.FarmActor{
			ownerUID: {Aggregate: farm.NewAggregate(ownerUID, "owner")},
		}},
		WithFriendStore(friends),
		WithConnectionRegistry(registry, "gateway-1"),
	)
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)

	viewer := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, viewer, viewerToken)
	writeEnvelope(t, viewer, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	if got := readEnvelope(t, viewer); got.Err != errcode.OK {
		t.Fatalf("EnterFarm = %#v", got)
	}
	refs, err := registry.LookupSubscribers(context.Background(), ownerUID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("LookupSubscribers = %#v err=%v", refs, err)
	}
	connID := refs[0].ConnID

	writeEnvelope(t, viewer, Envelope{
		Cmd:       CommandLeaveFarm,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{}`),
	})
	if got := readEnvelope(t, viewer); got.Err != errcode.OK {
		t.Fatalf("LeaveFarm = %#v", got)
	}

	envelope, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 6})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := gateway.applyFarmDeltaBatch([]uint64{connID}, envelope); err != nil {
		t.Fatalf("applyFarmDeltaBatch: %v", err)
	}

	_ = viewer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := viewer.ReadMessage(); err == nil {
		t.Fatal("received FarmDelta after LeaveFarm / unsubscribed")
	}
}

func TestReceiveFarmDeltaBatchWritesIdenticalEncodedBytes(t *testing.T) {
	t.Parallel()

	const (
		ownerUID  = uint64(42)
		pushToken = "batch-bytes-token"
	)
	registry := presence.NewWithBackend(newConnectionRegistryBackend())
	friends := newFriendStoreStub()
	friends.add(ownerUID, 7)
	friends.add(ownerUID, 8)
	gateway := New(
		authStub{},
		sessionMapStub{"v7": 7, "v8": 8},
		multiRuntimeStub{actors: map[uint64]*room.FarmActor{
			ownerUID: {Aggregate: farm.NewAggregate(ownerUID, "owner")},
		}},
		WithFriendStore(friends),
		WithConnectionRegistry(registry, "gateway-1"),
	)
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)

	v7 := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, v7, "v7")
	writeEnvelope(t, v7, Envelope{Cmd: CommandEnterFarm, ClientSeq: 2, Payload: json.RawMessage(`{"owner_uid":42}`)})
	if got := readEnvelope(t, v7); got.Err != errcode.OK {
		t.Fatalf("EnterFarm v7 = %#v", got)
	}
	v8 := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, v8, "v8")
	writeEnvelope(t, v8, Envelope{Cmd: CommandEnterFarm, ClientSeq: 2, Payload: json.RawMessage(`{"owner_uid":42}`)})
	if got := readEnvelope(t, v8); got.Err != errcode.OK {
		t.Fatalf("EnterFarm v8 = %#v", got)
	}

	refs, err := registry.LookupSubscribers(context.Background(), ownerUID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("LookupSubscribers = %#v err=%v", refs, err)
	}
	connIDs := []uint64{refs[0].ConnID, refs[1].ConnID}
	envelope, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 9, Action: 212})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := gateway.applyFarmDeltaBatch(connIDs, envelope); err != nil {
		t.Fatalf("applyFarmDeltaBatch: %v", err)
	}

	frame7 := readRawFrame(t, v7)
	frame8 := readRawFrame(t, v8)
	batch7, err7 := clientwire.DecodeBinaryBatch(frame7)
	batch8, err8 := clientwire.DecodeBinaryBatch(frame8)
	want, wantErr := clientwire.DecodeFarmDelta(envelope)
	if err7 != nil || err8 != nil || wantErr != nil || len(batch7) != 1 || len(batch8) != 1 ||
		batch7[0].FarmDelta == nil || batch8[0].FarmDelta == nil ||
		!reflect.DeepEqual(clientwire.FarmDeltaFromProto(batch7[0].FarmDelta), want) ||
		!reflect.DeepEqual(clientwire.FarmDeltaFromProto(batch8[0].FarmDelta), want) {
		t.Fatalf("binary frames differ: err7=%v err8=%v wantErr=%v", err7, err8, wantErr)
	}
}

func TestReceiveFarmDeltaBatchRejectsMalformedEnvelopeWithoutPush(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		viewerUID   = uint64(7)
		viewerToken = "viewer-malformed-token"
		pushToken   = "batch-malformed-token"
	)
	registry := presence.NewWithBackend(newConnectionRegistryBackend())
	friends := newFriendStoreStub()
	friends.add(ownerUID, viewerUID)
	gateway := New(
		authStub{},
		sessionMapStub{viewerToken: viewerUID},
		multiRuntimeStub{actors: map[uint64]*room.FarmActor{
			ownerUID: {Aggregate: farm.NewAggregate(ownerUID, "owner")},
		}},
		WithFriendStore(friends),
		WithConnectionRegistry(registry, "gateway-1"),
	)
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)

	viewer := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, viewer, viewerToken)
	writeEnvelope(t, viewer, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	if got := readEnvelope(t, viewer); got.Err != errcode.OK {
		t.Fatalf("EnterFarm = %#v", got)
	}
	refs, err := registry.LookupSubscribers(context.Background(), ownerUID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("LookupSubscribers = %#v err=%v", refs, err)
	}

	malformedCases := [][]byte{
		[]byte(`{"cmd":9000,"client_seq":1,"err":0,"payload":{"owner_uid":42,"farm_seq":1}}`),
		[]byte(`{"cmd":9000,"client_seq":0,"err":1001,"payload":{"owner_uid":42,"farm_seq":1}}`),
		[]byte(`{"cmd":9000,"client_seq":0,"err":0,"payload":{"owner_uid":42,"farm_seq":1},"surprise":true}`),
	}
	valid, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 1})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	malformedCases = append(malformedCases, append(append([]byte(nil), valid...), []byte(` {"extra":1}`)...))

	for i, envelope := range malformedCases {
		if err := gateway.applyFarmDeltaBatch([]uint64{refs[0].ConnID}, envelope); err == nil {
			t.Fatalf("case %d expected malformed envelope error", i)
		}
	}

	_ = viewer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := viewer.ReadMessage(); err == nil {
		t.Fatal("received WS push for malformed FarmDelta batch")
	}
}

func TestReceiveFarmDeltaBatchDedupesConnIDs(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		viewerUID   = uint64(7)
		viewerToken = "viewer-dedupe-token"
		pushToken   = "batch-dedupe-token"
	)
	registry := presence.NewWithBackend(newConnectionRegistryBackend())
	friends := newFriendStoreStub()
	friends.add(ownerUID, viewerUID)
	gateway := New(
		authStub{},
		sessionMapStub{viewerToken: viewerUID},
		multiRuntimeStub{actors: map[uint64]*room.FarmActor{
			ownerUID: {Aggregate: farm.NewAggregate(ownerUID, "owner")},
		}},
		WithFriendStore(friends),
		WithConnectionRegistry(registry, "gateway-1"),
	)
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)

	viewer := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, viewer, viewerToken)
	writeEnvelope(t, viewer, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	if got := readEnvelope(t, viewer); got.Err != errcode.OK {
		t.Fatalf("EnterFarm = %#v", got)
	}
	refs, err := registry.LookupSubscribers(context.Background(), ownerUID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("LookupSubscribers = %#v err=%v", refs, err)
	}
	connID := refs[0].ConnID
	envelope, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 11})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := gateway.applyFarmDeltaBatch([]uint64{connID, connID, connID}, envelope); err != nil {
		t.Fatalf("applyFarmDeltaBatch: %v", err)
	}

	frame := readRawFrame(t, viewer)
	batch, decodeErr := clientwire.DecodeBinaryBatch(frame)
	want, wantErr := clientwire.DecodeFarmDelta(envelope)
	if decodeErr != nil || wantErr != nil || len(batch) != 1 || batch[0].FarmDelta == nil || !reflect.DeepEqual(clientwire.FarmDeltaFromProto(batch[0].FarmDelta), want) {
		t.Fatalf("binary frame = %#v decodeErr=%v wantErr=%v", batch, decodeErr, wantErr)
	}
	_ = viewer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := viewer.ReadMessage(); err == nil {
		t.Fatal("duplicate conn_id delivered a second FarmDelta frame")
	}
}

func TestRoomHubBroadcastEncodesOnceAndReusesBytes(t *testing.T) {
	t.Parallel()

	hub := NewRoomHub()
	var encodeCalls atomic.Int64
	hub.encodeFarmDelta = func(delta farm.FarmDelta) ([]byte, error) {
		encodeCalls.Add(1)
		return clientwire.EncodeFarmDeltaRecord(delta)
	}

	var (
		mu     sync.Mutex
		frames [][]byte
	)
	for _, connID := range []uint64{1, 2, 3} {
		hub.Subscribe(11, connID, func(_ farm.FarmDelta, encoded []byte) {
			mu.Lock()
			frames = append(frames, encoded)
			mu.Unlock()
		})
	}

	hub.Broadcast(farm.FarmDelta{OwnerUID: 11, FarmSeq: 2})

	if got := encodeCalls.Load(); got != 1 {
		t.Fatalf("encode calls = %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	for i := 1; i < len(frames); i++ {
		if &frames[i][0] != &frames[0][0] {
			t.Fatalf("connection %d did not receive the same encoded byte slice", i)
		}
	}
}

func readRawFrame(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return data
}
