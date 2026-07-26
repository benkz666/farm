// Command smoke exercises the phase 2 HTTP and WebSocket planting loop.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/gateway"
	"farm/server/internal/pkgerr"
)

const (
	defaultBaseURL = "http://127.0.0.1:9002"
	smokePassword  = "smoke-password"
)

type authResponse struct {
	UID   uint64 `json:"uid"`
	Token string `json:"token"`
	WSURL string `json:"ws_url"`
}

type enterFarmPayload struct {
	Snapshot farm.FarmSnapshotJSON `json:"snapshot"`
}

type actionPayload struct {
	FarmSeq uint64         `json:"farm_seq"`
	Patch   farm.PatchJSON `json:"patch"`
}

type enterFarmResponse struct {
	Snapshot   farm.FarmSnapshotJSON `json:"snapshot"`
	FarmSeq    uint64                `json:"farm_seq"`
	ServerTime int64                 `json:"server_time"`
	Relation   string                `json:"relation"`
}

type syncFarmResponse struct {
	Deltas   []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq  uint64                 `json:"farm_seq"`
}

func main() {
	mode := "planting"
	if env := os.Getenv("FARM_SMOKE_MODE"); env != "" {
		mode = env
	} else if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if err := run(mode); err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
	switch mode {
	case "friends":
		fmt.Println("smoke: friends gen/accept/list/duplicate-add passed")
	case "room":
		fmt.Println("smoke: room enter/delta/sync passed")
	case "all":
		fmt.Println("smoke: planting + friends + room passed")
	default:
		fmt.Println("smoke: planting buy/till/plant/advance/harvest/sell passed")
	}
}

func run(mode string) error {
	baseURL := strings.TrimRight(getenv("FARM_SMOKE_BASE_URL", defaultBaseURL), "/")
	switch mode {
	case "friends":
		return runFriends(baseURL)
	case "room":
		return runRoom(baseURL)
	case "all":
		if err := runPlanting(baseURL); err != nil {
			return err
		}
		if err := runFriends(baseURL); err != nil {
			return err
		}
		return runRoom(baseURL)
	default:
		return runPlanting(baseURL)
	}
}

