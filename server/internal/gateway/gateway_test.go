package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"farm/server/internal/social"
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

func TestEnterFarmAdvancesExpiredGrowingPlotBeforeSnapshot(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		SeasonDuration: 1_000,
		MatureAt:       20_000,
		LastSettleAt:   19_000,
		LastWaterAt:    19_000,
	}
	gateway := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gateway.SetClock(func() int64 { return 20_000 })

	conn := openWebSocket(t, gateway.Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	if got := readEnvelope(t, conn); got.Err != pkgerr.OK {
		t.Fatalf("handshake = %#v", got)
	}
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandEnterFarm,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})

	got := readEnvelope(t, conn)
	if got.Err != pkgerr.OK {
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
	if got.Cmd != CommandHandshake || got.ClientSeq != 7 || got.Err != pkgerr.OK {
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
		wantErr   pkgerr.Code
	}{
		{
			name:      "invalid token",
			clientSeq: 8,
			payload:   json.RawMessage(`{"token":"invalid","client_config_ver":1}`),
			wantErr:   pkgerr.Unauthorized,
		},
		{
			name:      "stale config",
			clientSeq: 9,
			payload:   json.RawMessage(`{"token":"token-42","client_config_ver":0}`),
			wantErr:   pkgerr.ConfigStale,
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
	if got := readEnvelope(t, conn); got.Err != pkgerr.OK {
		t.Fatalf("handshake response = %#v", got)
	}
	for clientSeq := uint32(2); clientSeq <= connectionBurst; clientSeq++ {
		writeEnvelope(t, conn, Envelope{
			Cmd:       CommandPing,
			ClientSeq: clientSeq,
			Payload:   json.RawMessage(`{"client_time":0}`),
		})
		if got := readEnvelope(t, conn); got.Err != pkgerr.OK {
			t.Fatalf("Ping response = %#v", got)
		}
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandPing,
		ClientSeq: connectionBurst + 1,
		Payload:   json.RawMessage(`{"client_time":0}`),
	})
	got := readEnvelope(t, conn)
	if got.Cmd != CommandPing || got.ClientSeq != connectionBurst+1 || got.Err != pkgerr.RateLimited {
		t.Fatalf("rate limit response = %#v", got)
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
	if got := readEnvelope(t, conn); got.Err != pkgerr.OK {
		t.Fatalf("handshake = %#v", got)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandTill,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":0}`),
	})
	till := readEnvelope(t, conn)
	if till.Err != pkgerr.OK {
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
	if again.Err != pkgerr.PlotNotWasteland {
		t.Fatalf("second Till err = %d, want %d", again.Err, pkgerr.PlotNotWasteland)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandBuy,
		ClientSeq: 4,
		Payload:   json.RawMessage(`{"item_id":1,"quantity":1}`),
	})
	buy := readEnvelope(t, conn)
	if buy.Err != pkgerr.OK {
		t.Fatalf("Buy = %#v", buy)
	}
	if aggregate.Coin != 875 || aggregate.Items[farm.SeedItem(1)] != 1 {
		t.Fatalf("after buy coin=%d seeds=%d", aggregate.Coin, aggregate.Items[farm.SeedItem(1)])
	}
}

func TestWebSocketFertilizeReturnsUpdatedPatch(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Items[farm.FertilizerItem(1)] = 1
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateGrowing,
		CropID:         1,
		StageCount:     3,
		SeasonDuration: 60_000,
		MatureAt:       70_000,
		LastSettleAt:   10_000,
		LastWaterAt:    10_000,
	}
	gw := New(authStub{}, sessionStub{uid: 42}, runtimeStub{aggregate: aggregate})
	gw.SetClock(func() int64 { return 11_000 })

	conn := openWebSocket(t, gw.Handler())
	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandHandshake,
		ClientSeq: 1,
		Payload:   json.RawMessage(`{"token":"token-42","client_config_ver":1}`),
	})
	if got := readEnvelope(t, conn); got.Err != pkgerr.OK {
		t.Fatalf("handshake = %#v", got)
	}

	writeEnvelope(t, conn, Envelope{
		Cmd:       CommandFertilize,
		ClientSeq: 2,
		Payload:   json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
	})
	got := readEnvelope(t, conn)
	if got.Err != pkgerr.OK {
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

			if response.Err != pkgerr.BadRequest {
				t.Fatalf("response.Err = %d, want %d", response.Err, pkgerr.BadRequest)
			}
		})
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
	selfInvite, err := social.IssueInvite(selfUID, now, []byte(inviteSecret))
	if err != nil {
		t.Fatalf("issue self invite: %v", err)
	}

	for _, test := range []struct {
		name    string
		request Envelope
		prepare func(*friendStoreStub)
		wantErr pkgerr.Code
	}{
		{
			name: "accept self invite",
			request: Envelope{
				Cmd:     CommandAcceptInvite,
				Payload: marshalPayload(acceptInviteRequest{Token: selfInvite}),
			},
			wantErr: pkgerr.CannotFriendSelf,
		},
		{
			name: "add existing friend",
			request: Envelope{
				Cmd:     CommandAddFriendByUID,
				Payload: marshalPayload(friendPeerRequest{PeerUID: peerUID}),
			},
			prepare: func(friends *friendStoreStub) {
				friends.add(selfUID, peerUID)
			},
			wantErr: pkgerr.AlreadyFriend,
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
	invite, err := social.IssueInvite(inviterUID, now, []byte(inviteSecret))
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
	if list.Err != pkgerr.OK {
		t.Fatalf("FriendList error = %d", list.Err)
	}
	var listPayload friendListResponse
	if err := json.Unmarshal(list.Payload, &listPayload); err != nil {
		t.Fatalf("decode FriendList response: %v", err)
	}
	if len(listPayload.Friends) != 1 || listPayload.Friends[0].UID != existingUID || listPayload.Friends[0].Nickname != "existing" {
		t.Fatalf("FriendList payload = %#v", listPayload)
	}

	share := gateway.handleWSRequest(connection, Envelope{Cmd: CommandGenShareLink, Payload: emptyPayload})
	if share.Err != pkgerr.OK {
		t.Fatalf("GenShareLink error = %d", share.Err)
	}
	var sharePayload genShareLinkResponse
	if err := json.Unmarshal(share.Payload, &sharePayload); err != nil {
		t.Fatalf("decode GenShareLink response: %v", err)
	}
	if !strings.HasPrefix(sharePayload.Path, "/i/") {
		t.Fatalf("share path = %q, want /i/ prefix", sharePayload.Path)
	}
	if inviter, code := social.ParseInvite(strings.TrimPrefix(sharePayload.Path, "/i/"), []byte(inviteSecret), now); code != pkgerr.OK || inviter != selfUID {
		t.Fatalf("parse generated invite = (%d, %d), want (%d, %d)", inviter, code, selfUID, pkgerr.OK)
	}

	accept := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandAcceptInvite,
		Payload: marshalPayload(acceptInviteRequest{Token: invite}),
	})
	if accept.Err != pkgerr.OK || !friends.has(selfUID, inviterUID) {
		t.Fatalf("AcceptInvite response = %#v, friendship=%t", accept, friends.has(selfUID, inviterUID))
	}

	remove := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandRemoveFriend,
		Payload: marshalPayload(friendPeerRequest{PeerUID: existingUID}),
	})
	if remove.Err != pkgerr.OK || friends.has(selfUID, existingUID) {
		t.Fatalf("RemoveFriend response = %#v, friendship=%t", remove, friends.has(selfUID, existingUID))
	}

	add := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandAddFriendByUID,
		Payload: marshalPayload(friendPeerRequest{PeerUID: newFriendUID}),
	})
	if add.Err != pkgerr.OK || !friends.has(selfUID, newFriendUID) {
		t.Fatalf("AddFriendByUID response = %#v, friendship=%t", add, friends.has(selfUID, newFriendUID))
	}
}

