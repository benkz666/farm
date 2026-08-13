package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/domain/farm"
	"farm/server/farmsvr/crossfarm"
	"farm/server/farmsvr/farmrpc"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	"farm/server/shared/clientjson"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/sharding"
	"farm/server/shared/store"
	socialapi "farm/server/socialsvr/api"

	farmv1 "farm/server/gen/farm/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		"Sec-WebSocket-Protocol": []string{BinarySubprotocol},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != BinarySubprotocol {
		t.Fatalf("subprotocol = %q, want %q", got, BinarySubprotocol)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	handshake := readEnvelope(t, conn)
	if handshake.Cmd != CommandHandshake || handshake.ClientSeq != 1 || handshake.Err != errcode.OK {
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
	if pong.Cmd != CommandPing || pong.ClientSeq != 2 || pong.Err != errcode.OK || pongPayload.ClientTime != 123 || pongPayload.ServerTime == 0 {
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
		FarmSeq  clientjson.Uint64     `json:"farm_seq"`
		Relation string                `json:"relation"`
	}
	if err := json.Unmarshal(enter.Payload, &enterPayload); err != nil {
		t.Fatalf("decode EnterFarm: %v", err)
	}
	if enter.Cmd != CommandEnterFarm || enter.ClientSeq != 3 || enter.Err != errcode.OK {
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
	if got := readEnvelope(t, conn); got.Err != errcode.NotFriend {
		t.Fatalf("foreign EnterFarm error = %d, want %d", got.Err, errcode.NotFriend)
	}
}

func TestWebSocketBinaryBatchProcessesHandshakeThenPingInOrder(t *testing.T) {
	t.Parallel()

	connection := openWebSocket(t, New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	).Handler())
	frame, err := EncodeBinaryBatch([]Envelope{
		{Cmd: CommandHandshake, ClientSeq: 1, Payload: json.RawMessage(`{"token":"token-42","client_config_ver":1}`)},
		{Cmd: CommandPing, ClientSeq: 2, Payload: json.RawMessage(`{"client_time":123}`)},
	})
	if err != nil {
		t.Fatalf("encode request batch: %v", err)
	}
	if err := connection.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatalf("write request batch: %v", err)
	}
	messageType, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read response batch: %v", err)
	}
	responses, err := DecodeBinaryBatch(data)
	if err != nil {
		t.Fatalf("decode response batch: %v", err)
	}
	if messageType != websocket.BinaryMessage || len(responses) != 2 {
		t.Fatalf("messageType=%d responses=%#v", messageType, responses)
	}
	if responses[0].Cmd != CommandHandshake || responses[0].ClientSeq != 1 || responses[0].Err != errcode.OK ||
		responses[1].Cmd != CommandPing || responses[1].ClientSeq != 2 || responses[1].Err != errcode.OK {
		t.Fatalf("responses=%#v", responses)
	}
}

func TestDebugTimeProfileHotSwitchOverWebSocket(t *testing.T) {
	aggregate := farm.NewAggregate(42, "alice")
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTimeProfile(gameconfig.TimeProfileDemo),
	)
	gateway.EnableDebugTime()
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandSetTimeProfile,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"time_profile":"authentic"}`),
	})
	response := readEnvelope(t, connection)
	if response.Err != errcode.OK {
		t.Fatalf("SetTimeProfile response = %#v", response)
	}
	var payload setTimeProfileResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode SetTimeProfile: %v", err)
	}
	if payload.TimeProfile != gameconfig.TimeProfileAuthentic || !payload.TimeProfileMutable {
		t.Fatalf("SetTimeProfile payload = %#v", payload)
	}
	if got := gateway.TimeProfile(); got != gameconfig.TimeProfileAuthentic {
		t.Fatalf("gateway profile = %q, want authentic", got)
	}

	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})
	enter := readEnvelope(t, connection)
	var enterPayload enterFarmResponse
	if err := json.Unmarshal(enter.Payload, &enterPayload); err != nil {
		t.Fatalf("decode EnterFarm: %v", err)
	}
	if enterPayload.TimeProfile != gameconfig.TimeProfileAuthentic || !enterPayload.TimeProfileMutable {
		t.Fatalf("EnterFarm payload profile = %#v", enterPayload)
	}
}

func TestTimeProfileHotSwitchRejectedOutsideDebugMode(t *testing.T) {
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandSetTimeProfile,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"time_profile":"authentic"}`),
	})
	if response := readEnvelope(t, connection); response.Err != errcode.BadRequest {
		t.Fatalf("SetTimeProfile outside debug err = %d, want %d", response.Err, errcode.BadRequest)
	}
	if got := gateway.TimeProfile(); got != gameconfig.TimeProfileDemo {
		t.Fatalf("rejected switch changed profile to %q", got)
	}
}

func TestTimeProfileHotSwitchRollsBackPeersOnFailure(t *testing.T) {
	okStub := &recordingDebugServer{}
	okPair := grpcx.NewBufconnPair(t, "token", func(server *grpc.Server) {
		farmv1.RegisterDebugServiceServer(server, okStub)
	})
	failStub := &recordingDebugServer{failProfile: gameconfig.TimeProfileAuthentic}
	failPair := grpcx.NewBufconnPair(t, "token", func(server *grpc.Server) {
		farmv1.RegisterDebugServiceServer(server, failStub)
	})
	pool := grpcx.NewPool("token")
	okConn, err := okPair.Pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("ok conn: %v", err)
	}
	failConn, err := failPair.Pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("fail conn: %v", err)
	}
	pool.RegisterTestConn("a-ok", okConn)
	pool.RegisterTestConn("z-fail", failConn)

	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithTimeProfile(gameconfig.TimeProfileDemo),
		WithDebugTimeFanout(pool, map[string]string{"a-ok": "a-ok", "z-fail": "z-fail"}, nil, ""),
	)
	if err := gateway.switchTimeProfile(context.Background(), gameconfig.TimeProfileAuthentic); err == nil {
		t.Fatal("switch unexpectedly succeeded")
	}
	if gateway.TimeProfile() != gameconfig.TimeProfileDemo {
		t.Fatalf("local profile = %q, want demo", gateway.TimeProfile())
	}
	if !reflect.DeepEqual(okStub.profiles, []string{gameconfig.TimeProfileAuthentic, gameconfig.TimeProfileDemo}) {
		t.Fatalf("peer rollback profiles = %#v", okStub.profiles)
	}
}

type recordingDebugServer struct {
	farmv1.UnimplementedDebugServiceServer
	profiles    []string
	failProfile string
}

func (server *recordingDebugServer) SetTimeProfile(_ context.Context, request *farmv1.SetTimeProfileRequest) (*farmv1.SetTimeProfileResponse, error) {
	server.profiles = append(server.profiles, request.TimeProfile)
	if request.TimeProfile == server.failProfile {
		return nil, status.Error(codes.Unavailable, "fail")
	}
	return &farmv1.SetTimeProfileResponse{TimeProfile: request.TimeProfile}, nil
}

func TestGatewaySecondLocalSessionKicksFirst(t *testing.T) {
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	first := openWebSocket(t, gateway.Handler())
	second := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, first, "token-42")
	writeEnvelope(t, second, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   marshalPayload(handshakeRequest{Token: "token-42", ClientConfigVer: 1}),
	})
	if got := readEnvelope(t, second); got.Err != errcode.OK {
		t.Fatalf("second handshake = %#v, want OK", got)
	}

	kick := readEnvelope(t, first)
	var kickPayload struct {
		Reason errcode.Code `json:"reason"`
	}
	if err := json.Unmarshal(kick.Payload, &kickPayload); err != nil {
		t.Fatalf("decode SessionKick: %v", err)
	}
	if kick.Cmd != CommandSessionKick || kick.ClientSeq != 0 || kick.Err != errcode.OK || kickPayload.Reason != errcode.Kicked {
		t.Fatalf("first session kick = %#v payload=%#v", kick, kickPayload)
	}

	writeEnvelope(t, second, Envelope{
		Cmd:       CommandPing,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"client_time":123}`),
	})
	if got := readEnvelope(t, second); got.Err != errcode.OK {
		t.Fatalf("second session Ping = %#v, want OK", got)
	}
}

func TestGatewaySecondSessionAcrossSharedRegistryKicksFirst(t *testing.T) {
	const pushToken = "push-token"
	backend := newConnectionRegistryBackend()
	registryA := presence.NewWithBackend(backend)
	registryB := presence.NewWithBackend(backend)
	firstGateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithConnectionRegistry(registryA, "gateway-0"),
	)
	firstServer := httptest.NewServer(firstGateway.Handler())
	t.Cleanup(firstServer.Close)
	secondGateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithConnectionRegistry(registryB, "gateway-1"),
		WithSessionKickPusher(newTestSessionKickPusher(t, pushToken, firstGateway, "gateway-0")),
	)
	first := openWebSocketAt(t, firstServer.URL)
	second := openWebSocket(t, secondGateway.Handler())
	handshakeWebSocket(t, first, "token-42")

	writeEnvelope(t, second, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   marshalPayload(handshakeRequest{Token: "token-42", ClientConfigVer: 1}),
	})
	if got := readEnvelope(t, second); got.Err != errcode.OK {
		t.Fatalf("cross-gateway second handshake = %#v, want OK", got)
	}
	kick := readEnvelope(t, first)
	var kickPayload struct {
		Reason errcode.Code `json:"reason"`
	}
	if err := json.Unmarshal(kick.Payload, &kickPayload); err != nil {
		t.Fatalf("decode cross-gateway SessionKick: %v", err)
	}
	if kick.Cmd != CommandSessionKick || kick.ClientSeq != 0 || kick.Err != errcode.OK || kickPayload.Reason != errcode.Kicked {
		t.Fatalf("cross-gateway kick = %#v payload=%#v", kick, kickPayload)
	}

	refs, err := registryA.Lookup(t.Context(), 42)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(refs) != 1 || refs[0].GatewayID != "gateway-1" {
		t.Fatalf("active connection refs = %#v, want only gateway-1", refs)
	}

	writeEnvelope(t, second, Envelope{
		Cmd:       CommandPing,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"client_time":123}`),
	})
	if got := readEnvelope(t, second); got.Err != errcode.OK {
		t.Fatalf("new cross-gateway session Ping = %#v, want OK", got)
	}
}

func TestHandshakeResponsePrecedesQueuedTaskNotify(t *testing.T) {
	task := store.Task{ID: store.TaskPlantID, Title: "完成一次播种", Progress: 1, Target: 1, RewardCoin: 20}
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	var serverConnection *wsConnection
	gateway.afterConnectionRegistered = func(registered *wsConnection) {
		serverConnection = registered
		_ = gateway.PublishTaskNotify(t.Context(), 42, task)
	}
	connection := openWebSocket(t, gateway.Handler())

	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   marshalPayload(handshakeRequest{Token: "token-42", ClientConfigVer: 1}),
	})
	handshake := readEnvelope(t, connection)
	if handshake.Cmd != CommandHandshake || handshake.ClientSeq != 1 || handshake.Err != errcode.OK {
		t.Fatalf("Handshake = %#v", handshake)
	}
	notify := readEnvelope(t, connection)
	if notify.Cmd != CommandTaskNotify || notify.ClientSeq != 0 || notify.Err != errcode.OK {
		t.Fatalf("TaskNotify = %#v", notify)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	select {
	case <-serverConnection.taskNotifyDone:
	case <-time.After(time.Second):
		t.Fatal("TaskNotify dispatcher did not exit after WebSocket close")
	}
}