func runPlanting(baseURL string) error {
	username, err := smokeUsername()
	if err != nil {
		return err
	}

	if _, err := authenticate(baseURL+"/api/register", username, smokePassword); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	login, err := authenticate(baseURL+"/api/login", username, smokePassword)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if login.UID == 0 || login.Token == "" || login.WSURL == "" {
		return fmt.Errorf("login returned incomplete credentials")
	}

	conn, response, err := websocket.DefaultDialer.Dial(login.WSURL, http.Header{
		"Sec-WebSocket-Protocol": []string{gateway.JSONSubprotocol},
	})
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()
	if response.Header.Get("Sec-WebSocket-Protocol") != gateway.JSONSubprotocol {
		return fmt.Errorf("websocket subprotocol was not negotiated")
	}

	seq := uint32(1)
	if err := exchange(conn, gateway.Envelope{
		Cmd:       gateway.CommandHandshake,
		ClientSeq: seq,
		Payload: mustJSON(map[string]any{
			"token":             login.Token,
			"client_config_ver": gameconf.ConfigVer,
		}),
	}); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	seq++

	enterEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seq,
		Payload:   json.RawMessage(`{"owner_uid":0}`),
	})
	if err != nil {
		return fmt.Errorf("enter farm: %w", err)
	}
	seq++
	var enter enterFarmPayload
	if err := json.Unmarshal(enterEnv.Payload, &enter); err != nil {
		return fmt.Errorf("decode EnterFarm payload: %w", err)
	}
	if enter.Snapshot.Coin != gameconf.InitialCoin {
		return fmt.Errorf("coin = %d, want %d", enter.Snapshot.Coin, gameconf.InitialCoin)
	}

	crop, ok := gameconf.CropByID(1)
	if !ok {
		return fmt.Errorf("missing white radish crop config")
	}

	if _, err := mustAction(conn, &seq, gateway.CommandBuy, map[string]any{
		"item_id":  1,
		"quantity": 1,
	}); err != nil {
		return fmt.Errorf("buy: %w", err)
	}
	if _, err := mustAction(conn, &seq, gateway.CommandTill, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 0,
	}); err != nil {
		return fmt.Errorf("till: %w", err)
	}
	if _, err := mustAction(conn, &seq, gateway.CommandPlant, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 1,
	}); err != nil {
		return fmt.Errorf("plant: %w", err)
	}

	// 水分窗边界各浇一次，保证满产；再推进到成熟。
	seasonMS := int64(crop.CycleHours) * gameconf.HourMs(gameconf.TimeProfileDemo)
	waterSpan := seasonMS * 35 / 100
	if err := debugAdvance(baseURL, waterSpan); err != nil {
		return fmt.Errorf("advance to first water: %w", err)
	}
	if _, err := mustAction(conn, &seq, gateway.CommandWater, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 0,
	}); err != nil {
		return fmt.Errorf("water#1: %w", err)
	}
	if err := debugAdvance(baseURL, waterSpan); err != nil {
		return fmt.Errorf("advance to second water: %w", err)
	}
	if _, err := mustAction(conn, &seq, gateway.CommandWater, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 0,
	}); err != nil {
		return fmt.Errorf("water#2: %w", err)
	}
	remain := seasonMS - 2*waterSpan
	if remain < 0 {
		remain = 0
	}
	if err := debugAdvance(baseURL, remain+1); err != nil {
		return fmt.Errorf("advance to mature: %w", err)
	}

	harvest, err := mustAction(conn, &seq, gateway.CommandHarvest, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 0,
	})
	if err != nil {
		return fmt.Errorf("harvest: %w", err)
	}
	fruitKey := string(farm.FruitItem(1))
	yield := harvest.Patch.Warehouse[fruitKey]
	if yield == 0 {
		return fmt.Errorf("harvest yielded 0 fruit")
	}
	if yield != uint32(crop.Yield) {
		return fmt.Errorf("harvest yield = %d, want %d", yield, crop.Yield)
	}

	sell, err := mustAction(conn, &seq, gateway.CommandSell, map[string]any{
		"item_id":  1,
		"quantity": yield,
	})
	if err != nil {
		return fmt.Errorf("sell: %w", err)
	}
	wantCoin := gameconf.InitialCoin - int64(crop.SeedPrice) + int64(crop.FruitPrice)*int64(yield)
	if sell.Patch.Coin != wantCoin {
		return fmt.Errorf("coin after sell = %d, want %d", sell.Patch.Coin, wantCoin)
	}
	if sell.Patch.Warehouse[fruitKey] != 0 {
		return fmt.Errorf("warehouse still has fruit after sell")
	}
	return nil
}

func mustAction(conn *websocket.Conn, seq *uint32, cmd uint32, payload map[string]any) (actionPayload, error) {
	env, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       cmd,
		ClientSeq: *seq,
		Payload:   mustJSON(payload),
	})
	*seq++
	if err != nil {
		return actionPayload{}, err
	}
	var out actionPayload
	if err := json.Unmarshal(env.Payload, &out); err != nil {
		return actionPayload{}, fmt.Errorf("decode action payload: %w", err)
	}
	return out, nil
}

func debugAdvance(baseURL string, ms int64) error {
	body, err := json.Marshal(map[string]int64{"ms": ms})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(baseURL+"/api/debug/advance", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s (is FARM_ALLOW_DEBUG_TIME=1 set?)", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func authenticate(endpoint, username, password string) (authResponse, error) {
	body, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		return authResponse{}, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return authResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		if readErr != nil {
			return authResponse{}, fmt.Errorf("status %d (read response: %w)", response.StatusCode, readErr)
		}
		return authResponse{}, fmt.Errorf("status %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result authResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return authResponse{}, err
	}
	return result, nil
}

func exchange(conn *websocket.Conn, request gateway.Envelope) error {
	_, err := exchangeResponse(conn, request)
	return err
}

func exchangeResponse(conn *websocket.Conn, request gateway.Envelope) (gateway.Envelope, error) {
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, fmt.Errorf("write request: %w", err)
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
	}
	if messageType != websocket.TextMessage {
		return gateway.Envelope{}, fmt.Errorf("response message type = %d, want text", messageType)
	}
	response, err := gateway.DecodeEnvelope(data)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Cmd != request.Cmd || response.ClientSeq != request.ClientSeq {
		return gateway.Envelope{}, fmt.Errorf("response headers = cmd %d, seq %d; want cmd %d, seq %d", response.Cmd, response.ClientSeq, request.Cmd, request.ClientSeq)
	}
	if response.Err != pkgerr.OK {
		return gateway.Envelope{}, fmt.Errorf("err = %d, want %d", response.Err, pkgerr.OK)
	}
	return response, nil
}

