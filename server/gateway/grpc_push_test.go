package gateway

import (
	"context"
	"testing"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
)

func TestPushServerDeliversTypedFarmDelta(t *testing.T) {
	connection, writer := newTestPushConn(t, 0)
	connection.id, connection.uid, connection.authed = 7, 42, true
	gateway := New(nil, nil)
	gateway.connections.Store(connection.id, connection)

	_, err := NewPushServer(gateway).PushFarmDeltaBatch(context.Background(), &farmv1.PushFarmDeltaBatchRequest{
		ConnIds: []uint64{7},
		Delta:   &publicv3.FarmDelta{OwnerUid: 42, FarmSeq: 9},
	})
	if err != nil {
		t.Fatalf("PushFarmDeltaBatch: %v", err)
	}
	frames := waitWrites(t, writer, 1, time.Second)
	batch, err := clientwire.DecodeWireBatch(frames[0])
	if err != nil || len(batch) != 1 || batch[0].GetFarmDelta().GetFarmSeq() != 9 {
		t.Fatalf("typed push batch=%v err=%v", batch, err)
	}
}

func TestPushServerDeliversPlayerDelta(t *testing.T) {
	assertPushEnvelope(t, &publicv3.WireEnvelope{
		Cmd: CommandPlayerDelta,
		Payload: &publicv3.WireEnvelope_PlayerDelta{
			PlayerDelta: &publicv3.PlayerDelta{Coin: 12},
		},
	}, func(envelope *publicv3.WireEnvelope) bool {
		return envelope.GetPlayerDelta().GetCoin() == 12
	})
}

func TestPushServerDeliversMailNotify(t *testing.T) {
	assertPushEnvelope(t, &publicv3.WireEnvelope{
		Cmd: CommandMailNotify,
		Payload: &publicv3.WireEnvelope_MailNotify{
			MailNotify: &publicv3.MailNotify{Kind: "new_mail"},
		},
	}, func(envelope *publicv3.WireEnvelope) bool {
		return envelope.GetMailNotify().GetKind() == "new_mail"
	})
}

func TestPushServerDeliversTaskNotify(t *testing.T) {
	assertPushEnvelope(t, &publicv3.WireEnvelope{
		Cmd: CommandTaskNotify,
		Payload: &publicv3.WireEnvelope_TaskNotify{
			TaskNotify: &publicv3.Task{Id: 1, Progress: 2, Target: 3},
		},
	}, func(envelope *publicv3.WireEnvelope) bool {
		return envelope.GetTaskNotify().GetProgress() == 2
	})
}

func assertPushEnvelope(t *testing.T, envelope *publicv3.WireEnvelope, valid func(*publicv3.WireEnvelope) bool) {
	t.Helper()
	connection, writer := newTestPushConn(t, 0)
	connection.id, connection.uid, connection.authed = 7, 42, true
	gateway := New(nil, nil)
	gateway.connections.Store(connection.id, connection)

	_, err := NewPushServer(gateway).DeliverPush(context.Background(), &farmv1.DeliverPushRequest{
		ConnectionId: 7,
		Uid:          42,
		Envelope:     envelope,
	})
	if err != nil {
		t.Fatalf("DeliverPush: %v", err)
	}
	frames := waitWrites(t, writer, 1, time.Second)
	batch, err := clientwire.DecodeWireBatch(frames[0])
	if err != nil || len(batch) != 1 || !valid(batch[0]) {
		t.Fatalf("opaque push batch=%v err=%v", batch, err)
	}
}

func TestPushServerKicksSession(t *testing.T) {
	connection, writer := newTestPushConn(t, 0)
	connection.id, connection.uid, connection.authed = 7, 42, true
	gateway := New(nil, nil)
	gateway.connections.Store(connection.id, connection)

	_, err := NewPushServer(gateway).PushSessionKick(context.Background(), &farmv1.PushSessionKickRequest{
		ConnectionId: 7,
		Uid:          42,
		Reason:       int32(errcode.Kicked),
	})
	if err != nil {
		t.Fatalf("PushSessionKick: %v", err)
	}
	frames := waitWrites(t, writer, 1, time.Second)
	batch, err := clientwire.DecodeWireBatch(frames[0])
	if err != nil || len(batch) != 1 || batch[0].GetSessionKick().GetReason() != int32(errcode.Kicked) {
		t.Fatalf("session kick batch=%v err=%v", batch, err)
	}
}

func TestPushServerRevokesOnlyMatchingRoom(t *testing.T) {
	gateway := New(nil, nil)
	connection := &wsConnection{id: 7, uid: 42, authed: true, roomUID: 99}
	gateway.connections.Store(connection.id, connection)
	server := NewPushServer(gateway)

	_, _ = server.RevokeFarmAccess(context.Background(), &farmv1.RevokeFarmAccessRequest{
		ConnectionId: 7, ViewerUid: 42, OwnerUid: 100,
	})
	if connection.currentRoom() != 99 {
		t.Fatal("mismatched revocation removed the room")
	}
	_, _ = server.RevokeFarmAccess(context.Background(), &farmv1.RevokeFarmAccessRequest{
		ConnectionId: 7, ViewerUid: 42, OwnerUid: 99,
	})
	if connection.currentRoom() != 0 {
		t.Fatalf("matching revocation kept room %d", connection.currentRoom())
	}
}