func TestInviteLandingReturnsLoginGuidance(t *testing.T) {
	t.Parallel()

	handler := New(authStub{}, sessionStub{}, runtimeStub{}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/i/payload.signature", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response struct {
		OK        bool   `json:"ok"`
		NeedLogin bool   `json:"need_login"`
		Token     string `json:"token"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.OK || !response.NeedLogin || response.Token != "payload.signature" {
		t.Fatalf("response = %#v", response)
	}
}

func openWebSocket(t *testing.T, handler http.Handler) *websocket.Conn {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	endpoint.Scheme = "ws"
	endpoint.Path = "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), http.Header{
		"Sec-WebSocket-Protocol": []string{JSONSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
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

type friendStoreStub struct {
	pairs     map[[2]uint64]bool
	nicknames map[uint64]string
}

func newFriendStoreStub() *friendStoreStub {
	return &friendStoreStub{
		pairs:     make(map[[2]uint64]bool),
		nicknames: make(map[uint64]string),
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

func (s *friendStoreStub) CountFriends(_ context.Context, uid uint64) (int, error) {
	count := 0
	for pair := range s.pairs {
		if pair[0] == uid || pair[1] == uid {
			count++
		}
	}
	return count, nil
}

func (s *friendStoreStub) add(a, b uint64) {
	s.pairs[friendPair(a, b)] = true
}

func (s *friendStoreStub) has(a, b uint64) bool {
	return s.pairs[friendPair(a, b)]
}

func friendPair(a, b uint64) [2]uint64 {
	if a < b {
		return [2]uint64{a, b}
	}
	return [2]uint64{b, a}
}