func smokeUsername() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate username: %w", err)
	}
	return "smoke-" + hex.EncodeToString(suffix[:]), nil
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// runFriends 覆盖期 3 加好友主路径：A GenShareLink → B AcceptInvite → A FriendList 含 B
// → B 再 AddFriendByUID(A) 应得 AlreadyFriend(1402)。
func runFriends(baseURL string) error {
	userA, err := smokeUsername()
	if err != nil {
		return fmt.Errorf("generate username A: %w", err)
	}
	userB, err := smokeUsername()
	if err != nil {
		return fmt.Errorf("generate username B: %w", err)
	}

	if _, err := authenticate(baseURL+"/api/register", userA, smokePassword); err != nil {
		return fmt.Errorf("register A: %w", err)
	}
	if _, err := authenticate(baseURL+"/api/register", userB, smokePassword); err != nil {
		return fmt.Errorf("register B: %w", err)
	}
	loginA, err := authenticate(baseURL+"/api/login", userA, smokePassword)
	if err != nil {
		return fmt.Errorf("login A: %w", err)
	}
	loginB, err := authenticate(baseURL+"/api/login", userB, smokePassword)
	if err != nil {
		return fmt.Errorf("login B: %w", err)
	}
	if loginA.UID == 0 || loginB.UID == 0 || loginA.UID == loginB.UID {
		return fmt.Errorf("login uids invalid: A=%d B=%d", loginA.UID, loginB.UID)
	}

	connA, err := dialAndHandshake(loginA)
	if err != nil {
		return fmt.Errorf("connect A: %w", err)
	}
	defer connA.Close()
	connB, err := dialAndHandshake(loginB)
	if err != nil {
		return fmt.Errorf("connect B: %w", err)
	}
	defer connB.Close()

	seqA := uint32(2) // handshake 消耗 seq 1
	seqB := uint32(2)

	shareEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandGenShareLink,
		ClientSeq: seqA,
		Payload:   emptyJSONObject,
	})
	if err != nil {
		return fmt.Errorf("GenShareLink: %w", err)
	}
	seqA++
	var share genShareLinkResponse
	if err := json.Unmarshal(shareEnv.Payload, &share); err != nil {
		return fmt.Errorf("decode GenShareLink payload: %w", err)
	}
	if !strings.HasPrefix(share.Path, "/i/") {
		return fmt.Errorf("share path = %q, want /i/ prefix", share.Path)
	}
	token := strings.TrimPrefix(share.Path, "/i/")
	if token == "" {
		return fmt.Errorf("share token is empty")
	}

	if _, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandAcceptInvite,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"token": token}),
	}); err != nil {
		return fmt.Errorf("AcceptInvite: %w", err)
	}
	seqB++

	listEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandFriendList,
		ClientSeq: seqA,
		Payload:   emptyJSONObject,
	})
	if err != nil {
		return fmt.Errorf("FriendList: %w", err)
	}
	seqA++
	var list friendListResponse
	if err := json.Unmarshal(listEnv.Payload, &list); err != nil {
		return fmt.Errorf("decode FriendList payload: %w", err)
	}
	if !friendListContains(list.Friends, loginB.UID) {
		return fmt.Errorf("FriendList does not contain B (uid=%d): %#v", loginB.UID, list)
	}

	dupEnv, err := exchangeEnvelope(connB, gateway.Envelope{
		Cmd:       gateway.CommandAddFriendByUID,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"peer_uid": loginA.UID}),
	})
	if err != nil {
		return fmt.Errorf("AddFriendByUID: %w", err)
	}
	seqB++
	if dupEnv.Err != pkgerr.AlreadyFriend {
		return fmt.Errorf("duplicate Add err = %d, want %d", dupEnv.Err, pkgerr.AlreadyFriend)
	}
	return nil
}