func TestFriendEnterEmitsTaskNotifyAfterVisitProgressChanges(t *testing.T) {
	const (
		ownerUID   = uint64(42)
		visitorUID = uint64(7)
	)
	friends := newFriendStoreStub()
	friends.add(ownerUID, visitorUID)
	gateway := New(
		authStub{},
		sessionMapStub{"visitor-token": visitorUID},
		runtimeStub{aggregate: farm.NewAggregate(ownerUID, "owner")},
		WithFriendStore(friends),
		WithTaskMailStore(newTaskMailStoreStub()),
	)
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "visitor-token")

	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	first := readEnvelope(t, connection)
	second := readEnvelope(t, connection)
	var entered, notified Envelope
	for _, message := range []Envelope{first, second} {
		switch message.Cmd {
		case CommandEnterFarm:
			entered = message
		case CommandTaskNotify:
			notified = message
		}
	}
	if entered.Err != errcode.OK || entered.ClientSeq != 2 {
		t.Fatalf("EnterFarm = %#v", entered)
	}
	if notified.Err != errcode.OK || notified.ClientSeq != 0 {
		t.Fatalf("TaskNotify = %#v", notified)
	}
	var task store.Task
	if err := json.Unmarshal(notified.Payload, &task); err != nil {
		t.Fatalf("decode TaskNotify payload: %v", err)
	}
	if task.ID != store.TaskVisitID || task.Progress != task.Target {
		t.Fatalf("visit TaskNotify payload = %#v", task)
	}
}

func TestWebSocketRegistersAndUnregistersConnection(t *testing.T) {
	t.Parallel()

	backend := newConnectionRegistryBackend()
	registry := presence.NewWithBackend(backend)
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithConnectionRegistry(registry, "gateway-0"),
	)
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	refs, err := registry.Lookup(context.Background(), 42)
	if err != nil {
		t.Fatalf("Lookup after handshake: %v", err)
	}
	if len(refs) != 1 || refs[0].GatewayID != "gateway-0" {
		t.Fatalf("Lookup after handshake = %#v", refs)
	}

	if err := connection.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		refs, err = registry.Lookup(context.Background(), 42)
		if err != nil {
			t.Fatalf("Lookup after close: %v", err)
		}
		if len(refs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Lookup after close = %#v, want empty", refs)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRegisterConnectionRejectsUnauthenticatedConnection(t *testing.T) {
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
	)
	connection := &wsConnection{id: 1, uid: 42}

	if err := gateway.registerConnection(t.Context(), connection); err == nil {
		t.Fatal("registerConnection accepted an unauthenticated connection")
	}
	if _, ok := gateway.connections.Load(connection.id); ok {
		t.Fatal("unauthenticated connection was published to push fan-out")
	}
}

func TestGatewayPushEndpointDeliversDeltaToRemoteRoomSubscriber(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		viewerUID   = uint64(7)
		viewerToken = "viewer-token"
		pushToken   = "push-token"
	)
	backend := newConnectionRegistryBackend()
	registry := presence.NewWithBackend(backend)
	friends := newFriendStoreStub()
	friends.add(ownerUID, viewerUID)
	runtime := multiRuntimeStub{actors: map[uint64]*room.FarmActor{
		ownerUID: {Aggregate: farm.NewAggregate(ownerUID, "owner")},
	}}
	gateway := New(
		authStub{},
		sessionMapStub{viewerToken: viewerUID},
		runtime,
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
	if err != nil {
		t.Fatalf("LookupSubscribers: %v", err)
	}
	if len(refs) != 1 || refs[0].GatewayID != "gateway-1" {
		t.Fatalf("LookupSubscribers = %#v", refs)
	}
	envelope, err := clientwire.EncodeFarmDelta(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 3})
	if err != nil {
		t.Fatalf("EncodeFarmDelta: %v", err)
	}
	if err := gateway.applyFarmDeltaBatch([]uint64{refs[0].ConnID}, envelope); err != nil {
		t.Fatalf("applyFarmDeltaBatch: %v", err)
	}

	delta := readEnvelope(t, viewer)
	if delta.Cmd != CommandFarmDelta || delta.ClientSeq != 0 || delta.Err != errcode.OK {
		t.Fatalf("pushed Delta envelope = %#v", delta)
	}
	var payload farm.FarmDelta
	if err := json.Unmarshal(delta.Payload, &payload); err != nil {
		t.Fatalf("decode pushed Delta: %v", err)
	}
	if payload.OwnerUID != ownerUID || payload.FarmSeq != 3 {
		t.Fatalf("pushed Delta = %#v", payload)
	}
}

func TestGatewayTaskNotifyApplyRoutesOnlyAuthenticatedMatchingSession(t *testing.T) {
	const (
		uid   = uint64(42)
		token = "player-token"
	)
	registry := presence.NewWithBackend(newConnectionRegistryBackend())
	gateway := New(
		authStub{},
		sessionMapStub{token: uid},
		runtimeStub{aggregate: farm.NewAggregate(uid, "alice")},
		WithConnectionRegistry(registry, "gateway-0"),
	)
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)
	connection := openWebSocketAt(t, server.URL)
	handshakeWebSocket(t, connection, token)
	refs, err := registry.Lookup(t.Context(), uid)
	if err != nil || len(refs) != 1 {
		t.Fatalf("Lookup connection = %#v, %v", refs, err)
	}
	task := store.Task{ID: store.TaskPlantID, Title: "完成一次播种", Progress: 1, Target: 1, RewardCoin: 20}
	gateway.applyTaskNotify(refs[0].ConnID, uid, task)
	notified := readEnvelope(t, connection)
	if notified.Cmd != CommandTaskNotify || notified.ClientSeq != 0 || notified.Err != errcode.OK {
		t.Fatalf("TaskNotify envelope = %#v", notified)
	}

	gateway.applyTaskNotify(refs[0].ConnID, uid+1, task)
	_ = connection.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("UID-mismatched TaskNotify reached the session")
	}

	if err := connection.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := gateway.connections.Load(refs[0].ConnID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed connection remained registered")
		}
		time.Sleep(time.Millisecond)
	}
	gateway.applyTaskNotify(refs[0].ConnID, uid, task)
}

func TestFriendEnterSyncAndLeaveFarmBroadcast(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		friendUID   = uint64(7)
		ownerToken  = "owner-token"
		friendToken = "friend-token"
	)
	friends := newFriendStoreStub()
	friends.add(ownerUID, friendUID)
	runtime := multiRuntimeStub{actors: map[uint64]*room.FarmActor{
		ownerUID:  {Aggregate: farm.NewAggregate(ownerUID, "owner")},
		friendUID: {Aggregate: farm.NewAggregate(friendUID, "friend")},
	}}
	gateway := New(
		authStub{},
		sessionMapStub{ownerToken: ownerUID, friendToken: friendUID},
		runtime,
		WithFriendStore(friends),
	)
	gateway.SetClock(func() int64 { return 10_000 })

	owner := openWebSocket(t, gateway.Handler())
	friend := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, owner, ownerToken)
	handshakeWebSocket(t, friend, friendToken)

	writeEnvelope(t, friend, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	friendEnter := readEnvelope(t, friend)
	var friendEnterPayload struct {
		Snapshot farm.FarmSnapshotJSON `json:"snapshot"`
		FarmSeq  clientjson.Uint64     `json:"farm_seq"`
		Relation string                `json:"relation"`
	}
	if err := json.Unmarshal(friendEnter.Payload, &friendEnterPayload); err != nil {
		t.Fatalf("decode friend EnterFarm: %v", err)
	}
	if friendEnter.Err != errcode.OK || friendEnterPayload.Relation != "FRIEND" {
		t.Fatalf("friend EnterFarm = %#v, payload = %#v", friendEnter, friendEnterPayload)
	}
	if friendEnterPayload.Snapshot.Coin != 0 || friendEnterPayload.Snapshot.Exp != 0 {
		t.Fatalf("friend EnterFarm leaked economy: coin=%d exp=%d",
			friendEnterPayload.Snapshot.Coin, friendEnterPayload.Snapshot.Exp)
	}
	if friendEnterPayload.Snapshot.Bag != nil || friendEnterPayload.Snapshot.Warehouse != nil {
		t.Fatalf("friend EnterFarm leaked bag/warehouse: bag=%v warehouse=%v",
			friendEnterPayload.Snapshot.Bag, friendEnterPayload.Snapshot.Warehouse)
	}

	writeEnvelope(t, owner, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})
	if got := readEnvelope(t, owner); got.Err != errcode.OK {
		t.Fatalf("owner EnterFarm = %#v", got)
	}
	writeEnvelope(t, friend, Envelope{
		Cmd:       CommandTill,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":42,"plot_index":0,"arg":0}`),
	})
	if got := readEnvelope(t, friend); got.Err != errcode.NotOwner {
		t.Fatalf("visitor Till error = %d, want %d", got.Err, errcode.NotOwner)
	}

	writeEnvelope(t, owner, Envelope{
		Cmd:       CommandTill,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":0}`),
	})
	if got := readEnvelope(t, owner); got.Err != errcode.OK {
		t.Fatalf("owner Till response = %#v", got)
	}
	delta := readEnvelope(t, friend)
	var deltaPayload farm.FarmDelta
	if err := json.Unmarshal(delta.Payload, &deltaPayload); err != nil {
		t.Fatalf("decode FarmDelta: %v", err)
	}
	if delta.Cmd != CommandFarmDelta || delta.ClientSeq != 0 || delta.Err != errcode.OK ||
		deltaPayload.OwnerUID != ownerUID || deltaPayload.FarmSeq != 1 || len(deltaPayload.Plots) != 1 {
		t.Fatalf("FarmDelta = %#v, payload = %#v", delta, deltaPayload)
	}

	writeEnvelope(t, friend, Envelope{
		Cmd:       CommandSyncFarm,
		ClientSeq: 4,
		Payload:   json.RawMessage(`{"owner_uid":42,"from_seq":0}`),
	})
	sync := readEnvelope(t, friend)
	var syncPayload syncFarmResponse
	if err := json.Unmarshal(sync.Payload, &syncPayload); err != nil {
		t.Fatalf("decode SyncFarm: %v", err)
	}
	if sync.Err != errcode.OK || syncPayload.FarmSeq != 1 || len(syncPayload.Deltas) != 1 || syncPayload.Snapshot != nil {
		t.Fatalf("SyncFarm = %#v, payload = %#v", sync, syncPayload)
	}

	ownerActor := runtime.actors[ownerUID]
	for seq := uint64(2); seq <= farm.DeltaRingCapacity+1; seq++ {
		ownerActor.Deltas.Append(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: seq})
	}
	ownerActor.Aggregate.FarmSeq = farm.DeltaRingCapacity + 1
	writeEnvelope(t, friend, Envelope{
		Cmd:       CommandSyncFarm,
		ClientSeq: 5,
		Payload:   json.RawMessage(`{"owner_uid":42,"from_seq":0}`),
	})
	expiredSync := readEnvelope(t, friend)
	var expiredSyncPayload syncFarmResponse
	if err := json.Unmarshal(expiredSync.Payload, &expiredSyncPayload); err != nil {
		t.Fatalf("decode expired SyncFarm: %v", err)
	}
	if expiredSync.Err != errcode.OK || expiredSyncPayload.Snapshot == nil || len(expiredSyncPayload.Deltas) != 0 {
		t.Fatalf("expired SyncFarm = %#v, payload = %#v", expiredSync, expiredSyncPayload)
	}

	writeEnvelope(t, friend, Envelope{
		Cmd:       CommandLeaveFarm,
		ClientSeq: 6,
		Payload:   emptyPayload,
	})
	if got := readEnvelope(t, friend); got.Err != errcode.OK {
		t.Fatalf("LeaveFarm = %#v", got)
	}

	writeEnvelope(t, owner, Envelope{
		Cmd:       CommandTill,
		ClientSeq: 4,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":1,"arg":0}`),
	})
	if got := readEnvelope(t, owner); got.Err != errcode.OK {
		t.Fatalf("second owner Till response = %#v", got)
	}
	if err := friend.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set friend read deadline: %v", err)
	}
	if _, _, err := friend.ReadMessage(); err == nil {
		t.Fatal("received FarmDelta after LeaveFarm")
	}
}

