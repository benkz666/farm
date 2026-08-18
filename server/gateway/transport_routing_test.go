package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/sharding"
	"farm/server/shared/store"
)

type testAuthenticator struct {
	register func(context.Context, string, string) (uint64, string, error)
	login    func(context.Context, string, string) (uint64, string, error)
}

func (auth testAuthenticator) Register(ctx context.Context, username, password string) (uint64, string, error) {
	if auth.register == nil {
		return 0, "", errors.New("unexpected register")
	}
	return auth.register(ctx, username, password)
}

func (auth testAuthenticator) Login(ctx context.Context, username, password string) (uint64, string, error) {
	if auth.login == nil {
		return 0, "", errors.New("unexpected login")
	}
	return auth.login(ctx, username, password)
}

type testSessions map[string]uint64

func (testSessions) Put(context.Context, string, uint64, time.Duration) error { return nil }
func (sessions testSessions) Get(_ context.Context, token string) (uint64, error) {
	uid, ok := sessions[token]
	if !ok {
		return 0, store.ErrSessionNotFound
	}
	return uid, nil
}
func (testSessions) Delete(context.Context, string) error { return nil }

type testFarmClient struct {
	farmID  string
	request *farmv1.ClientCommandRequest
	respond func(*farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse
}

func (client *testFarmClient) Execute(_ context.Context, farmID string, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	client.farmID, client.request = farmID, request
	if client.respond != nil {
		return client.respond(request), nil
	}
	return &farmv1.ClientCommandResponse{Envelope: commandResponseFor(request.Envelope, errcode.OK)}, nil
}

type testSocialClient struct {
	request *farmv1.ClientCommandRequest
}

func (client *testSocialClient) ExecuteClientCommand(_ context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	client.request = request
	return &farmv1.ClientCommandResponse{Envelope: commandResponseFor(request.Envelope, errcode.OK)}, nil
}

func testRoutes(t *testing.T) *sharding.RouteTable {
	t.Helper()
	routes, err := sharding.ParseRouteTable([]byte(`{"logical_shards":1024,"routes":[{"shard_start":0,"shard_end":1023,"farm_id":"farm-0"}]}`))
	if err != nil {
		t.Fatalf("ParseRouteTable: %v", err)
	}
	return routes
}

func commandRequestEnvelope(command, sequence uint32) *publicv3.WireEnvelope {
	return &publicv3.WireEnvelope{
		Cmd: command, ClientSeq: sequence,
		Payload: &publicv3.WireEnvelope_CommandRequest{CommandRequest: &publicv3.CommandRequest{}},
	}
}

func commandResponseFor(request *publicv3.WireEnvelope, code errcode.Code) *publicv3.WireEnvelope {
	return &publicv3.WireEnvelope{
		Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: int32(code),
		Payload: &publicv3.WireEnvelope_CommandResponse{CommandResponse: &publicv3.CommandResponse{}},
	}
}

func authenticatedConnection(gateway *Gateway, uid uint64) *wsConnection {
	connection := &wsConnection{id: 7, uid: uid, token: "session", authed: true, limiter: newConnectionLimiter()}
	connection.nextAuthValidationAt.Store(time.Now().Add(time.Hour).UnixNano())
	gateway.connections.Store(connection.id, connection)
	return connection
}

func TestGatewayKeepsHandshakeLocal(t *testing.T) {
	gateway := New(nil, testSessions{"session": 42})
	connection := &wsConnection{id: 7, limiter: newConnectionLimiter()}
	request := commandRequestEnvelope(CommandHandshake, 1)
	request.GetCommandRequest().AuthToken = "session"
	request.GetCommandRequest().ClientConfigVer = gameconfig.ConfigVer

	response, _, disconnect := gateway.dispatchWireRequest(context.Background(), connection, request)
	if disconnect || response.GetErr() != int32(errcode.OK) || response.GetCommandResponse().GetUid() != 42 {
		t.Fatalf("handshake response=%v disconnect=%v", response, disconnect)
	}
	if !connection.authed || connection.uid != 42 {
		t.Fatalf("connection not authenticated: %#v", connection)
	}
}

func TestGatewayRoutesTypedCommandsWithoutBusinessTranslation(t *testing.T) {
	farmClient := &testFarmClient{}
	socialClient := &testSocialClient{}
	gateway := New(nil, testSessions{"session": 42},
		WithFarmRPC(farmClient, testRoutes(t)), WithSocialRPC(socialClient), WithWSRateLimitDisabled(),
	)
	connection := authenticatedConnection(gateway, 42)

	water := commandRequestEnvelope(CommandWater, 2)
	water.GetCommandRequest().OwnerUid = 99 // Farm, not Gateway, decides whether this is legal.
	water.GetCommandRequest().PlotIndex = 3
	response, _, disconnect := gateway.dispatchWireRequest(context.Background(), connection, water)
	if disconnect || response.GetErr() != int32(errcode.OK) {
		t.Fatalf("Water response=%v disconnect=%v", response, disconnect)
	}
	if farmClient.farmID != "farm-0" || farmClient.request.GetUid() != 42 || farmClient.request.GetRouteUid() != 42 || farmClient.request.Envelope != water {
		t.Fatalf("Farm request was translated or misrouted: farm=%q request=%v", farmClient.farmID, farmClient.request)
	}

	search := commandRequestEnvelope(CommandSearchUser, 3)
	search.GetCommandRequest().Username = "alice"
	response, _, disconnect = gateway.dispatchWireRequest(context.Background(), connection, search)
	if disconnect || response.GetErr() != int32(errcode.OK) || socialClient.request.Envelope != search || socialClient.request.GetUid() != 42 {
		t.Fatalf("Social route response=%v request=%v", response, socialClient.request)
	}
}

func TestGatewayAppliesOnlyFarmRoomDirective(t *testing.T) {
	farmClient := &testFarmClient{respond: func(request *farmv1.ClientCommandRequest) *farmv1.ClientCommandResponse {
		return &farmv1.ClientCommandResponse{
			Envelope:   commandResponseFor(request.Envelope, errcode.OK),
			RoomAction: farmv1.RoomAction_ROOM_ACTION_SUBSCRIBE,
			RoomUid:    99, RoomSeq: 12,
		}
	}}
	gateway := New(nil, testSessions{"session": 42},
		WithFarmRPC(farmClient, testRoutes(t)), WithSocialRPC(&testSocialClient{}), WithWSRateLimitDisabled(),
	)
	connection := authenticatedConnection(gateway, 42)
	request := &publicv3.WireEnvelope{
		Cmd: CommandEnterFarm, ClientSeq: 2,
		Payload: &publicv3.WireEnvelope_EnterFarmRequest{EnterFarmRequest: &publicv3.EnterFarmRequest{OwnerUid: 99}},
	}
	response, _, _ := gateway.dispatchWireRequest(context.Background(), connection, request)
	if response.GetErr() != int32(errcode.OK) || connection.currentRoom() != 99 {
		t.Fatalf("room directive response=%v room=%d", response, connection.currentRoom())
	}
	connection.roomMu.Lock()
	seq, known := connection.roomSeq, connection.roomSeqKnown
	connection.roomMu.Unlock()
	if !known || seq != 12 {
		t.Fatalf("room watermark=(%d,%v), want (12,true)", seq, known)
	}
}

func TestGatewayServesFreshCaughtUpSelfSyncFromRoomWatermark(t *testing.T) {
	farmClient := &testFarmClient{}
	gateway := New(nil, testSessions{"session": 42},
		WithFarmRPC(farmClient, testRoutes(t)), WithSocialRPC(&testSocialClient{}), WithWSRateLimitDisabled(),
	)
	connection := authenticatedConnection(gateway, 42)
	connection.roomUID = 42
	connection.roomSeq = 12
	connection.roomSeqKnown = true
	connection.roomSeqObservedAt = time.Now().UnixNano()
	request := &publicv3.WireEnvelope{
		Cmd: CommandSyncFarm, ClientSeq: 2,
		Payload: &publicv3.WireEnvelope_SyncFarmRequest{
			SyncFarmRequest: &publicv3.SyncFarmRequest{OwnerUid: 42, FromSeq: 12},
		},
	}

	response, _, disconnect := gateway.dispatchWireRequest(context.Background(), connection, request)
	if disconnect || response.GetErr() != int32(errcode.OK) ||
		response.PreparedField != clientwire.PreparedSyncFarmResponse || len(response.PreparedPayload) == 0 {
		t.Fatalf("local Sync response=%#v disconnect=%v", response, disconnect)
	}
	if farmClient.request != nil {
		t.Fatalf("fresh caught-up Sync unexpectedly reached Farm: %#v", farmClient.request)
	}

	connection.roomSeqObservedAt = time.Now().Add(-roomWatermarkFreshness - time.Millisecond).UnixNano()
	request.ClientSeq++
	response, _, disconnect = gateway.dispatchWireRequest(context.Background(), connection, request)
	if disconnect || response.GetErr() != int32(errcode.OK) || farmClient.request == nil {
		t.Fatalf("stale Sync response=%#v request=%#v disconnect=%v", response, farmClient.request, disconnect)
	}
}

func TestGatewayAuthHTTPRemainsJSONAndUsesStringUID(t *testing.T) {
	const uid = uint64(9_007_199_254_740_993)
	auth := testAuthenticator{login: func(_ context.Context, username, password string) (uint64, string, error) {
		if username != "alice" || password != "secret" {
			return 0, "", errors.New("unexpected credentials")
		}
		return uid, "token", nil
	}}
	gateway := New(auth, testSessions{})
	request := httptest.NewRequest(http.MethodPost, "http://farm.test/api/login", bytes.NewBufferString(`{"username":"alice","password":"secret"}`))
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["uid"] != "9007199254740993" || body["token"] != "token" {
		t.Fatalf("response=%v", body)
	}
}
