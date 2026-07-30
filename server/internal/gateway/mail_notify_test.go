package gateway

import (
	"encoding/json"
	"testing"
	"time"

	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

func TestMailNotifyMailboxIsolatesSlowConnection(t *testing.T) {
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	started := make(chan struct{})
	release := make(chan struct{})
	fastDelivered := make(chan string, 1)
	slow := &wsConnection{id: 1, uid: 42, authed: true}
	fast := &wsConnection{id: 2, uid: 42, authed: true}
	gateway.mailNotifyDelivery = func(connection *wsConnection, kind string) error {
		if connection == slow {
			close(started)
			<-release
			return nil
		}
		fastDelivered <- kind
		return nil
	}
	slow.enableMailNotify(gateway)
	fast.enableMailNotify(gateway)
	defer slow.closeMailNotify()
	defer fast.closeMailNotify()
	defer close(release)
	gateway.connections.Store(slow.id, slow)
	gateway.connections.Store(fast.id, fast)

	if err := gateway.PublishMailNotify(t.Context(), 42, "friend_request"); err != nil {
		t.Fatalf("PublishMailNotify: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow MailNotify delivery did not start")
	}
	select {
	case kind := <-fastDelivered:
		if kind != "friend_request" {
			t.Fatalf("fast MailNotify kind = %q", kind)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("slow MailNotify connection delayed fast delivery")
	}
}

func TestHandshakeResponsePrecedesQueuedMailNotify(t *testing.T) {
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	var serverConnection *wsConnection
	gateway.afterConnectionRegistered = func(registered *wsConnection) {
		serverConnection = registered
		_ = gateway.PublishMailNotify(t.Context(), 42, "friend_request")
	}
	connection := openWebSocket(t, gateway.Handler())
	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	handshake := readEnvelope(t, connection)
	if handshake.Cmd != CommandHandshake || handshake.ClientSeq != 1 || handshake.Err != pkgerr.OK {
		t.Fatalf("Handshake = %#v", handshake)
	}
	notify := readEnvelope(t, connection)
	if notify.Cmd != CommandMailNotify || notify.ClientSeq != 0 || notify.Err != pkgerr.OK {
		t.Fatalf("MailNotify = %#v", notify)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	select {
	case <-serverConnection.mailNotifyDone:
	case <-time.After(time.Second):
		t.Fatal("MailNotify dispatcher did not exit after WebSocket close")
	}
}