func TestVisitorWaterUsesCrossOwnerDecisionAndReceivesDelta(t *testing.T) {
	const (
		ownerUID   = uint64(42)
		visitorUID = uint64(7)
	)
	bridge := &crossOwnerBridge{}

	friends := newFriendStoreStub()
	friends.add(ownerUID, visitorUID)
	ownerAggregate := farm.NewAggregate(ownerUID, "owner")
	ownerAggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonStartAt:  1,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
		LastSettleAt:   40_000,
		LastWaterAt:    1,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	runtime := multiRuntimeStub{actors: map[uint64]*room.FarmActor{
		ownerUID:   {Aggregate: ownerAggregate},
		visitorUID: {Aggregate: farm.NewAggregate(visitorUID, "visitor")},
	}}
	gateway := New(
		authStub{},
		sessionMapStub{"visitor-token": visitorUID},
		runtime,
		WithFriendStore(friends),
		WithCrossFarmClient(bridge),
	)
	gateway.SetClock(func() int64 { return 40_000 })
	owner := crossfarm.NewOwner(
		runtime,
		friends,
		gateway.Now,
		crossfarm.DeltaPublisherFunc(func(_ context.Context, delta farm.FarmDelta, _ presence.ConnRef) error {
			gateway.rooms.Broadcast(delta)
			return nil
		}),
		nil,
	)
	bridge.owner = owner

	visitor := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, visitor, "visitor-token")
	writeEnvelope(t, visitor, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	if got := readEnvelope(t, visitor); got.Err != errcode.OK {
		t.Fatalf("EnterFarm = %#v", got)
	}

	writeEnvelope(t, visitor, Envelope{
		Cmd:       CommandWater,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":42,"plot_index":0,"arg":0}`),
	})
	if err := visitor.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set PlayerDelta read deadline: %v", err)
	}
	first := readEnvelope(t, visitor)
	second := readEnvelope(t, visitor)
	third := readEnvelope(t, visitor)
	var response Envelope
	var delta Envelope
	var playerDelta Envelope
	for _, message := range []Envelope{first, second, third} {
		if message.Cmd == CommandWater {
			response = message
		}
		if message.Cmd == CommandFarmDelta {
			delta = message
		}
		if message.Cmd == 9002 {
			playerDelta = message
		}
	}
	if response.Err != errcode.OK || response.ClientSeq != 3 {
		t.Fatalf("Water response = %#v", response)
	}
	if delta.Err != errcode.OK {
		t.Fatalf("FarmDelta = %#v", delta)
	}
	var playerPayload farm.PlayerDelta
	if err := json.Unmarshal(playerDelta.Payload, &playerPayload); err != nil {
		t.Fatalf("decode PlayerDelta: %v", err)
	}
	if playerDelta.ClientSeq != 0 || playerDelta.Err != errcode.OK || playerPayload.Exp != 2 || playerPayload.Coin != 1_000 {
		t.Fatalf("PlayerDelta = %#v, payload = %#v", playerDelta, playerPayload)
	}
	if ownerAggregate.FarmSeq != 1 || ownerAggregate.Plots[0].LastWaterAt != 40_000 {
		t.Fatalf("owner aggregate after Water = %#v", ownerAggregate)
	}
	visitorAggregate := runtime.actors[visitorUID].Aggregate
	if visitorAggregate.Exp != 2 || visitorAggregate.Daily.MaintainCnt != 1 {
		t.Fatalf("visitor reward = exp:%d daily:%#v", visitorAggregate.Exp, visitorAggregate.Daily)
	}
}

func TestClearOnGrowingPlotFailsWithoutPatchOrFarmDelta(t *testing.T) {
	t.Parallel()

	const (
		ownerUID    = uint64(42)
		friendUID   = uint64(7)
		ownerToken  = "owner-token"
		friendToken = "friend-token"
	)
	friends := newFriendStoreStub()
	friends.add(ownerUID, friendUID)
	ownerAggregate := farm.NewAggregate(ownerUID, "owner")
	ownerAggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		StageCount:     3,
		SeasonStartAt:  10_000,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
		LastSettleAt:   10_000,
		LastWaterAt:    10_000,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	runtime := multiRuntimeStub{actors: map[uint64]*room.FarmActor{
		ownerUID:  {Aggregate: ownerAggregate},
		friendUID: {Aggregate: farm.NewAggregate(friendUID, "friend")},
	}}
	gateway := New(
		authStub{},
		sessionMapStub{ownerToken: ownerUID, friendToken: friendUID},
		runtime,
		WithFriendStore(friends),
	)
	gateway.SetClock(func() int64 { return 11_000 })

	owner := openWebSocket(t, gateway.Handler())
	friend := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, owner, ownerToken)
	handshakeWebSocket(t, friend, friendToken)

	writeEnvelope(t, friend, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":42}`),
	})
	friendEnter := readEnvelope(t, friend)
	if friendEnter.Err != errcode.OK {
		t.Fatalf("friend EnterFarm = %#v", friendEnter)
	}

	writeEnvelope(t, owner, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})
	if got := readEnvelope(t, owner); got.Err != errcode.OK {
		t.Fatalf("owner EnterFarm = %#v", got)
	}
	plotBeforeClear := ownerAggregate.Plots[0]
	seqBeforeClear := ownerAggregate.FarmSeq

	writeEnvelope(t, owner, Envelope{
		Cmd:       CommandClear,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":0}`),
	})
	clear := readEnvelope(t, owner)
	if clear.Err != errcode.PlotNotCleanable {
		t.Fatalf("Clear err = %d, want %d", clear.Err, errcode.PlotNotCleanable)
	}
	if string(clear.Payload) != "{}" {
		t.Fatalf("Clear response unexpectedly includes patch: %s", clear.Payload)
	}
	if ownerAggregate.FarmSeq != seqBeforeClear {
		t.Fatalf("aggregate FarmSeq = %d, want %d", ownerAggregate.FarmSeq, seqBeforeClear)
	}
	if !reflect.DeepEqual(ownerAggregate.Plots[0], plotBeforeClear) {
		t.Fatalf("failed clear mutated plot:\n got %#v\nwant %#v", ownerAggregate.Plots[0], plotBeforeClear)
	}
	if deltas, ok := runtime.actors[ownerUID].Deltas.Since(0); !ok || len(deltas) != 0 {
		t.Fatalf("failed clear appended farm delta: %#v", deltas)
	}
}