// runRoom 覆盖期 3 房间同步主路径：A、B 互为好友 → B Enter A → A Till →
// B 收到 FarmDelta(9000) 且 seq+1；再以 SyncFarm 验证 delta 与 snapshot 路径状态一致。
func runRoom(baseURL string) error {
	userA, err := smokeUsername()
	if err != nil {
		return fmt.Errorf("generate username A: %w", err)
	}
	userB, err := smokeUsername()
	if err != nil {
		return fmt.Errorf("generate username B: %w", err)
	}

	if _, err := authenticate(baseURL+"/api/register", userA, smokePassword); err != nil {
		return fmt.Errorf("register A: %w", err)
	}
	if _, err := authenticate(baseURL+"/api/register", userB, smokePassword); err != nil {
		return fmt.Errorf("register B: %w", err)
	}
	loginA, err := authenticate(baseURL+"/api/login", userA, smokePassword)
	if err != nil {
		return fmt.Errorf("login A: %w", err)
	}
	loginB, err := authenticate(baseURL+"/api/login", userB, smokePassword)
	if err != nil {
		return fmt.Errorf("login B: %w", err)
	}
	if loginA.UID == 0 || loginB.UID == 0 || loginA.UID == loginB.UID {
		return fmt.Errorf("login uids invalid: A=%d B=%d", loginA.UID, loginB.UID)
	}

	connA, err := dialAndHandshake(loginA)
	if err != nil {
		return fmt.Errorf("connect A: %w", err)
	}
	defer connA.Close()
	connB, err := dialAndHandshake(loginB)
	if err != nil {
		return fmt.Errorf("connect B: %w", err)
	}
	defer connB.Close()

	seqA := uint32(2) // handshake 消耗 seq 1
	seqB := uint32(2)

	// A GenShareLink → B AcceptInvite，建立好友关系。
	shareEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandGenShareLink,
		ClientSeq: seqA,
		Payload:   emptyJSONObject,
	})
	if err != nil {
		return fmt.Errorf("GenShareLink: %w", err)
	}
	seqA++
	var share genShareLinkResponse
	if err := json.Unmarshal(shareEnv.Payload, &share); err != nil {
		return fmt.Errorf("decode GenShareLink payload: %w", err)
	}
	token := strings.TrimPrefix(share.Path, "/i/")
	if token == "" {
		return fmt.Errorf("share token is empty")
	}
	if _, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandAcceptInvite,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"token": token}),
	}); err != nil {
		return fmt.Errorf("AcceptInvite: %w", err)
	}
	seqB++

	// B Enter A：relation=FRIEND，并订阅 A 的房间。
	enterBEnv, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"owner_uid": loginA.UID}),
	})
	if err != nil {
		return fmt.Errorf("B EnterFarm(A): %w", err)
	}
	seqB++
	var enterB enterFarmResponse
	if err := json.Unmarshal(enterBEnv.Payload, &enterB); err != nil {
		return fmt.Errorf("decode B EnterFarm payload: %w", err)
	}
	if enterB.Relation != "FRIEND" {
		return fmt.Errorf("B relation = %q, want FRIEND", enterB.Relation)
	}
	if enterB.FarmSeq != 0 {
		return fmt.Errorf("B initial farm_seq = %d, want 0", enterB.FarmSeq)
	}

	// A Enter 自己农场，拿到权威快照基线。
	enterAEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	})
	if err != nil {
		return fmt.Errorf("A EnterFarm(self): %w", err)
	}
	seqA++
	var enterA enterFarmResponse
	if err := json.Unmarshal(enterAEnv.Payload, &enterA); err != nil {
		return fmt.Errorf("decode A EnterFarm payload: %w", err)
	}
	if enterA.Relation != "SELF" {
		return fmt.Errorf("A relation = %q, want SELF", enterA.Relation)
	}

	// A Till plot 0：成功后 farm_seq=1，并广播 FarmDelta(9000) 给 B。
	tillEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandTill,
		ClientSeq: seqA,
		Payload: mustJSON(map[string]any{
			"owner_uid":  0,
			"plot_index": 0,
			"arg":        0,
		}),
	})
	if err != nil {
		return fmt.Errorf("A Till: %w", err)
	}
	seqA++
	var till actionPayload
	if err := json.Unmarshal(tillEnv.Payload, &till); err != nil {
		return fmt.Errorf("decode A Till payload: %w", err)
	}
	if till.FarmSeq != 1 {
		return fmt.Errorf("A Till farm_seq = %d, want 1", till.FarmSeq)
	}
	if till.Patch.Plot == nil {
		return fmt.Errorf("A Till patch missing plot")
	}
	authoritativeState := till.Patch.Plot.State
	if authoritativeState != farm.StateTilled {
		return fmt.Errorf("A Till plot state = %d, want %d", authoritativeState, farm.StateTilled)
	}

	// B 应收到服务端主动推送的 FarmDelta(9000)，ClientSeq=0。
	if err := connB.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set B read deadline: %w", err)
	}
	deltaEnv, err := readServerPush(connB)
	if err != nil {
		return fmt.Errorf("B read FarmDelta: %w", err)
	}
	if deltaEnv.Cmd != gateway.CommandFarmDelta {
		return fmt.Errorf("B push cmd = %d, want %d", deltaEnv.Cmd, gateway.CommandFarmDelta)
	}
	if deltaEnv.ClientSeq != 0 {
		return fmt.Errorf("B push client_seq = %d, want 0", deltaEnv.ClientSeq)
	}
	if deltaEnv.Err != pkgerr.OK {
		return fmt.Errorf("B push err = %d, want 0", deltaEnv.Err)
	}
	var delta farm.FarmDelta
	if err := json.Unmarshal(deltaEnv.Payload, &delta); err != nil {
		return fmt.Errorf("decode FarmDelta: %w", err)
	}
	if delta.OwnerUID != loginA.UID {
		return fmt.Errorf("delta owner_uid = %d, want %d", delta.OwnerUID, loginA.UID)
	}
	if delta.FarmSeq != 1 {
		return fmt.Errorf("delta farm_seq = %d, want 1", delta.FarmSeq)
	}
	if delta.ActorUID != loginA.UID {
		return fmt.Errorf("delta actor_uid = %d, want %d", delta.ActorUID, loginA.UID)
	}
	if delta.Action != gateway.CommandTill {
		return fmt.Errorf("delta action = %d, want %d", delta.Action, gateway.CommandTill)
	}
	if len(delta.Plots) != 1 || delta.Plots[0].Index != 0 {
		return fmt.Errorf("delta plots = %#v, want single plot 0", delta.Plots)
	}
	if delta.Plots[0].State != authoritativeState {
		return fmt.Errorf("delta plot state = %d, want authoritative %d", delta.Plots[0].State, authoritativeState)
	}

	// SyncFarm from_seq=0：delta 路径，应回一条 delta 且无 snapshot。
	syncFromZero, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandSyncFarm,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"owner_uid": loginA.UID, "from_seq": 0}),
	})
	if err != nil {
		return fmt.Errorf("B SyncFarm(from=0): %w", err)
	}
	seqB++
	var syncZero syncFarmResponse
	if err := json.Unmarshal(syncFromZero.Payload, &syncZero); err != nil {
		return fmt.Errorf("decode SyncFarm(from=0) payload: %w", err)
	}
	if syncZero.FarmSeq != 1 || len(syncZero.Deltas) != 1 || syncZero.Snapshot != nil {
		return fmt.Errorf("SyncFarm(from=0) = %#v, want farm_seq=1, 1 delta, no snapshot", syncZero)
	}
	if syncZero.Deltas[0].FarmSeq != 1 || len(syncZero.Deltas[0].Plots) != 1 ||
		syncZero.Deltas[0].Plots[0].State != authoritativeState {
		return fmt.Errorf("SyncFarm(from=0) delta = %#v, want state=%d", syncZero.Deltas[0], authoritativeState)
	}

	// SyncFarm from_seq=1：已追平，应无 delta 且无 snapshot。
	syncCaught, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandSyncFarm,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"owner_uid": loginA.UID, "from_seq": 1}),
	})
	if err != nil {
		return fmt.Errorf("B SyncFarm(from=1): %w", err)
	}
	seqB++
	var syncOne syncFarmResponse
	if err := json.Unmarshal(syncCaught.Payload, &syncOne); err != nil {
		return fmt.Errorf("decode SyncFarm(from=1) payload: %w", err)
	}
	if syncOne.FarmSeq != 1 || len(syncOne.Deltas) != 0 || syncOne.Snapshot != nil {
		return fmt.Errorf("SyncFarm(from=1) = %#v, want farm_seq=1, no deltas, no snapshot", syncOne)
	}

	// B LeaveFarm：之后 A 再写不应再被推送到 B。
	if _, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandLeaveFarm,
		ClientSeq: seqB,
		Payload:   emptyJSONObject,
	}); err != nil {
		return fmt.Errorf("B LeaveFarm: %w", err)
	}
	seqB++

	if _, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandTill,
		ClientSeq: seqA,
		Payload: mustJSON(map[string]any{
			"owner_uid":  0,
			"plot_index": 1,
			"arg":        0,
		}),
	}); err != nil {
		return fmt.Errorf("A Till plot 1: %w", err)
	}
	seqA++

	if err := connB.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		return fmt.Errorf("set B post-leave deadline: %w", err)
	}
	if _, _, err := connB.ReadMessage(); err == nil {
		return fmt.Errorf("B received FarmDelta after LeaveFarm")
	}
	return nil
}

