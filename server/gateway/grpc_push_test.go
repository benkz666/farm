package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
)

func TestGRPCPushMailNotify(t *testing.T) {
	t.Parallel()

	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	delivered := make(chan string, 1)
	connection := &wsConnection{id: 7, uid: 42, authed: true}
	gateway.mailNotifyDelivery = func(ws *wsConnection, kind string) error {
		if ws == connection {
			delivered <- kind
		}
		return nil
	}
	connection.enableMailNotify(gateway)
	defer connection.closeMailNotify()
	gateway.connections.Store(connection.id, connection)

	pool := newTestPushPool(t, "push-token", gateway)
	conn, err := pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	_, err = farmv1.NewGatewayPushServiceClient(conn).PushMailNotify(context.Background(), &farmv1.PushMailNotifyRequest{
		ConnectionId: 7,
		Uid:          42,
		Kind:         "friend_request",
	})
	if err != nil {
		t.Fatalf("PushMailNotify: %v", err)
	}
	select {
	case kind := <-delivered:
		if kind != "friend_request" {
			t.Fatalf("kind = %q", kind)
		}
	case <-time.After(time.Second):
		t.Fatal("MailNotify was not delivered")
	}
}

func TestGRPCPushFarmDeltaBatch(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		viewerUID   = uint64(7)
		viewerToken = "viewer-grpc-batch-token"
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
	delta := farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 5}

	pool := newTestPushPool(t, "push-token", gateway)
	clientConn, err := pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	_, err = farmv1.NewGatewayPushServiceClient(clientConn).PushFarmDeltaBatch(context.Background(), &farmv1.PushFarmDeltaBatchRequest{
		ConnIds: []uint64{refs[0].ConnID},
		Delta:   clientwire.FarmDeltaToProto(delta),
	})
	if err != nil {
		t.Fatalf("PushFarmDeltaBatch: %v", err)
	}

	frame := readRawFrame(t, viewer)
	batch, decodeErr := clientwire.DecodeBinaryBatch(frame)
	if decodeErr != nil || len(batch) != 1 || batch[0].Cmd != CommandFarmDelta || batch[0].FarmDelta == nil || batch[0].FarmDelta.FarmSeq != delta.FarmSeq {
		t.Fatalf("binary frame = %#v decodeErr=%v", batch, decodeErr)
	}
}

func TestGRPCPushFarmDeltaBatchRejectsMalformedEnvelope(t *testing.T) {
	t.Parallel()

	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: farm.NewAggregate(42, "alice")})
	pool := newTestPushPool(t, "push-token", gateway)
	clientConn, err := pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	_, err = farmv1.NewGatewayPushServiceClient(clientConn).PushFarmDeltaBatch(context.Background(), &farmv1.PushFarmDeltaBatchRequest{
		ConnIds: []uint64{1},
	})
	if err == nil {
		t.Fatal("expected malformed envelope error")
	}
}