func TestEnterFarmAdvancesExpiredGrowingPlotBeforeSnapshot(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonStartAt:  19_000,
		SeasonDuration: 1_000,
		MatureAt:       20_000,
		LastSettleAt:   19_000,
		LastWaterAt:    19_000,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gateway.SetClock(func() int64 { return 20_000 })

	conn := openWebSocket(t, gateway.Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	if got := readEnvelope(t, conn); got.Err != errcode.OK {
		t.Fatalf("handshake = %#v", got)
	}
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})

	got := readEnvelope(t, conn)
	if got.Err != errcode.OK {
		t.Fatalf("EnterFarm = %#v", got)
	}
	var payload struct {
		Snapshot farm.FarmSnapshotJSON `json:"snapshot"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode EnterFarm payload: %v", err)
	}
	if payload.Snapshot.Plots[0].State != farm.StateMature {
		t.Fatalf("snapshot plot state = %d, want mature", payload.Snapshot.Plots[0].State)
	}
	if aggregate.Plots[0].State != farm.StateMature {
		t.Fatalf("aggregate plot state = %d, want mature", aggregate.Plots[0].State)
	}
}

func TestEnterFarmAdvanceBroadcastsDeltaToExistingSubscribers(t *testing.T) {
	t.Parallel()

	const (
		ownerUID  = uint64(42)
		friendUID = uint64(7)
	)
	aggregate := farm.NewAggregate(ownerUID, "owner")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonStartAt:  10_000,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
		LastSettleAt:   69_999,
		LastWaterAt:    69_999,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	friends := newFriendStoreStub()
	friends.add(ownerUID, friendUID)
	runtime := multiRuntimeStub{actors: map[uint64]*room.FarmActor{
		ownerUID:  {Aggregate: aggregate},
		friendUID: {Aggregate: farm.NewAggregate(friendUID, "friend")},
	}}
	gateway := New(
		authStub{},
		sessionMapStub{"owner-token": ownerUID, "friend-token": friendUID},
		runtime,
		WithFriendStore(friends),
	)
	now := int64(69_999)
	gateway.SetClock(func() int64 { return now })
	owner := openWebSocket(t, gateway.Handler())
	friend := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, owner, "owner-token")
	handshakeWebSocket(t, friend, "friend-token")

	writeEnvelope(t, owner, Envelope{Cmd: CommandEnterFarm, ClientSeq: 2, Payload: json.RawMessage(`{"owner_uid":0}`)})
	if got := readEnvelope(t, owner); got.Err != errcode.OK {
		t.Fatalf("owner EnterFarm = %#v", got)
	}
	now = 70_000
	writeEnvelope(t, friend, Envelope{Cmd: CommandEnterFarm, ClientSeq: 2, Payload: json.RawMessage(`{"owner_uid":42}`)})
	enter := readEnvelope(t, friend)
	var enterPayload enterFarmResponse
	if err := json.Unmarshal(enter.Payload, &enterPayload); err != nil {
		t.Fatalf("decode friend EnterFarm: %v", err)
	}
	if enter.Err != errcode.OK || enterPayload.FarmSeq != 1 {
		t.Fatalf("friend EnterFarm = %#v, payload = %#v", enter, enterPayload)
	}
	delta := readEnvelope(t, owner)
	var deltaPayload farm.FarmDelta
	if err := json.Unmarshal(delta.Payload, &deltaPayload); err != nil {
		t.Fatalf("decode FarmDelta: %v", err)
	}
	if delta.Cmd != CommandFarmDelta || deltaPayload.FarmSeq != 1 || len(deltaPayload.Plots) != 1 {
		t.Fatalf("advance delta = %#v, payload = %#v", delta, deltaPayload)
	}
	if deltas, ok := runtime.actors[ownerUID].Deltas.Since(1); !ok || len(deltas) != 1 {
		t.Fatalf("ring deltas = %#v, ok=%t, want one delta", deltas, ok)
	}
}

func TestEnterFarmBuffersDeltaUntilSnapshotResponse(t *testing.T) {
	const (
		ownerUID  = uint64(42)
		friendUID = uint64(7)
	)
	friends := newFriendStoreStub()
	friends.add(ownerUID, friendUID)
	var gateway *Gateway
	runtime := runtimeHookStub{
		actor: &room.FarmActor{Aggregate: farm.NewAggregate(ownerUID, "owner")},
		after: func() {
			gateway.rooms.Broadcast(farm.FarmDelta{OwnerUID: ownerUID, FarmSeq: 1})
		},
	}
	gateway = New(
		authStub{},
		sessionStub{uid: friendUID},
		runtime,
		WithFriendStore(friends),
	)
	connection := openWebSocket(t, gateway.Handler())
	handshakeWebSocket(t, connection, "token-42")

	writeEnvelope(t, connection, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   marshalPayload(enterFarmRequest{OwnerUID: clientjson.UID(ownerUID)}),
	})
	if response := readEnvelope(t, connection); response.Cmd != CommandEnterFarm || response.Err != errcode.OK {
		t.Fatalf("first response = %#v, want successful EnterFarm", response)
	}
	if delta := readEnvelope(t, connection); delta.Cmd != CommandFarmDelta || delta.Err != errcode.OK {
		t.Fatalf("second response = %#v, want FarmDelta after EnterFarm", delta)
	}
}

func TestEnterFarmRechecksFriendshipAfterSubscription(t *testing.T) {
	const (
		ownerUID  = uint64(42)
		friendUID = uint64(7)
	)
	friends := &revokeOnSecondReadFriendStore{friendStoreStub: newFriendStoreStub()}
	friends.add(ownerUID, friendUID)
	gateway := New(
		authStub{},
		sessionStub{uid: friendUID},
		runtimeStub{aggregate: farm.NewAggregate(ownerUID, "owner")},
		WithFriendStore(friends),
	)
	connection := &wsConnection{id: 1, uid: friendUID}

	response := gateway.handleEnterFarm(connection, Envelope{
		Cmd:     CommandEnterFarm,
		Payload: marshalPayload(enterFarmRequest{OwnerUID: clientjson.UID(ownerUID)}),
	})
	if response.Err != errcode.NotFriend {
		t.Fatalf("EnterFarm error = %d, want %d", response.Err, errcode.NotFriend)
	}
	if connection.roomUID != 0 {
		t.Fatalf("room UID = %d, want no subscription after friendship revocation", connection.roomUID)
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

func TestConnectionLimiterDisconnectsAfterFiveConsecutiveRejections(t *testing.T) {
	t.Parallel()

	limiter := newConnectionLimiter()
	for range connectionBurst {
		if !limiter.Allow() {
			t.Fatal("limiter rejected initial capacity")
		}
	}
	for range consecutiveRateLimitThreshold - 1 {
		if limiter.Allow() {
			t.Fatal("limiter accepted request beyond capacity")
		}
		if limiter.ShouldDisconnect() {
			t.Fatal("limiter requested disconnect before fifth rejection")
		}
	}
	if limiter.Allow() {
		t.Fatal("limiter accepted request beyond capacity")
	}
	if !limiter.ShouldDisconnect() {
		t.Fatal("limiter did not request disconnect after fifth rejection")
	}
}

func TestWebSocketHandshakeAcceptsResumeFields(t *testing.T) {
	t.Parallel()

	conn := openWebSocket(t, New(authStub{}, sessionStub{uid: 42}, runtimeStub{}).Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 7,
		Payload:   json.RawMessage(`{"token":"token-42","resume_farm_uid":42,"resume_farm_seq":99,"client_config_ver":1}`),
	})

	got := readEnvelope(t, conn)
	if got.Cmd != CommandHandshake || got.ClientSeq != 7 || got.Err != errcode.OK {
		t.Fatalf("handshake response = %#v", got)
	}
}

func TestWebSocketHandshakeRejectsInvalidTokenAndStaleConfig(t *testing.T) {
	t.Parallel()

	conn := openWebSocket(t, New(authStub{}, sessionStub{uid: 42}, runtimeStub{}).Handler())
	for _, test := range []struct {
		name      string
		clientSeq uint32
		payload   json.RawMessage
		wantErr   errcode.Code
	}{
		{
			name:      "invalid token",
			clientSeq: 8,
			payload:   json.RawMessage(`{"token":"invalid","client_config_ver":1}`),
			wantErr:   errcode.Unauthorized,
		},
		{
			name:      "stale config",
			clientSeq: 9,
			payload:   json.RawMessage(`{"token":"token-42","client_config_ver":0}`),
			wantErr:   errcode.ConfigStale,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeEnvelope(t, conn, Envelope{
				Cmd:       CommandHandshake,
				ClientSeq: test.clientSeq,
				Payload:   test.payload,
			})
			got := readEnvelope(t, conn)
			if got.Cmd != CommandHandshake || got.ClientSeq != test.clientSeq || got.Err != test.wantErr {
				t.Fatalf("handshake response = %#v, want error %d", got, test.wantErr)
			}
		})
	}
}

func TestWebSocketRateLimitResponsePreservesRequestHeader(t *testing.T) {
	t.Parallel()

	conn := openWebSocket(t, New(authStub{}, sessionStub{uid: 42}, runtimeStub{}).Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	if got := readEnvelope(t, conn); got.Err != errcode.OK {
		t.Fatalf("handshake response = %#v", got)
	}
	requests := make([]Envelope, 0, connectionBurst-1)
	for clientSeq := uint32(2); clientSeq <= connectionBurst; clientSeq++ {
		requests = append(requests, Envelope{
			Cmd:       CommandPing,
			ClientSeq: clientSeq,
			Payload:   json.RawMessage(`{"client_time":0}`),
		})
	}
	writeEnvelopes(t, conn, requests)
	for range requests {
		if got := readEnvelope(t, conn); got.Err != errcode.OK {
			t.Fatalf("Ping response = %#v", got)
		}
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandPing,
		ClientSeq: connectionBurst + 1,
		Payload:   json.RawMessage(`{"client_time":0}`),
	})
	got := readEnvelope(t, conn)
	if got.Cmd != CommandPing || got.ClientSeq != connectionBurst+1 || got.Err != errcode.RateLimited {
		t.Fatalf("rate limit response = %#v", got)
	}
}

func TestWebSocketRateLimitCanBeDisabledForCapacityTest(t *testing.T) {
	t.Parallel()

	conn := openWebSocket(t, New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{},
		WithWSRateLimitDisabled(),
	).Handler())
	handshakeWebSocket(t, conn, "token-42")
	for clientSeq := uint32(2); clientSeq <= connectionBurst+5; clientSeq++ {
		writeEnvelope(t, conn, Envelope{
			Cmd:       CommandPing,
			ClientSeq: clientSeq,
			Payload:   json.RawMessage(`{"client_time":0}`),
		})
		if got := readEnvelope(t, conn); got.Cmd != CommandPing || got.ClientSeq != clientSeq || got.Err != errcode.OK {
			t.Fatalf("Ping %d response = %#v", clientSeq, got)
		}
	}
}

func TestSearchUserReturnsExactMatchAndNotFound(t *testing.T) {
	t.Parallel()

	friends := newFriendStoreStub()
	friends.users["alice"] = searchUserStub{UID: 7, Nickname: "Alice's farm"}
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{},
		WithFriendStore(friends),
	)
	connection := &wsConnection{uid: 42, authed: true}

	t.Run("exact match", func(t *testing.T) {
		response := gateway.handleWSRequest(connection, Envelope{
			Cmd:     CommandSearchUser,
			Payload: json.RawMessage(`{"username":"alice"}`),
		})
		if response.Err != errcode.OK {
			t.Fatalf("SearchUser err = %d, want OK", response.Err)
		}
		var payload struct {
			Users []struct {
				UID      clientjson.UID `json:"uid"`
				Nickname string         `json:"nickname"`
			} `json:"users"`
		}
		if err := json.Unmarshal(response.Payload, &payload); err != nil {
			t.Fatalf("decode SearchUser payload: %v", err)
		}
		if len(payload.Users) != 1 || uint64(payload.Users[0].UID) != 7 || payload.Users[0].Nickname != "Alice's farm" {
			t.Fatalf("SearchUser payload = %#v", payload)
		}
	})

	t.Run("not found", func(t *testing.T) {
		response := gateway.handleWSRequest(connection, Envelope{
			Cmd:     CommandSearchUser,
			Payload: json.RawMessage(`{"username":"missing"}`),
		})
		if response.Err != errcode.UserNotFound {
			t.Fatalf("SearchUser err = %d, want %d", response.Err, errcode.UserNotFound)
		}
	})

	t.Run("self is returned for client-side self marker", func(t *testing.T) {
		friends.users["self"] = searchUserStub{UID: 42, Nickname: "me"}
		response := gateway.handleWSRequest(connection, Envelope{
			Cmd:     CommandSearchUser,
			Payload: json.RawMessage(`{"username":"self"}`),
		})
		if response.Err != errcode.OK {
			t.Fatalf("SearchUser self err = %d, want OK", response.Err)
		}
		var payload struct {
			Users []struct {
				UID      clientjson.UID `json:"uid"`
				Nickname string         `json:"nickname"`
			} `json:"users"`
		}
		if err := json.Unmarshal(response.Payload, &payload); err != nil {
			t.Fatalf("decode self SearchUser payload: %v", err)
		}
		if len(payload.Users) != 1 || uint64(payload.Users[0].UID) != 42 || payload.Users[0].Nickname != "me" {
			t.Fatalf("SearchUser self payload = %#v", payload)
		}
	})
}

func TestSearchUserIsLimitedByConnectionLimiter(t *testing.T) {
	t.Parallel()

	conn := openWebSocket(t, New(authStub{}, sessionStub{uid: 42}, runtimeStub{}).Handler())
	handshakeWebSocket(t, conn, "token-42")
	requests := make([]Envelope, 0, connectionBurst-1)
	for clientSeq := uint32(2); clientSeq <= connectionBurst; clientSeq++ {
		requests = append(requests, Envelope{
			Cmd:       CommandPing,
			ClientSeq: clientSeq,
			Payload:   json.RawMessage(`{"client_time":0}`),
		})
	}
	// Put the limited request in the same frame as the burst. Reading all ping
	// responses first gives the real-time token bucket enough time to refill
	// under -race, making the assertion scheduler-dependent.
	requests = append(requests, Envelope{
		Cmd:       CommandSearchUser,
		ClientSeq: connectionBurst + 1,
		Payload:   json.RawMessage(`{"username":"alice"}`),
	})
	writeEnvelopes(t, conn, requests)
	for index := range requests {
		if index == len(requests)-1 {
			break
		}
		if got := readEnvelope(t, conn); got.Err != errcode.OK {
			t.Fatalf("Ping response = %#v", got)
		}
	}
	if got := readEnvelope(t, conn); got.Cmd != CommandSearchUser || got.ClientSeq != connectionBurst+1 || got.Err != errcode.RateLimited {
		t.Fatalf("SearchUser rate-limit response = %#v", got)
	}
}

func TestWebSocketTillSuccessAndFailure(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	gw := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gw.SetClock(func() int64 { return 10_000 })

	conn := openWebSocket(t, gw.Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	if got := readEnvelope(t, conn); got.Err != errcode.OK {
		t.Fatalf("handshake = %#v", got)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandTill,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":0}`),
	})
	till := readEnvelope(t, conn)
	if till.Err != errcode.OK {
		t.Fatalf("Till = %#v", till)
	}
	if aggregate.Plots[0].State != farm.StateTilled {
		t.Fatalf("plot state = %d, want tilled", aggregate.Plots[0].State)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandTill,
		ClientSeq: 3,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":0}`),
	})
	again := readEnvelope(t, conn)
	if again.Err != errcode.PlotNotWasteland {
		t.Fatalf("second Till err = %d, want %d", again.Err, errcode.PlotNotWasteland)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandBuy,
		ClientSeq: 4,
		Payload:   json.RawMessage(`{"item_id":1,"quantity":1}`),
	})
	buy := readEnvelope(t, conn)
	if buy.Err != errcode.OK {
		t.Fatalf("Buy = %#v", buy)
	}
	if aggregate.Coin != 875 || aggregate.Items[farm.SeedItem(1)] != 1 {
		t.Fatalf("after buy coin=%d seeds=%d", aggregate.Coin, aggregate.Items[farm.SeedItem(1)])
	}
}

func TestPetCommandsActivateFeedAndReportStatus(t *testing.T) {
	aggregate := farm.NewAggregate(42, "owner")
	aggregate.Coin = 10_000
	aggregate.Level = 10
	if result := aggregate.Buy(farm.BuyReq{ItemID: farm.DogMuttShopItemID, Quantity: 1}); result.Err != errcode.OK {
		t.Fatalf("Buy dog = %d", result.Err)
	}
	if result := aggregate.Buy(farm.BuyReq{ItemID: farm.DogShepherdShopItemID, Quantity: 1}); result.Err != errcode.OK {
		t.Fatalf("Buy shepherd = %d", result.Err)
	}
	aggregate.Items[farm.DogFoodItem()] = 4
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gateway.SetClock(func() int64 { return 1_000 })
	connection := &wsConnection{uid: 42, authed: true}

	activate := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandPetActivate,
		Payload: json.RawMessage(`{"dog_type":2}`),
	})
	if activate.Err != errcode.OK {
		t.Fatalf("PetActivate = %#v", activate)
	}
	feed := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandPetFeed,
		Payload: json.RawMessage(`{"grams":4}`),
	})
	if feed.Err != errcode.OK {
		t.Fatalf("PetFeed = %#v", feed)
	}
	status := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandPetStatus,
		Payload: emptyPayload,
	})
	var got farm.PetStatus
	if err := json.Unmarshal(status.Payload, &got); err != nil {
		t.Fatalf("decode PetStatus: %v", err)
	}
	if status.Err != errcode.OK || got.ActiveDog != farm.DogShepherd || got.BowlGrams != 4 ||
		len(got.Dogs) != 2 || got.Dogs[1].DogType != farm.DogShepherd {
		t.Fatalf("PetStatus = %#v, payload=%#v", status, got)
	}
}