// readServerPush 读取一帧服务端主动推送的 Envelope，不校验 ClientSeq。
func readServerPush(conn *websocket.Conn) (gateway.Envelope, error) {
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return gateway.Envelope{}, err
	}
	if messageType != websocket.TextMessage {
		return gateway.Envelope{}, fmt.Errorf("push message type = %d, want text", messageType)
	}
	return gateway.DecodeEnvelope(data)
}

func dialAndHandshake(login authResponse) (*websocket.Conn, error) {
	conn, response, err := websocket.DefaultDialer.Dial(login.WSURL, http.Header{
		"Sec-WebSocket-Protocol": []string{gateway.JSONSubprotocol},
	})
	if err != nil {
		return nil, fmt.Errorf("dial websocket: %w", err)
	}
	if response.Header.Get("Sec-WebSocket-Protocol") != gateway.JSONSubprotocol {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket subprotocol was not negotiated")
	}
	if err := exchange(conn, gateway.Envelope{
		Cmd:       gateway.CommandHandshake,
		ClientSeq: 1,
		Payload: mustJSON(map[string]any{
			"token":             login.Token,
			"client_config_ver": gameconf.ConfigVer,
		}),
	}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return conn, nil
}

// exchangeEnvelope 与 exchangeResponse 类似，但不校验 Err 字段，便于断言错误码。
func exchangeEnvelope(conn *websocket.Conn, request gateway.Envelope) (gateway.Envelope, error) {
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, fmt.Errorf("write request: %w", err)
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
	}
	if messageType != websocket.TextMessage {
		return gateway.Envelope{}, fmt.Errorf("response message type = %d, want text", messageType)
	}
	response, err := gateway.DecodeEnvelope(data)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
	}
	if response.Cmd != request.Cmd || response.ClientSeq != request.ClientSeq {
		return gateway.Envelope{}, fmt.Errorf("response headers = cmd %d, seq %d; want cmd %d, seq %d", response.Cmd, response.ClientSeq, request.Cmd, request.ClientSeq)
	}
	return response, nil
}

type genShareLinkResponse struct {
	Path string `json:"path"`
}

type friendListResponse struct {
	Friends []friendEntry `json:"friends"`
}

type friendEntry struct {
	UID      uint64 `json:"uid"`
	Nickname string `json:"nickname"`
}

func friendListContains(friends []friendEntry, uid uint64) bool {
	for _, friend := range friends {
		if friend.UID == uid {
			return true
		}
	}
	return false
}

var emptyJSONObject = json.RawMessage(`{}`)
