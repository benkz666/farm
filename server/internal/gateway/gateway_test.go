package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

func TestRegisterReturnsAuthResponse(t *testing.T) {
	t.Parallel()

	handler := New(authStub{
		register: func(context.Context, string, string) (uint64, string, error) {
			return 42, "token-42", nil
		},
	}, sessionStub{}, runtimeStub{}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(`{"username":"alice","password":"secret12"}`))
	request.Host = "farm.test"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response authResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.UID != 42 || response.Token != "token-42" || response.WSURL != "ws://farm.test/ws" {
		t.Fatalf("response = %#v", response)
	}
}

func TestWebSocketHandshakePingAndEnterOwnFarm(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	server := httptest.NewServer(New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate}).Handler())
	t.Cleanup(server.Close)

	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	endpoint.Scheme = "ws"
	endpoint.Path = "/ws"
	conn, response, err := websocket.DefaultDialer.Dial(endpoint.String(), http.Header{
		"Sec-WebSocket-Protocol": []string{JSONSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != JSONSubprotocol {
		t.Fatalf("subprotocol = %q, want %q", got, JSONSubprotocol)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	handshake := readEnvelope(t, conn)
	if handshake.Cmd != CommandHandshake || handshake.ClientSeq != 1 || handshake.Err != pkgerr.OK {
		t.Fatalf("handshake response = %#v", handshake)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandPing,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"client_time":123}`),
	})
	pong := readEnvelope(t, conn)
	var pongPayload struct {
		ClientTime int64 `json:"client_time"`
		ServerTime int64 `json:"server_time"`
	}
	if err := json.Unmarshal(pong.Payload, &pongPayload); err != nil {
		t.Fatalf("decode pong: %v", err)
	}
	if pong.Cmd != CommandPing || pong.ClientSeq != 2 || pong.Err != pkgerr.OK || pongPayload.ClientTime != 123 || pongPayload.ServerTime == 0 {
		t.Fatalf("pong = %#v, payload = %#v", pong, pongPayload)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})
	enter := readEnvelope(t, conn)
	var enterPayload struct {
		Snapshot farm.FarmSnapshotJSON `json:"snapshot"`
		FarmSeq  uint64                `json:"farm_seq"`
		Relation string                `json:"relation"`
	}
	if err := json.Unmarshal(enter.Payload, &enterPayload); err != nil {
		t.Fatalf("decode EnterFarm: %v", err)
	}
	if enter.Cmd != CommandEnterFarm || enter.ClientSeq != 3 || enter.Err != pkgerr.OK {
		t.Fatalf("EnterFarm response = %#v", enter)
	}
	if enterPayload.Snapshot.OwnerUID != 42 || len(enterPayload.Snapshot.Plots) != 18 || enterPayload.Relation != "SELF" {
		t.Fatalf("EnterFarm payload = %#v", enterPayload)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 4,
		Payload:   json.RawMessage(`{"owner_uid":99}`),
	})
	if got := readEnvelope(t, conn); got.Err != pkgerr.NotFriend {
		t.Fatalf("foreign EnterFarm error = %d, want %d", got.Err, pkgerr.NotFriend)
	}
}

func TestConnectionLimiterRejectsOverCapacity(t *testing.T) {
	t.Parallel()

	limiter := newConnectionLimiter()
	for range 20 {
		if !limiter.Allow() {
			t.Fatal("limiter rejected initial capacity")
		}
	}
	if limiter.Allow() {
		t.Fatal("limiter accepted request beyond capacity")
	}
}

func writeEnvelope(t *testing.T, conn *websocket.Conn, envelope Envelope) {
	t.Helper()
	data, err := EncodeEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
}

func readEnvelope(t *testing.T, conn *websocket.Conn) Envelope {
	t.Helper()
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	envelope, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	return envelope
}

type authStub struct {
	register func(context.Context, string, string) (uint64, string, error)
	login    func(context.Context, string, string) (uint64, string, error)
}

func (s authStub) Register(ctx context.Context, username, password string) (uint64, string, error) {
	if s.register == nil {
		return 0, "", errors.New("unexpected Register")
	}
	return s.register(ctx, username, password)
}

func (s authStub) Login(ctx context.Context, username, password string) (uint64, string, error) {
	if s.login == nil {
		return 0, "", errors.New("unexpected Login")
	}
	return s.login(ctx, username, password)
}

type sessionStub struct {
	uid uint64
	err error
}

func (s sessionStub) Put(context.Context, string, uint64, time.Duration) error { return nil }

func (s sessionStub) Get(_ context.Context, token string) (uint64, error) {
	if token != "token-42" {
		return 0, store.ErrSessionNotFound
	}
	return s.uid, s.err
}

func (s sessionStub) Delete(context.Context, string) error { return nil }

type runtimeStub struct {
	aggregate *farm.Aggregate
	err       error
}

func (s runtimeStub) Do(_ uint64, fn func(*actor.FarmActor) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(&actor.FarmActor{Aggregate: s.aggregate})
}