func TestVisitorStealRejectsInsufficientCompensationBeforePublishing(t *testing.T) {
	friends := newFriendStoreStub()
	friends.add(7, 42)
	visitor := farm.NewAggregate(7, "visitor")
	visitor.Coin = 169 // 白萝卜赔付为 17 × 10。
	runtime := multiRuntimeStub{actors: map[uint64]*room.FarmActor{
		7: {Aggregate: visitor},
	}}
	gateway := New(
		authStub{},
		sessionStub{uid: 7},
		runtime,
		WithFriendStore(friends),
		WithCrossFarmClient(noopCrossClient{}),
	)
	response := gateway.handleWSRequest(&wsConnection{uid: 7, authed: true, roomUID: 42}, Envelope{
		Cmd:     CommandSteal,
		Payload: json.RawMessage(`{"owner_uid":42,"plot_index":0,"crop_id":1}`),
	})
	if response.Err != errcode.StealNoAfford {
		t.Fatalf("Steal = %#v, want %d", response, errcode.StealNoAfford)
	}
	if visitor.Coin != 169 {
		t.Fatalf("visitor coin mutated after failed freeze: %d", visitor.Coin)
	}
}

func TestGatewayRoutesAuthoritativeCommandsThroughFarmRPC(t *testing.T) {
	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("parse route table: %v", err)
	}
	client := &farmRPCStub{responses: []farmrpc.CommandResponse{
		{
			Err: errcode.OK,
			Payload: marshalPayload(farmrpc.EnterFarmResponse{
				Snapshot:   farm.NewAggregate(42, "alice").Snapshot(),
				ServerTime: 123,
			}),
		},
		{
			Err:     errcode.OK,
			Payload: json.RawMessage(`{"farm_seq":1,"patch":{}}`),
		},
		{
			Err:     errcode.OK,
			Payload: json.RawMessage(`{"farm_seq":2,"patch":{}}`),
		},
		{
			Err:     errcode.OK,
			Payload: json.RawMessage(`{"farm_seq":2}`),
		},
		{
			Err:     errcode.OK,
			Payload: json.RawMessage(`{"active_dog":0,"owned":0}`),
		},
	}}
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		nil,
		WithFarmRPC(client, routes),
		WithConnectionRegistry(nil, "gateway-0"),
	)
	connection := &wsConnection{id: 73, uid: 42}

	enter := gateway.handleEnterFarm(connection, Envelope{
		Cmd:     CommandEnterFarm,
		Payload: json.RawMessage(`{"owner_uid":0}`),
	})
	plant := gateway.handlePlotOrShop(connection, Envelope{
		Cmd:     CommandPlant,
		Payload: json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
	})
	buy := gateway.handlePlotOrShop(connection, Envelope{
		Cmd:     CommandBuy,
		Payload: json.RawMessage(`{"item_id":1,"quantity":1}`),
	})
	sync := gateway.handleSyncFarm(connection, Envelope{
		Cmd:     CommandSyncFarm,
		Payload: json.RawMessage(`{"owner_uid":0,"from_seq":1}`),
	})
	pet := gateway.handlePet(connection, Envelope{
		Cmd:     CommandPetStatus,
		Payload: emptyPayload,
	})

	if enter.Err != errcode.OK || plant.Err != errcode.OK || buy.Err != errcode.OK ||
		sync.Err != errcode.OK || pet.Err != errcode.OK {
		t.Fatalf("responses = enter:%#v plant:%#v buy:%#v sync:%#v pet:%#v", enter, plant, buy, sync, pet)
	}
	if len(client.calls) != 5 {
		t.Fatalf("RPC calls = %d, want 5", len(client.calls))
	}
	for _, call := range client.calls {
		if call.farmID != "farm-0" || call.request.FarmUID != 42 ||
			call.request.Originator != (presence.ConnRef{ConnID: connection.id, GatewayID: "gateway-0"}) {
			t.Fatalf("RPC call = %#v", call)
		}
	}
	if client.calls[0].request.Operation != farmrpc.OperationEnterFarm {
		t.Fatalf("first operation = %q", client.calls[0].request.Operation)
	}
	if client.calls[1].request.Operation != farmrpc.OperationPlotAction {
		t.Fatalf("second operation = %q", client.calls[1].request.Operation)
	}
	if client.calls[2].request.Operation != farmrpc.OperationShop ||
		client.calls[3].request.Operation != farmrpc.OperationSyncFarm ||
		client.calls[4].request.Operation != farmrpc.OperationPet {
		t.Fatalf("routed operations = %#v", client.calls)
	}
}

func TestGatewayFarmRPCSharesTask4ClaimsAcrossLocalDays(t *testing.T) {
	const (
		uid   = uint64(42)
		token = "internal-token"
	)
	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("parse route table: %v", err)
	}

	dayOne := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	dayTwo := time.Date(2026, time.July, 31, 0, 0, 0, 0, time.Local).UnixMilli()
	now := dayOne
	claimer := &farmRPCTask4Claimer{}
	farmHandler := farmrpc.NewHandler(
		runtimeStub{aggregate: farm.NewAggregate(uid, "alice")},
		[]byte(token),
		func(farmUID uint64) bool { return farmUID == uid },
		func() int64 { return now },
		farmrpc.WithTaskClaimer(claimer),
	)
	owns := func(farmUID uint64) bool { return farmUID == uid }
	pair := grpcx.NewBufconnPair(t, token, func(server *grpc.Server) {
		farmrpc.RegisterCommandService(server, farmHandler, owns)
	})

	gateway := New(
		authStub{},
		sessionStub{uid: uid},
		nil,
		WithFarmRPC(farmrpc.NewGRPCClient(pair.Pool, map[string]string{"farm-0": "bufconn"}), routes),
		WithTaskMailStore(newTaskMailStoreStub()),
	)
	connection := &wsConnection{id: 7, uid: uid, authed: true}
	claimTask4 := func() Envelope {
		return gateway.handleWSRequest(connection, Envelope{
			Cmd:     CommandTaskClaim,
			Payload: json.RawMessage(`{"task_id":4}`),
		})
	}
	claimDailyLogin := func() Envelope {
		return gateway.handleWSRequest(connection, Envelope{
			Cmd:     CommandClaimDailyLogin,
			Payload: emptyPayload,
		})
	}

	if response := claimTask4(); response.Err != errcode.OK {
		t.Fatalf("day one TaskClaim(4) = %#v, want OK", response)
	}
	if response := claimDailyLogin(); response.Err != errcode.DuplicateOK {
		t.Fatalf("day one ClaimDailyLogin after TaskClaim(4) = %#v, want %d", response, errcode.DuplicateOK)
	}

	now = dayTwo
	if response := claimDailyLogin(); response.Err != errcode.OK {
		t.Fatalf("day two ClaimDailyLogin = %#v, want newly claimable task", response)
	}
	if response := claimTask4(); response.Err != errcode.TaskAlreadyClaimed {
		t.Fatalf("day two TaskClaim(4) after ClaimDailyLogin = %#v, want %d", response, errcode.TaskAlreadyClaimed)
	}
	if got := claimer.claimedDays(); !reflect.DeepEqual(got, []int64{
		gameconfig.LocalDayKey(dayOne), gameconfig.LocalDayKey(dayTwo),
	}) {
		t.Fatalf("FarmRPC successful Task 4 claims used days %#v", got)
	}
}

func TestGatewayReservesCrossActionThroughTypedCrossRPC(t *testing.T) {
	routes, err := sharding.ParseRouteTable([]byte(`{
		"logical_shards": 1024,
		"routes": [{"shard_start": 0, "shard_end": 1023, "farm_id": "farm-0"}]
	}`))
	if err != nil {
		t.Fatalf("parse route table: %v", err)
	}
	eventBus := &recordingCrossClient{}
	friends := newFriendStoreStub()
	friends.add(7, 42)
	client := &farmRPCStub{responses: []farmrpc.CommandResponse{{Err: errcode.OK}}}
	gateway := New(
		authStub{},
		sessionStub{uid: 7},
		nil,
		WithFarmRPC(client, routes),
		WithFriendStore(friends),
		WithCrossFarmClient(eventBus),
	)

	response := gateway.handleVisitorMutualAid(&wsConnection{id: 1, uid: 7, roomUID: 42}, Envelope{
		Cmd:       CommandWater,
		ClientSeq: 9,
		Payload:   json.RawMessage(`{"owner_uid":42,"plot_index":0,"arg":0}`),
	})

	if response.Cmd != 0 {
		t.Fatalf("cross response = %#v, want deferred response", response)
	}
	if eventBus.reserveCalls != 1 {
		t.Fatalf("typed cross reserve calls = %d, want 1", eventBus.reserveCalls)
	}
	if len(client.calls) != 0 {
		t.Fatalf("generic Farm RPC calls = %#v, want none", client.calls)
	}
	gateway.crossPending.Range(func(key, value any) bool {
		gateway.crossPending.Delete(key)
		return true
	})
}

func TestWebSocketFertilizeReturnsUpdatedPatch(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Items[farm.FertilizerItem(1)] = 1
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		StageCount:     3,
		SeasonStartAt:  10_000,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
		LastSettleAt:   10_000,
		LastWaterAt:    10_000,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	gw := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gw.SetClock(func() int64 { return 11_000 })

	conn := openWebSocket(t, gw.Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	if got := readEnvelope(t, conn); got.Err != errcode.OK {
		t.Fatalf("handshake = %#v", got)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandFertilize,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
	})
	got := readEnvelope(t, conn)
	if got.Err != errcode.OK {
		t.Fatalf("Fertilize = %#v", got)
	}
	if aggregate.Plots[0].MatureAt != 64_000 || aggregate.Items[farm.FertilizerItem(1)] != 0 {
		t.Fatalf("aggregate after fertilize = %#v, items=%v", aggregate.Plots[0], aggregate.Items)
	}
	var payload actionResponse
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("decode Fertilize payload: %v", err)
	}
	if payload.Patch.Plot == nil || payload.Patch.Plot.MatureAt != 64_000 {
		t.Fatalf("Fertilize patch = %#v", payload.Patch)
	}
}

func TestPlotActionRejectsArgOutsideUint16(t *testing.T) {
	t.Parallel()

	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{
		err: errors.New("runtime must not be called for invalid arg"),
	})
	connection := &wsConnection{uid: 42}

	for _, command := range []uint32{
		CommandTill,
		CommandClear,
		CommandPlant,
		CommandWater,
		CommandRemoveWeed,
		CommandRemovePest,
		CommandFertilize,
		CommandHarvest,
	} {
		t.Run(fmt.Sprintf("cmd_%d", command), func(t *testing.T) {
			response := gateway.handlePlotOrShop(connection, Envelope{
				Cmd:       command,
				ClientSeq: 1,
				Payload: marshalPayload(plotActionRequest{
					OwnerUID:  0,
					PlotIndex: 0,
					Arg:       0x1_0000,
				}),
			})

			if response.Err != errcode.BadRequest {
				t.Fatalf("response.Err = %d, want %d", response.Err, errcode.BadRequest)
			}
		})
	}
}

func TestVisitorCannotWriteOwnFarmUsingZeroOwnerUID(t *testing.T) {
	t.Parallel()

	visitorUID := uint64(7)
	aggregate := farm.NewAggregate(visitorUID, "visitor")
	gateway := New(authStub{}, sessionStub{uid: visitorUID}, runtimeStub{aggregate: aggregate})
	connection := &wsConnection{uid: visitorUID, roomUID: 42}

	for _, command := range []uint32{CommandTill, CommandBuy} {
		t.Run(fmt.Sprintf("cmd_%d", command), func(t *testing.T) {
			payload := marshalPayload(shopRequest{ItemID: 1, Quantity: 1})
			if command == CommandTill {
				payload = marshalPayload(plotActionRequest{OwnerUID: 0, PlotIndex: 0})
			}

			response := gateway.handlePlotOrShop(connection, Envelope{Cmd: command, Payload: payload})
			if response.Err != errcode.NotOwner {
				t.Fatalf("response.Err = %d, want %d", response.Err, errcode.NotOwner)
			}
		})
	}
	if aggregate.Plots[0].State != farm.StateWasteland || aggregate.Coin != 1_000 {
		t.Fatalf("visitor aggregate mutated: %#v", aggregate)
	}
}

func TestVisitorCanSellOwnFruitWhileVisiting(t *testing.T) {
	t.Parallel()

	const visitorUID = uint64(7)
	aggregate := farm.NewAggregate(visitorUID, "visitor")
	aggregate.Items[farm.FruitItem(1)] = 2
	gateway := New(authStub{}, sessionStub{uid: visitorUID}, runtimeStub{aggregate: aggregate})
	connection := &wsConnection{uid: visitorUID, roomUID: 42}

	response := gateway.handlePlotOrShop(connection, Envelope{
		Cmd:     CommandSell,
		Payload: marshalPayload(shopRequest{ItemID: 1, Quantity: 2}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("visitor selling own fruit = %d, want %d", response.Err, errcode.OK)
	}
	if aggregate.Coin != 1_034 {
		t.Fatalf("visitor coin = %d, want 1034", aggregate.Coin)
	}
	if _, ok := aggregate.Items[farm.FruitItem(1)]; ok {
		t.Fatalf("sold fruit remains in visitor inventory: %d", aggregate.Items[farm.FruitItem(1)])
	}
}

func TestSyncFarmAheadSequenceReturnsSnapshot(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "owner")
	aggregate.FarmSeq = 3
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	response := gateway.handleSyncFarm(&wsConnection{uid: 42}, Envelope{
		Cmd:     CommandSyncFarm,
		Payload: marshalPayload(syncFarmRequest{OwnerUID: 0, FromSeq: 4}),
	})

	if response.Err != errcode.OK {
		t.Fatalf("SyncFarm err = %d, want OK", response.Err)
	}
	var payload syncFarmResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode SyncFarm: %v", err)
	}
	if payload.Snapshot == nil || payload.FarmSeq != 3 || len(payload.Deltas) != 0 {
		t.Fatalf("SyncFarm payload = %#v, want snapshot at seq 3", payload)
	}
}

func TestSyncFarmAdvancesExpiredGrowingPlotAndReturnsMatureDelta(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "owner")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonIndex:    0,
		SeasonTotal:    1,
		SeasonStartAt:  10_000,
		SeasonDuration: 10_000,
		MatureAt:       20_000,
		LastSettleAt:   10_000,
		LastWaterAt:    19_999,
		WeedNextWin:    gameconfig.RiskWindowsPerSeason,
		PestNextWin:    gameconfig.RiskWindowsPerSeason,
	}
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gateway.SetClock(func() int64 { return 20_000 })

	response := gateway.handleSyncFarm(&wsConnection{uid: 42}, Envelope{
		Cmd:     CommandSyncFarm,
		Payload: marshalPayload(syncFarmRequest{OwnerUID: 0, FromSeq: 0}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("SyncFarm err = %d, want OK", response.Err)
	}
	var payload syncFarmResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode SyncFarm: %v", err)
	}
	if payload.FarmSeq != 1 || payload.ServerTime != 20_000 || len(payload.Deltas) != 1 {
		t.Fatalf("SyncFarm payload = %#v", payload)
	}
	change := payload.Deltas[0].Plots[0]
	if change.State != farm.StateMature || change.FinalYield == 0 ||
		change.SeasonStartAt != 10_000 || change.LastSettleAt != 20_000 {
		t.Fatalf("mature change = %#v", change)
	}
	if aggregate.Plots[0].State != farm.StateMature {
		t.Fatalf("aggregate plot state = %d, want Mature", aggregate.Plots[0].State)
	}
}

func TestFriendCommandsRejectSelfAndDuplicate(t *testing.T) {
	t.Parallel()

	const (
		selfUID      = uint64(42)
		peerUID      = uint64(7)
		inviteSecret = "gateway-invite-secret"
		now          = int64(1_000)
	)
	selfInvite, err := socialapi.IssueInvite(selfUID, now, []byte(inviteSecret))
	if err != nil {
		t.Fatalf("issue self invite: %v", err)
	}

	for _, test := range []struct {
		name    string
		request Envelope
		prepare func(*friendStoreStub)
		wantErr errcode.Code
	}{
		{
			name: "accept self invite",
			request: Envelope{
				Cmd:     CommandAcceptInvite,
				Payload: marshalPayload(acceptInviteRequest{Token: selfInvite}),
			},
			wantErr: errcode.CannotFriendSelf,
		},
		{
			name: "add existing friend",
			request: Envelope{
				Cmd:     CommandAddFriendByUID,
				Payload: marshalPayload(friendPeerRequest{PeerUID: clientjson.UID(peerUID)}),
			},
			prepare: func(friends *friendStoreStub) {
				friends.add(selfUID, peerUID)
			},
			wantErr: errcode.AlreadyFriend,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			friends := newFriendStoreStub()
			if test.prepare != nil {
				test.prepare(friends)
			}
			gateway := New(
				authStub{},
				sessionStub{uid: selfUID},
				runtimeStub{},
				WithFriendStore(friends),
				WithInviteSecret([]byte(inviteSecret)),
			)
			gateway.SetClock(func() int64 { return now })

			response := gateway.handleWSRequest(&wsConnection{uid: selfUID, authed: true}, test.request)
			if response.Err != test.wantErr {
				t.Fatalf("response.Err = %d, want %d", response.Err, test.wantErr)
			}
		})
	}
}

func TestAddFriendByUIDMapsMissingPlayerToBadRequest(t *testing.T) {
	const (
		selfUID = uint64(42)
		peerUID = uint64(7)
	)
	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{},
		WithFriendStore(friendStoreErrorStub{err: store.ErrPlayerNotFound}),
	)

	response := gateway.handleFriendRequest(&wsConnection{uid: selfUID}, Envelope{
		Cmd:     CommandAddFriendByUID,
		Payload: marshalPayload(friendPeerRequest{PeerUID: clientjson.UID(peerUID)}),
	})
	if response.Err != errcode.BadRequest {
		t.Fatalf("AddFriendByUID error = %d, want %d", response.Err, errcode.BadRequest)
	}
}

func TestFriendRequestFlowRequestAcceptReject(t *testing.T) {
	t.Parallel()

	const (
		alice = uint64(42)
		bob   = uint64(7)
	)
	friends := newFriendStoreStub()
	friends.nicknames[alice] = "Alice"
	friends.nicknames[bob] = "Bob"
	gateway := New(
		authStub{},
		sessionStub{uid: alice},
		runtimeStub{},
		WithFriendStore(friends),
	)

	aliceConn := &wsConnection{uid: alice, authed: true}
	bobConn := &wsConnection{uid: bob, authed: true}

	req := gateway.handleWSRequest(aliceConn, Envelope{
		Cmd:     CommandRequestFriend,
		Payload: json.RawMessage(`{"peer_uid":7}`),
	})
	if req.Err != errcode.OK {
		t.Fatalf("RequestFriend err = %d", req.Err)
	}
	dup := gateway.handleWSRequest(aliceConn, Envelope{
		Cmd:     CommandRequestFriend,
		Payload: json.RawMessage(`{"peer_uid":7}`),
	})
	if dup.Err != errcode.FriendRequestPending {
		t.Fatalf("duplicate RequestFriend err = %d, want %d", dup.Err, errcode.FriendRequestPending)
	}

	list := gateway.handleWSRequest(bobConn, Envelope{Cmd: CommandListFriendRequests, Payload: emptyPayload})
	if list.Err != errcode.OK {
		t.Fatalf("ListFriendRequests err = %d", list.Err)
	}
	var listPayload listFriendRequestsResponse
	if err := json.Unmarshal(list.Payload, &listPayload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listPayload.Requests) != 1 || uint64(listPayload.Requests[0].FromUID) != alice {
		t.Fatalf("incoming = %#v", listPayload)
	}

	rejectOther := gateway.handleWSRequest(bobConn, Envelope{
		Cmd:     CommandRejectFriendRequest,
		Payload: json.RawMessage(`{"from_uid":99}`),
	})
	if rejectOther.Err != errcode.FriendRequestNotFound {
		t.Fatalf("Reject missing err = %d", rejectOther.Err)
	}

	accept := gateway.handleWSRequest(bobConn, Envelope{
		Cmd:     CommandAcceptFriendRequest,
		Payload: json.RawMessage(`{"from_uid":42}`),
	})
	if accept.Err != errcode.OK {
		t.Fatalf("AcceptFriendRequest err = %d", accept.Err)
	}
	if !friends.has(alice, bob) {
		t.Fatalf("expected friendship after accept")
	}
}

func TestFriendCommandsListGenerateAcceptRemoveAndAdd(t *testing.T) {
	t.Parallel()

	const (
		selfUID      = uint64(42)
		existingUID  = uint64(7)
		inviterUID   = uint64(8)
		newFriendUID = uint64(9)
		inviteSecret = "gateway-invite-secret"
		now          = int64(1_000)
	)
	friends := newFriendStoreStub()
	friends.add(selfUID, existingUID)
	friends.nicknames[existingUID] = "existing"
	invite, err := socialapi.IssueInvite(inviterUID, now, []byte(inviteSecret))
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{},
		WithFriendStore(friends),
		WithInviteSecret([]byte(inviteSecret)),
	)
	gateway.SetClock(func() int64 { return now })
	connection := &wsConnection{uid: selfUID, authed: true}

	list := gateway.handleWSRequest(connection, Envelope{Cmd: CommandFriendList, Payload: emptyPayload})
	if list.Err != errcode.OK {
		t.Fatalf("FriendList error = %d", list.Err)
	}
	var listPayload friendListResponse
	if err := json.Unmarshal(list.Payload, &listPayload); err != nil {
		t.Fatalf("decode FriendList response: %v", err)
	}
	if len(listPayload.Friends) != 1 || uint64(listPayload.Friends[0].UID) != existingUID || listPayload.Friends[0].Nickname != "existing" {
		t.Fatalf("FriendList payload = %#v", listPayload)
	}

	share := gateway.handleWSRequest(connection, Envelope{Cmd: CommandGenShareLink, Payload: emptyPayload})
	if share.Err != errcode.OK {
		t.Fatalf("GenShareLink error = %d", share.Err)
	}
	var sharePayload genShareLinkResponse
	if err := json.Unmarshal(share.Payload, &sharePayload); err != nil {
		t.Fatalf("decode GenShareLink response: %v", err)
	}
	if !strings.HasPrefix(sharePayload.Path, "/i/") {
		t.Fatalf("share path = %q, want /i/ prefix", sharePayload.Path)
	}
	if inviter, code := socialapi.ParseInvite(strings.TrimPrefix(sharePayload.Path, "/i/"), []byte(inviteSecret), now); code != errcode.OK || inviter != selfUID {
		t.Fatalf("parse generated invite = (%d, %d), want (%d, %d)", inviter, code, selfUID, errcode.OK)
	}

	accept := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandAcceptInvite,
		Payload: marshalPayload(acceptInviteRequest{Token: invite}),
	})
	if accept.Err != errcode.OK || !friends.has(selfUID, inviterUID) {
		t.Fatalf("AcceptInvite response = %#v, friendship=%t", accept, friends.has(selfUID, inviterUID))
	}

	remove := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandRemoveFriend,
		Payload: marshalPayload(friendPeerRequest{PeerUID: clientjson.UID(existingUID)}),
	})
	if remove.Err != errcode.OK || friends.has(selfUID, existingUID) {
		t.Fatalf("RemoveFriend response = %#v, friendship=%t", remove, friends.has(selfUID, existingUID))
	}

	add := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandAddFriendByUID,
		Payload: marshalPayload(friendPeerRequest{PeerUID: clientjson.UID(newFriendUID)}),
	})
	if add.Err != errcode.OK || !friends.has(selfUID, newFriendUID) {
		t.Fatalf("AddFriendByUID response = %#v, friendship=%t", add, friends.has(selfUID, newFriendUID))
	}
}

func TestRemoveFriendRevokesBothRoomDirections(t *testing.T) {
	t.Parallel()

	const (
		selfUID = uint64(42)
		peerUID = uint64(7)
	)
	friends := newFriendStoreStub()
	friends.add(selfUID, peerUID)
	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{},
		WithFriendStore(friends),
	)
	var selfReceives, peerReceives int
	gateway.rooms.SubscribeViewer(peerUID, 1, selfUID, func(farm.FarmDelta, []byte) {
		selfReceives++
	})
	gateway.rooms.SubscribeViewer(selfUID, 2, peerUID, func(farm.FarmDelta, []byte) {
		peerReceives++
	})

	response := gateway.handleFriendRequest(&wsConnection{uid: selfUID}, Envelope{
		Cmd:     CommandRemoveFriend,
		Payload: marshalPayload(friendPeerRequest{PeerUID: clientjson.UID(peerUID)}),
	})
	if response.Err != errcode.OK {
		t.Fatalf("RemoveFriend err = %d, want OK", response.Err)
	}
	gateway.rooms.Broadcast(farm.FarmDelta{OwnerUID: peerUID, FarmSeq: 1})
	gateway.rooms.Broadcast(farm.FarmDelta{OwnerUID: selfUID, FarmSeq: 1})
	if selfReceives != 0 || peerReceives != 0 {
		t.Fatalf("room deliveries = self:%d peer:%d, want 0:0", selfReceives, peerReceives)
	}
}

func TestInviteLandingRedirectsToLoginWhenNoSession(t *testing.T) {
	t.Parallel()

	handler := New(authStub{}, sessionStub{uid: 42}, runtimeStub{}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/i/payload.signature", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusFound; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got := recorder.Header().Get("Location"); got != "/login?invite=payload.signature" {
		t.Fatalf("Location = %q, want %q", got, "/login?invite=payload.signature")
	}
}

func TestInviteLandingRedirectsToLoginWhenSessionMissingOrInvalid(t *testing.T) {
	t.Parallel()

	handler := New(authStub{}, sessionStub{uid: 42}, runtimeStub{}).Handler()

	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "no auth header", header: ""},
		{name: "invalid session token", header: "Bearer not-a-known-token"},
		{name: "malformed bearer scheme", header: "Token token-42"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/i/payload.signature", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if got, want := recorder.Code, http.StatusFound; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
			if got := recorder.Header().Get("Location"); got != "/login?invite=payload.signature" {
				t.Fatalf("Location = %q, want %q", got, "/login?invite=payload.signature")
			}
		})
	}
}

func TestInviteLandingAcceptsInviteWhenSessionValid(t *testing.T) {
	t.Parallel()

	const (
		selfUID      = uint64(42)
		inviterUID   = uint64(8)
		inviteSecret = "gateway-invite-secret"
		now          = int64(1_000)
	)
	friends := newFriendStoreStub()
	invite, err := socialapi.IssueInvite(inviterUID, now, []byte(inviteSecret))
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{},
		WithFriendStore(friends),
		WithInviteSecret([]byte(inviteSecret)),
	)
	gateway.SetClock(func() int64 { return now })
	handler := gateway.Handler()

	request := httptest.NewRequest(http.MethodGet, "/i/"+invite, nil)
	request.Header.Set("Authorization", "Bearer token-42")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	var response inviteLandingResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.OK || response.Err != errcode.OK {
		t.Fatalf("response = %#v, want ok=true err=0", response)
	}
	if !friends.has(selfUID, inviterUID) {
		t.Fatalf("friendship not created: pairs=%v", friends.pairs)
	}
}

func TestInviteLandingReturnsErrorCodeWhenInviteInvalid(t *testing.T) {
	t.Parallel()

	const (
		selfUID      = uint64(42)
		inviteSecret = "gateway-invite-secret"
		now          = int64(1_000)
	)
	friends := newFriendStoreStub()
	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{},
		WithFriendStore(friends),
		WithInviteSecret([]byte(inviteSecret)),
	)
	gateway.SetClock(func() int64 { return now })
	handler := gateway.Handler()

	request := httptest.NewRequest(http.MethodGet, "/i/garbage.sig", nil)
	request.Header.Set("Authorization", "Bearer token-42")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	var response inviteLandingResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.OK || response.Err != errcode.InviteInvalid {
		t.Fatalf("response = %#v, want ok=false err=%d", response, errcode.InviteInvalid)
	}
}

func openWebSocket(t *testing.T, handler http.Handler) *websocket.Conn {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return openWebSocketAt(t, server.URL)
}

func openWebSocketAt(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()

	endpoint, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	endpoint.Scheme = "ws"
	endpoint.Path = "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), http.Header{
		"Sec-WebSocket-Protocol": []string{BinarySubprotocol},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeEnvelope(t *testing.T, conn *websocket.Conn, envelope Envelope) {
	t.Helper()
	writeEnvelopes(t, conn, []Envelope{envelope})
}

func writeEnvelopes(t *testing.T, conn *websocket.Conn, envelopes []Envelope) {
	t.Helper()
	data, err := EncodeBinaryBatch(envelopes)
	if err != nil {
		t.Fatalf("EncodeBinaryBatch: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
}

func readEnvelope(t *testing.T, conn *websocket.Conn) Envelope {
	t.Helper()
	if pending, ok := readEnvelopePending.Load(conn); ok {
		queue := pending.([]Envelope)
		if len(queue) > 0 {
			next := queue[0]
			if len(queue) == 1 {
				readEnvelopePending.Delete(conn)
			} else {
				readEnvelopePending.Store(conn, queue[1:])
			}
			return next
		}
		readEnvelopePending.Delete(conn)
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("websocket message type = %d, want binary", messageType)
	}
	envelopes, err := DecodeBinaryBatch(data)
	if err != nil {
		t.Fatalf("DecodeBinaryBatch: %v", err)
	}
	if len(envelopes) == 0 {
		t.Fatal("empty binary batch")
	}
	if len(envelopes) > 1 {
		readEnvelopePending.Store(conn, envelopes[1:])
	}
	return envelopes[0]
}

var readEnvelopePending sync.Map // *websocket.Conn -> []Envelope

func handshakeWebSocket(t *testing.T, conn *websocket.Conn, token string) {
	t.Helper()
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   marshalPayload(handshakeRequest{Token: token, ClientConfigVer: 1}),
	})
	if got := readEnvelope(t, conn); got.Err != errcode.OK {
		t.Fatalf("handshake = %#v", got)
	}
}

type connectionRegistryBackend struct {
	mu    sync.Mutex
	zsets map[string]map[string]int64
}

func newConnectionRegistryBackend() *connectionRegistryBackend {
	return &connectionRegistryBackend{zsets: make(map[string]map[string]int64)}
}

func (b *connectionRegistryBackend) Upsert(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zsets[key] == nil {
		b.zsets[key] = make(map[string]int64)
	}
	for existing, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], existing)
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	return nil
}

func (b *connectionRegistryBackend) Claim(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zsets[key] == nil {
		b.zsets[key] = make(map[string]int64)
	}
	for existing, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], existing)
		}
	}
	if len(b.zsets[key]) > 0 {
		if _, renewing := b.zsets[key][member]; !renewing || len(b.zsets[key]) != 1 {
			return false, nil
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	return true, nil
}

func (b *connectionRegistryBackend) Replace(_ context.Context, key, member string, expiresAtUnixMilli, nowUnixMilli int64, _ time.Duration) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.zsets[key] == nil {
		b.zsets[key] = make(map[string]int64)
	}
	evicted := make([]string, 0, len(b.zsets[key]))
	for existing, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], existing)
			continue
		}
		if existing != member {
			evicted = append(evicted, existing)
			delete(b.zsets[key], existing)
		}
	}
	b.zsets[key][member] = expiresAtUnixMilli
	return evicted, nil
}

func (b *connectionRegistryBackend) Delete(_ context.Context, key, member string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.zsets[key], member)
	return nil
}

func (b *connectionRegistryBackend) AliveMembers(_ context.Context, key string, nowUnixMilli int64) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	alive := make([]string, 0, len(b.zsets[key]))
	for member, expiresAt := range b.zsets[key] {
		if expiresAt <= nowUnixMilli {
			delete(b.zsets[key], member)
			continue
		}
		alive = append(alive, member)
	}
	return alive, nil
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

type sessionMapStub map[string]uint64

func (sessionMapStub) Put(context.Context, string, uint64, time.Duration) error { return nil }

func (s sessionMapStub) Get(_ context.Context, token string) (uint64, error) {
	uid, ok := s[token]
	if !ok {
		return 0, store.ErrSessionNotFound
	}
	return uid, nil
}

func (sessionMapStub) Delete(context.Context, string) error { return nil }

type runtimeStub struct {
	aggregate *farm.Aggregate
	err       error
}

func (s runtimeStub) Do(_ uint64, fn func(*room.FarmActor) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(&room.FarmActor{Aggregate: s.aggregate})
}

type multiRuntimeStub struct {
	actors map[uint64]*room.FarmActor
}

func (s multiRuntimeStub) Do(uid uint64, fn func(*room.FarmActor) error) error {
	farmActor := s.actors[uid]
	if farmActor == nil {
		return errors.New("unexpected farm uid")
	}
	return fn(farmActor)
}

type runtimeHookStub struct {
	actor *room.FarmActor
	after func()
}

func (s runtimeHookStub) Do(_ uint64, fn func(*room.FarmActor) error) error {
	if err := fn(s.actor); err != nil {
		return err
	}
	if s.after != nil {
		s.after()
	}
	return nil
}

type farmRPCStub struct {
	responses []farmrpc.CommandResponse
	calls     []farmRPCCall
}

type farmRPCCall struct {
	farmID  string
	request farmrpc.CommandRequest
}

func (s *farmRPCStub) Execute(_ context.Context, farmID string, request farmrpc.CommandRequest) (farmrpc.CommandResponse, error) {
	s.calls = append(s.calls, farmRPCCall{farmID: farmID, request: request})
	if len(s.responses) == 0 {
		return farmrpc.CommandResponse{}, errors.New("unexpected Farm RPC command")
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

type farmRPCTask4Claimer struct {
	claimed map[[2]int64]bool
	days    []int64
}

func (s *farmRPCTask4Claimer) ClaimTask(_ context.Context, uid uint64, dayKey int64, taskID uint32) (store.TaskReward, error) {
	if taskID != store.TaskDailyLoginID {
		return store.TaskReward{}, errors.New("unexpected task ID")
	}
	key := [2]int64{int64(uid), dayKey}
	if s.claimed == nil {
		s.claimed = make(map[[2]int64]bool)
	}
	if s.claimed[key] {
		return store.TaskReward{}, store.ErrTaskAlreadyClaimed
	}
	s.claimed[key] = true
	s.days = append(s.days, dayKey)
	return store.TaskReward{Coin: 100}, nil
}

func (s *farmRPCTask4Claimer) claimedDays() []int64 {
	return append([]int64(nil), s.days...)
}

type friendStoreStub struct {
	pairs     map[[2]uint64]bool
	nicknames map[uint64]string
	users     map[string]searchUserStub
	requests  map[[2]uint64]int64 // [from,to] -> created_at
}

type searchUserStub struct {
	UID      uint64
	Nickname string
}

func newFriendStoreStub() *friendStoreStub {
	return &friendStoreStub{
		pairs:     make(map[[2]uint64]bool),
		nicknames: make(map[uint64]string),
		users:     make(map[string]searchUserStub),
		requests:  make(map[[2]uint64]int64),
	}
}

func (s *friendStoreStub) AreFriends(_ context.Context, a, b uint64) (bool, error) {
	return s.has(a, b), nil
}

func (s *friendStoreStub) AddFriends(_ context.Context, a, b uint64) error {
	if s.has(a, b) {
		return store.ErrAlreadyFriend
	}
	s.add(a, b)
	return nil
}

func (s *friendStoreStub) RemoveFriends(_ context.Context, a, b uint64) error {
	delete(s.pairs, friendPair(a, b))
	return nil
}

func (s *friendStoreStub) ListFriends(_ context.Context, uid uint64) ([]store.FriendRow, error) {
	var friends []store.FriendRow
	for pair := range s.pairs {
		peerUID := pair[0]
		if peerUID == uid {
			peerUID = pair[1]
		}
		if pair[0] == uid || pair[1] == uid {
			friends = append(friends, store.FriendRow{UID: peerUID, Nickname: s.nicknames[peerUID]})
		}
	}
	return friends, nil
}

func (s *friendStoreStub) FindUserByUsername(_ context.Context, username string) (store.UserSearchRow, error) {
	user, ok := s.users[username]
	if !ok {
		return store.UserSearchRow{}, store.ErrAccountNotFound
	}
	return store.UserSearchRow{UID: user.UID, Nickname: user.Nickname}, nil
}

func (s *friendStoreStub) CountFriends(_ context.Context, uid uint64) (int, error) {
	count := 0
	for pair := range s.pairs {
		if pair[0] == uid || pair[1] == uid {
			count++
		}
	}
	return count, nil
}

func (s *friendStoreStub) CreateFriendRequest(_ context.Context, fromUID, toUID uint64) error {
	if fromUID == toUID {
		return store.ErrCannotFriendSelf
	}
	if s.has(fromUID, toUID) {
		return store.ErrAlreadyFriend
	}
	if _, ok := s.requests[[2]uint64{toUID, fromUID}]; ok {
		s.add(fromUID, toUID)
		delete(s.requests, [2]uint64{toUID, fromUID})
		delete(s.requests, [2]uint64{fromUID, toUID})
		return nil
	}
	key := [2]uint64{fromUID, toUID}
	if _, ok := s.requests[key]; ok {
		return store.ErrFriendRequestPending
	}
	s.requests[key] = 1
	return nil
}

func (s *friendStoreStub) ListIncomingFriendRequests(_ context.Context, uid uint64) ([]store.FriendRequestRow, error) {
	var out []store.FriendRequestRow
	for pair, created := range s.requests {
		if pair[1] != uid {
			continue
		}
		out = append(out, store.FriendRequestRow{
			FromUID:   pair[0],
			Nickname:  s.nicknames[pair[0]],
			CreatedAt: created,
		})
	}
	return out, nil
}

func (s *friendStoreStub) AcceptFriendRequest(_ context.Context, toUID, fromUID uint64) error {
	key := [2]uint64{fromUID, toUID}
	if _, ok := s.requests[key]; !ok {
		return store.ErrFriendRequestNotFound
	}
	delete(s.requests, key)
	delete(s.requests, [2]uint64{toUID, fromUID})
	if !s.has(fromUID, toUID) {
		s.add(fromUID, toUID)
	}
	return nil
}

func (s *friendStoreStub) RejectFriendRequest(_ context.Context, toUID, fromUID uint64) error {
	key := [2]uint64{fromUID, toUID}
	if _, ok := s.requests[key]; !ok {
		return store.ErrFriendRequestNotFound
	}
	delete(s.requests, key)
	return nil
}

func (s *friendStoreStub) add(a, b uint64) {
	s.pairs[friendPair(a, b)] = true
}

func (s *friendStoreStub) has(a, b uint64) bool {
	return s.pairs[friendPair(a, b)]
}

type revokeOnSecondReadFriendStore struct {
	*friendStoreStub
	reads int
}

func (s *revokeOnSecondReadFriendStore) AreFriends(context.Context, uint64, uint64) (bool, error) {
	s.reads++
	return s.reads == 1, nil
}

type friendStoreErrorStub struct {
	err error
}

func (s friendStoreErrorStub) AreFriends(context.Context, uint64, uint64) (bool, error) {
	return false, s.err
}

func (s friendStoreErrorStub) AddFriends(context.Context, uint64, uint64) error {
	return s.err
}

func (s friendStoreErrorStub) RemoveFriends(context.Context, uint64, uint64) error {
	return s.err
}

func (s friendStoreErrorStub) ListFriends(context.Context, uint64) ([]store.FriendRow, error) {
	return nil, s.err
}

func (s friendStoreErrorStub) FindUserByUsername(context.Context, string) (store.UserSearchRow, error) {
	return store.UserSearchRow{}, s.err
}

func (s friendStoreErrorStub) CountFriends(context.Context, uint64) (int, error) {
	return 0, s.err
}

func (s friendStoreErrorStub) CreateFriendRequest(context.Context, uint64, uint64) error {
	return s.err
}

func (s friendStoreErrorStub) ListIncomingFriendRequests(context.Context, uint64) ([]store.FriendRequestRow, error) {
	return nil, s.err
}

func (s friendStoreErrorStub) AcceptFriendRequest(context.Context, uint64, uint64) error {
	return s.err
}

func (s friendStoreErrorStub) RejectFriendRequest(context.Context, uint64, uint64) error {
	return s.err
}

func friendPair(a, b uint64) [2]uint64 {
	if a < b {
		return [2]uint64{a, b}
	}
	return [2]uint64{b, a}
}

type stealHintStub struct {
	mu       sync.Mutex
	hints    map[uint64]bool
	setCalls []stealHintSetCall
	err      error
}

type stealHintSetCall struct {
	UID          uint64
	HasStealable bool
}

func newStealHintStub() *stealHintStub {
	return &stealHintStub{hints: make(map[uint64]bool)}
}

func (s *stealHintStub) SetStealHint(_ context.Context, uid uint64, hasStealable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.setCalls = append(s.setCalls, stealHintSetCall{UID: uid, HasStealable: hasStealable})
	if hasStealable {
		s.hints[uid] = true
	} else {
		delete(s.hints, uid)
	}
	return nil
}

func (s *stealHintStub) GetStealHints(_ context.Context, uids []uint64) (map[uint64]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[uint64]bool, len(uids))
	for _, uid := range uids {
		if s.hints[uid] {
			out[uid] = true
		}
	}
	return out, nil
}

func (s *stealHintStub) setCallsSnapshot() []stealHintSetCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stealHintSetCall(nil), s.setCalls...)
}

// TestFriendListSurfacesStealableHint 覆盖 Task 12 Step 1：
// 写入 steal hint 后，FriendList 必须在对应好友上返回 has_stealable=true。
func TestFriendListSurfacesStealableHint(t *testing.T) {
	t.Parallel()

	const (
		selfUID      = uint64(42)
		stealableUID = uint64(7)
		plainUID     = uint64(8)
	)
	friends := newFriendStoreStub()
	friends.add(selfUID, stealableUID)
	friends.add(selfUID, plainUID)
	friends.nicknames[stealableUID] = "stealable"
	friends.nicknames[plainUID] = "plain"

	hints := newStealHintStub()
	if err := hints.SetStealHint(context.Background(), stealableUID, true); err != nil {
		t.Fatalf("SetStealHint: %v", err)
	}

	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{},
		WithFriendStore(friends),
		WithStealHintStore(hints),
	)

	response := gateway.handleWSRequest(&wsConnection{uid: selfUID, authed: true}, Envelope{
		Cmd:     CommandFriendList,
		Payload: emptyPayload,
	})
	if response.Err != errcode.OK {
		t.Fatalf("FriendList err = %d, want OK", response.Err)
	}
	var payload friendListResponse
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode FriendList: %v", err)
	}
	byUID := make(map[uint64]bool, len(payload.Friends))
	for _, f := range payload.Friends {
		byUID[uint64(f.UID)] = f.HasStealable
	}
	if !byUID[stealableUID] {
		t.Fatalf("FriendList = %#v, want stealableUID has_stealable=true", payload)
	}
	if byUID[plainUID] {
		t.Fatalf("FriendList = %#v, want plainUID has_stealable=false", payload)
	}
}

// TestHarvestWritesStealableHint 验证本地动作后异步写 hint：
// 成熟地块被收获后，主人 hint 应被写为 false。
func TestHarvestWritesStealableHint(t *testing.T) {
	t.Parallel()

	const selfUID = uint64(42)
	agg := farm.NewAggregate(selfUID, "alice")
	agg.UnlockedPlots = 1
	agg.Plots[0] = farm.Plot{
		State:          farm.StateMature,
		CropID:         1,
		FinalYield:     10,
		HarvestRound:   1,
		SeasonDuration: 60_000,
		MatureAt:       1_000,
	}

	hints := newStealHintStub()
	gateway := New(
		authStub{},
		sessionStub{uid: selfUID},
		runtimeStub{aggregate: agg},
		WithStealHintStore(hints),
	)
	gateway.SetClock(func() int64 { return 1_000 })

	resp := gateway.handleWSRequest(&wsConnection{uid: selfUID, authed: true}, Envelope{
		Cmd:     CommandHarvest,
		Payload: marshalPayload(plotActionRequest{PlotIndex: 0}),
	})
	if resp.Err != errcode.OK {
		t.Fatalf("Harvest err = %d, want OK", resp.Err)
	}
	calls := hints.setCallsSnapshot()
	var sawFalse bool
	for _, c := range calls {
		if c.UID == selfUID && !c.HasStealable {
			sawFalse = true
		}
	}
	if !sawFalse {
		t.Fatalf("harvest did not write has_stealable=false; calls = %#v", calls)
	}
}

type crossOwnerBridge struct {
	owner *crossfarm.Owner
}

func (bridge *crossOwnerBridge) ReserveCrossVisitor(context.Context, crossfarm.CrossAction, uint32) (errcode.Code, error) {
	return errcode.OK, nil
}

func (bridge *crossOwnerBridge) ApplyCrossAction(ctx context.Context, action crossfarm.CrossAction) (crossfarm.CrossResult, error) {
	if bridge == nil || bridge.owner == nil {
		return crossfarm.CrossResult{}, errors.New("cross owner bridge is nil")
	}
	return bridge.owner.Apply(ctx, action)
}

func (bridge *crossOwnerBridge) AcknowledgeCrossResult(context.Context, uint64, uint64, uint64) error {
	return nil
}

func (bridge *crossOwnerBridge) DeliverCrossResult(context.Context, crossfarm.CrossResult) (crossfarm.VisitorReward, *farm.PlayerDelta, errcode.Code, error) {
	return crossfarm.VisitorReward{}, nil, errcode.Internal, errors.New("cross bridge settle is unavailable")
}

type noopCrossClient struct{}

func (noopCrossClient) ReserveCrossVisitor(context.Context, crossfarm.CrossAction, uint32) (errcode.Code, error) {
	return errcode.OK, nil
}

func (noopCrossClient) ApplyCrossAction(context.Context, crossfarm.CrossAction) (crossfarm.CrossResult, error) {
	return crossfarm.CrossResult{}, errors.New("noop cross apply")
}

func (noopCrossClient) AcknowledgeCrossResult(context.Context, uint64, uint64, uint64) error {
	return nil
}

func (noopCrossClient) DeliverCrossResult(context.Context, crossfarm.CrossResult) (crossfarm.VisitorReward, *farm.PlayerDelta, errcode.Code, error) {
	return crossfarm.VisitorReward{}, nil, errcode.Internal, errors.New("noop cross settle")
}

type recordingCrossClient struct {
	reserveCalls int
}

func (client *recordingCrossClient) ReserveCrossVisitor(context.Context, crossfarm.CrossAction, uint32) (errcode.Code, error) {
	client.reserveCalls++
	return errcode.OK, nil
}

func (*recordingCrossClient) ApplyCrossAction(context.Context, crossfarm.CrossAction) (crossfarm.CrossResult, error) {
	return crossfarm.CrossResult{}, errors.New("stop after reserve")
}

func (*recordingCrossClient) DeliverCrossResult(context.Context, crossfarm.CrossResult) (crossfarm.VisitorReward, *farm.PlayerDelta, errcode.Code, error) {
	return crossfarm.VisitorReward{}, nil, errcode.Internal, errors.New("unused")
}

func (*recordingCrossClient) AcknowledgeCrossResult(context.Context, uint64, uint64, uint64) error {
	return nil
}
