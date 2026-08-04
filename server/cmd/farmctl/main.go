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

	"farm/server/domain/farm"
	"farm/server/gateway"
	"farm/server/shared/clientjson"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/sharding"
)

const (
	defaultBaseURL = "http://127.0.0.1:9002"
	defaultGW0URL  = "http://127.0.0.1:9200"
	defaultGW1URL  = "http://127.0.0.1:9201"
	smokePassword  = "smoke-password"
)

type authResponse struct {
	UID   uint64 `json:"uid"`
	Token string `json:"token"`
	WSURL string `json:"ws_url"`
}

func (r *authResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		UID   clientjson.UID `json:"uid"`
		Token string         `json:"token"`
		WSURL string         `json:"ws_url"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	r.UID = uint64(wire.UID)
	r.Token = wire.Token
	r.WSURL = wire.WSURL
	return nil
}

type enterFarmPayload struct {
	Snapshot farm.FarmSnapshotJSON `json:"snapshot"`
}

type actionPayload struct {
	FarmSeq clientjson.Uint64 `json:"farm_seq"`
	Patch   farm.PatchJSON    `json:"patch"`
}

type enterFarmResponse struct {
	Snapshot   farm.FarmSnapshotJSON `json:"snapshot"`
	FarmSeq    clientjson.Uint64     `json:"farm_seq"`
	ServerTime int64                 `json:"server_time"`
	Relation   string                `json:"relation"`
}

type syncFarmResponse struct {
	Deltas   []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq  clientjson.Uint64      `json:"farm_seq"`
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
	case "shards":
		fmt.Println("smoke: sharded RPC plant/pet/task/mail + cross-farm help/steal passed")
	case "help":
		fmt.Println("smoke: cross-farm mutual aid water passed")
	case "steal":
		fmt.Println("smoke: steal quota/harvest-race/no-afford/dog-intercept passed")
	case "bench":
		// bench 自己打印逐轮明细与汇总，这里不再追加一行通过提示。
	case "hotspot":
		// hotspot 自己打印万人级单农场突发结果。
	case "all":
		fmt.Println("smoke: planting + friends + room + help + steal passed")
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
	case "shards":
		gw0 := strings.TrimRight(getenv("FARM_SMOKE_GATEWAY0", defaultGW0URL), "/")
		gw1 := strings.TrimRight(getenv("FARM_SMOKE_GATEWAY1", defaultGW1URL), "/")
		return runShards(gw0, gw1)
	case "help":
		return runHelp(baseURL)
	case "steal":
		return runSteal(baseURL)
	case "bench":
		// bench 有自己的命令行参数，透传子命令之后的全部实参。
		var args []string
		if len(os.Args) > 2 {
			args = os.Args[2:]
		}
		return runBench(args, baseURL)
	case "hotspot":
		var args []string
		if len(os.Args) > 2 {
			args = os.Args[2:]
		}
		return runHotspot(args, baseURL)
	case "all":
		if err := runPlanting(baseURL); err != nil {
			return err
		}
		if err := runFriends(baseURL); err != nil {
			return err
		}
		if err := runRoom(baseURL); err != nil {
			return err
		}
		if err := runHelp(baseURL); err != nil {
			return err
		}
		return runSteal(baseURL)
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
			"client_config_ver": gameconfig.ConfigVer,
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
	if enter.Snapshot.Coin != gameconfig.InitialCoin {
		return fmt.Errorf("coin = %d, want %d", enter.Snapshot.Coin, gameconfig.InitialCoin)
	}

	crop, ok := gameconfig.CropByID(1)
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
	seasonMS := gameconfig.SeasonDurationMs(crop, 0, gameconfig.TimeProfileDemo)
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
	// 调时跨过生长窗口时可能确定性触发缺水、杂草或害虫，最终产量允许低于
	// 作物基础产量；冒烟只验证产量非零且绝不超过配置上限。
	if yield > uint32(crop.Yield) {
		return fmt.Errorf("harvest yield = %d, exceeds configured maximum %d", yield, crop.Yield)
	}

	sell, err := mustAction(conn, &seq, gateway.CommandSell, map[string]any{
		"item_id":  1,
		"quantity": yield,
	})
	if err != nil {
		return fmt.Errorf("sell: %w", err)
	}
	wantCoin := gameconfig.InitialCoin - int64(crop.SeedPrice) + int64(crop.FruitPrice)*int64(yield)
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
	for attempt := 0; attempt < 32; attempt++ {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		response, err := gateway.DecodeEnvelope(data)
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
		}
		if response.ClientSeq == 0 {
			// FarmDelta/PlayerDelta can legitimately race an ordinary Rsp.
			continue
		}
		if response.Cmd != request.Cmd || response.ClientSeq != request.ClientSeq {
			return gateway.Envelope{}, fmt.Errorf("response headers = cmd %d, seq %d; want cmd %d, seq %d", response.Cmd, response.ClientSeq, request.Cmd, request.ClientSeq)
		}
		if response.Err != errcode.OK {
			return gateway.Envelope{}, fmt.Errorf("err = %d, want %d", response.Err, errcode.OK)
		}
		return response, nil
	}
	return gateway.Envelope{}, fmt.Errorf("no matching response for cmd=%d seq=%d", request.Cmd, request.ClientSeq)
}

// exchangeResponseWithPush verifies that a command crossing Farm/Gateway
// boundaries also reaches this client as a server push before its response.
func exchangeResponseWithPush(conn *websocket.Conn, request gateway.Envelope, pushCmd uint32) (gateway.Envelope, gateway.Envelope, error) {
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("write request: %w", err)
	}
	var push gateway.Envelope
	for attempt := 0; attempt < 32; attempt++ {
		messageType, frame, err := conn.ReadMessage()
		if err != nil {
			return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("read response: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		env, err := gateway.DecodeEnvelope(frame)
		if err != nil {
			return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
		}
		if env.Cmd == pushCmd && env.ClientSeq == 0 {
			push = env
			continue
		}
		if env.Cmd != request.Cmd || env.ClientSeq != request.ClientSeq {
			continue
		}
		if env.Err != errcode.OK {
			return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("err = %d, want %d", env.Err, errcode.OK)
		}
		if push.Cmd != pushCmd {
			return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("missing server push cmd=%d", pushCmd)
		}
		return env, push, nil
	}
	return gateway.Envelope{}, gateway.Envelope{}, fmt.Errorf("no matching response for cmd=%d seq=%d", request.Cmd, request.ClientSeq)
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
	if dupEnv.Err != errcode.AlreadyFriend {
		return fmt.Errorf("duplicate Add err = %d, want %d", dupEnv.Err, errcode.AlreadyFriend)
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
		return fmt.Errorf("player B EnterFarm(A): %w", err)
	}
	seqB++
	var enterB enterFarmResponse
	if err := json.Unmarshal(enterBEnv.Payload, &enterB); err != nil {
		return fmt.Errorf("decode B EnterFarm payload: %w", err)
	}
	if enterB.Relation != "FRIEND" {
		return fmt.Errorf("player B relation = %q, want FRIEND", enterB.Relation)
	}
	if enterB.FarmSeq != 0 {
		return fmt.Errorf("player B initial farm_seq = %d, want 0", enterB.FarmSeq)
	}

	// A Enter 自己农场，拿到权威快照基线。
	enterAEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	})
	if err != nil {
		return fmt.Errorf("player A EnterFarm(self): %w", err)
	}
	seqA++
	var enterA enterFarmResponse
	if err := json.Unmarshal(enterAEnv.Payload, &enterA); err != nil {
		return fmt.Errorf("decode A EnterFarm payload: %w", err)
	}
	if enterA.Relation != "SELF" {
		return fmt.Errorf("player A relation = %q, want SELF", enterA.Relation)
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
		return fmt.Errorf("player A Till: %w", err)
	}
	seqA++
	var till actionPayload
	if err := json.Unmarshal(tillEnv.Payload, &till); err != nil {
		return fmt.Errorf("decode A Till payload: %w", err)
	}
	if till.FarmSeq != 1 {
		return fmt.Errorf("player A Till farm_seq = %d, want 1", till.FarmSeq)
	}
	if till.Patch.Plot == nil {
		return fmt.Errorf("player A Till patch missing plot")
	}
	authoritativeState := till.Patch.Plot.State
	if authoritativeState != farm.StateTilled {
		return fmt.Errorf("player A Till plot state = %d, want %d", authoritativeState, farm.StateTilled)
	}

	// B 应收到服务端主动推送的 FarmDelta(9000)，ClientSeq=0。
	if err := connB.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set B read deadline: %w", err)
	}
	deltaEnv, err := readServerPush(connB)
	if err != nil {
		return fmt.Errorf("player B read FarmDelta: %w", err)
	}
	if deltaEnv.Cmd != gateway.CommandFarmDelta {
		return fmt.Errorf("player B push cmd = %d, want %d", deltaEnv.Cmd, gateway.CommandFarmDelta)
	}
	if deltaEnv.ClientSeq != 0 {
		return fmt.Errorf("player B push client_seq = %d, want 0", deltaEnv.ClientSeq)
	}
	if deltaEnv.Err != errcode.OK {
		return fmt.Errorf("player B push err = %d, want 0", deltaEnv.Err)
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
		return fmt.Errorf("player B SyncFarm(from=0): %w", err)
	}
	seqB++
	var syncZero syncFarmResponse
	if err := json.Unmarshal(syncFromZero.Payload, &syncZero); err != nil {
		return fmt.Errorf("decode SyncFarm(from=0) payload: %w", err)
	}
	if syncZero.FarmSeq != 1 || len(syncZero.Deltas) != 1 || syncZero.Snapshot != nil {
		return fmt.Errorf("sync farm(from=0) = %#v, want farm_seq=1, 1 delta, no snapshot", syncZero)
	}
	if syncZero.Deltas[0].FarmSeq != 1 || len(syncZero.Deltas[0].Plots) != 1 ||
		syncZero.Deltas[0].Plots[0].State != authoritativeState {
		return fmt.Errorf("sync farm(from=0) delta = %#v, want state=%d", syncZero.Deltas[0], authoritativeState)
	}

	// SyncFarm from_seq=1：已追平，应无 delta 且无 snapshot。
	syncCaught, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandSyncFarm,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"owner_uid": loginA.UID, "from_seq": 1}),
	})
	if err != nil {
		return fmt.Errorf("player B SyncFarm(from=1): %w", err)
	}
	seqB++
	var syncOne syncFarmResponse
	if err := json.Unmarshal(syncCaught.Payload, &syncOne); err != nil {
		return fmt.Errorf("decode SyncFarm(from=1) payload: %w", err)
	}
	if syncOne.FarmSeq != 1 || len(syncOne.Deltas) != 0 || syncOne.Snapshot != nil {
		return fmt.Errorf("sync farm(from=1) = %#v, want farm_seq=1, no deltas, no snapshot", syncOne)
	}

	// B LeaveFarm：之后 A 再写不应再被推送到 B。
	if _, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandLeaveFarm,
		ClientSeq: seqB,
		Payload:   emptyJSONObject,
	}); err != nil {
		return fmt.Errorf("player B LeaveFarm: %w", err)
	}
	if _, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandTill,
		ClientSeq: seqA,
		Payload: mustJSON(map[string]any{
			"owner_uid":  0,
			"plot_index": 1,
			"arg":        0,
		}),
	}); err != nil {
		return fmt.Errorf("player A Till plot 1: %w", err)
	}
	if err := connB.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		return fmt.Errorf("set B post-leave deadline: %w", err)
	}
	for {
		_, data, readErr := connB.ReadMessage()
		if readErr != nil {
			break
		}
		// TaskNotify、MailNotify 与房间 Delta 是独立异步通道，LeaveFarm 之后仍
		// 可能读到此前已经入队的个人通知。这里只拒绝离开后产生的 plot 1 Delta。
		envelope, decodeErr := gateway.DecodeEnvelope(data)
		if decodeErr != nil || envelope.Cmd != gateway.CommandFarmDelta {
			continue
		}
		var pushed farm.FarmDelta
		if json.Unmarshal(envelope.Payload, &pushed) == nil && pushed.OwnerUID == loginA.UID && pushed.FarmSeq >= 2 {
			return fmt.Errorf("player B received post-leave FarmDelta: seq=%d", pushed.FarmSeq)
		}
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
			"client_config_ver": gameconfig.ConfigVer,
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
	UID      clientjson.UID `json:"uid"`
	Nickname string         `json:"nickname"`
}

func friendListContains(friends []friendEntry, uid uint64) bool {
	for _, friend := range friends {
		if uint64(friend.UID) == uid {
			return true
		}
	}
	return false
}

// runHelp 覆盖三服务环境中的互助主路径：A/B 建立好友关系后进入对方农场 →
// 好友 → A 翻地种植 → B 拜访 A → B 浇水成功得经验 → B 立即再浇一次得
// AlreadyWatered 且无奖励（失败回滚计数）→ A SyncFarm 确认浇水已提交。
func runHelp(baseURL string) error {
	routes, err := loadSmokeRouteTable()
	if err != nil {
		return err
	}

	loginA, farmA, err := registerOnFarm(baseURL, routes, "farm-0")
	if err != nil {
		return fmt.Errorf("register A on farm-0: %w", err)
	}
	loginB, farmB, err := registerOnFarm(baseURL, routes, farmA)
	if err != nil {
		return fmt.Errorf("register B on %s: %w", farmA, err)
	}
	fmt.Printf("smoke help: A uid=%d farm=%s; B uid=%d farm=%s\n",
		loginA.UID, farmA, loginB.UID, farmB)

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

	seqA := uint32(2)
	seqB := uint32(2)

	// 好友关系：A GenShareLink → B AcceptInvite。
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
		return fmt.Errorf("decode GenShareLink: %w", err)
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

	// A 进入自己农场，买种、翻地、种植 plot 0。
	if _, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	}); err != nil {
		return fmt.Errorf("player A EnterFarm(self): %w", err)
	}
	seqA++
	if _, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandBuy,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"item_id": 1, "quantity": 1}),
	}); err != nil {
		return fmt.Errorf("player A Buy seed: %w", err)
	}
	seqA++
	if _, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandTill,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0, "plot_index": 0, "arg": 0}),
	}); err != nil {
		return fmt.Errorf("player A Till: %w", err)
	}
	seqA++
	if _, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandPlant,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0, "plot_index": 0, "arg": 1}),
	}); err != nil {
		return fmt.Errorf("player A Plant: %w", err)
	}
	seqA++

	// 推进到首次可浇水窗口，避免种植后立即浇水被判 AlreadyWatered。
	crop, ok := gameconfig.CropByID(1)
	if !ok {
		return fmt.Errorf("missing white radish crop config")
	}
	seasonMS := gameconfig.SeasonDurationMs(crop, 0, gameconfig.TimeProfileDemo)
	waterSpan := seasonMS * 35 / 100
	if err := debugAdvance(baseURL, waterSpan); err != nil {
		return fmt.Errorf("advance to water window: %w", err)
	}

	// B 拜访 A：relation=FRIEND。
	enterBEnv, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"owner_uid": loginA.UID}),
	})
	if err != nil {
		return fmt.Errorf("player B EnterFarm(A): %w", err)
	}
	seqB++
	var enterB enterFarmResponse
	if err := json.Unmarshal(enterBEnv.Payload, &enterB); err != nil {
		return fmt.Errorf("decode B EnterFarm: %w", err)
	}
	if enterB.Relation != "FRIEND" {
		return fmt.Errorf("player B relation = %q, want FRIEND", enterB.Relation)
	}

	// B 浇水成功：得经验 2，无金币。可能伴随 FarmDelta 推送，需多帧匹配。
	water1, err := exchangeVisitorEnvelope(connB, &seqB, gateway.CommandWater, loginA.UID, 0)
	if err != nil {
		return fmt.Errorf("player B Water#1: %w", err)
	}
	if water1.Err != errcode.OK {
		return fmt.Errorf("player B Water#1 err = %d, want 0", water1.Err)
	}
	var reward crossRewardResponse
	if err := json.Unmarshal(water1.Payload, &reward); err != nil {
		return fmt.Errorf("decode Water#1 reward: %w", err)
	}
	if reward.ExpGained != 2 {
		return fmt.Errorf("water#1 exp_gained = %d, want 2", reward.ExpGained)
	}
	if reward.CoinGained != 0 {
		return fmt.Errorf("water#1 coin_gained = %d, want 0", reward.CoinGained)
	}

	// B 立即再浇一次：水分已满，应得 AlreadyWatered 且无奖励（失败回滚计数）。
	water2, err := exchangeVisitorEnvelope(connB, &seqB, gateway.CommandWater, loginA.UID, 0)
	if err != nil {
		return fmt.Errorf("player B Water#2: %w", err)
	}
	if water2.Err != errcode.AlreadyWatered {
		return fmt.Errorf("water#2 err = %d, want %d (AlreadyWatered)", water2.Err, errcode.AlreadyWatered)
	}
	if len(water2.Payload) != 0 && string(water2.Payload) != "{}" {
		return fmt.Errorf("water#2 payload = %s, want empty (no reward on failure)", string(water2.Payload))
	}

	// A SyncFarm(from=0)：浇水已提交，farm_seq 应 >=1。
	syncEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandSyncFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0, "from_seq": 0}),
	})
	if err != nil {
		return fmt.Errorf("player A SyncFarm: %w", err)
	}
	var sync syncFarmResponse
	if err := json.Unmarshal(syncEnv.Payload, &sync); err != nil {
		return fmt.Errorf("decode A SyncFarm: %w", err)
	}
	if sync.FarmSeq == 0 {
		return fmt.Errorf("player A SyncFarm farm_seq = 0, want >=1 (water committed)")
	}
	return nil
}

// crossRewardResponse mirrors gateway.crossActionResponse; duplicated here
// because the gateway type is unexported.
type crossRewardResponse struct {
	ReqID      clientjson.Uint64 `json:"req_id"`
	ExpGained  uint32            `json:"exp_gained"`
	CoinGained clientjson.Int64  `json:"coin_gained"`
}

// exchangeVisitorEnvelope sends a visiting plot action and reads frames until
// it finds the matching response, draining any FarmDelta(9000) pushes the
// server emits to the visiting connection. It does NOT assert Err so callers
// can branch on success/failure codes.
func exchangeVisitorEnvelope(conn *websocket.Conn, seq *uint32, cmd uint32, ownerUID uint64, plotIndex uint32) (gateway.Envelope, error) {
	request := gateway.Envelope{
		Cmd:       cmd,
		ClientSeq: *seq,
		Payload:   mustJSON(map[string]any{"owner_uid": ownerUID, "plot_index": plotIndex, "arg": 0}),
	}
	*seq++
	data, err := gateway.EncodeEnvelope(request)
	if err != nil {
		return gateway.Envelope{}, fmt.Errorf("encode request: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return gateway.Envelope{}, fmt.Errorf("write request: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return gateway.Envelope{}, fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for attempt := 0; attempt < 4; attempt++ {
		messageType, frame, err := conn.ReadMessage()
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("read response: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		env, err := gateway.DecodeEnvelope(frame)
		if err != nil {
			return gateway.Envelope{}, fmt.Errorf("decode response: %w", err)
		}
		if env.Cmd == request.Cmd && env.ClientSeq == request.ClientSeq {
			return env, nil
		}
		// 服务端主动推送（如 FarmDelta）忽略，继续等待匹配响应。
	}
	return gateway.Envelope{}, fmt.Errorf("no matching response for cmd=%d seq=%d", request.Cmd, request.ClientSeq)
}

// runShards 保留双实例兼容场景：gRPC 种植/宠物/任务邮件及跨 Farm 互助与偷菜；
// A/B 的客户端分别连接 gateway-0/1。
func runShards(gw0, gw1 string) error {
	routes, err := loadSmokeRouteTable()
	if err != nil {
		return err
	}

	loginA, farmA, err := registerOnFarm(gw0, routes, "farm-0")
	if err != nil {
		return fmt.Errorf("register A on farm-0 via gateway-0: %w", err)
	}
	loginB, farmB, err := registerOnFarm(gw1, routes, "farm-1")
	if err != nil {
		return fmt.Errorf("register B on farm-1 via gateway-1: %w", err)
	}
	if farmA == farmB {
		return fmt.Errorf("player A and B landed on same farm %q", farmA)
	}
	fmt.Printf("smoke shards: A uid=%d farm=%s gw=%s; B uid=%d farm=%s gw=%s\n",
		loginA.UID, farmA, gw0, loginB.UID, farmB, gw1)

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

	seqA := uint32(2)
	seqB := uint32(2)
	_, err = prepareShardedFarm(connA, &seqA, true)
	if err != nil {
		return fmt.Errorf("prepare A through sharded Farm RPC: %w", err)
	}
	plantedBAt, err := prepareShardedFarm(connB, &seqB, false)
	if err != nil {
		return fmt.Errorf("prepare B through sharded Farm RPC: %w", err)
	}

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

	// A 拜访 B：应转发到 farm-1。
	enterAEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": loginB.UID}),
	})
	if err != nil {
		return fmt.Errorf("player A EnterFarm(B): %w", err)
	}
	seqA++
	var enterA enterFarmResponse
	if err := json.Unmarshal(enterAEnv.Payload, &enterA); err != nil {
		return fmt.Errorf("decode A EnterFarm payload: %w", err)
	}
	if enterA.Relation != "FRIEND" {
		return fmt.Errorf("player A relation = %q, want FRIEND", enterA.Relation)
	}
	if enterA.Snapshot.UnlockedPlots == 0 {
		return fmt.Errorf("player A EnterFarm(B) returned empty snapshot")
	}

	// B 拜访 A：应转发到 farm-0。
	enterBEnv, err := exchangeResponse(connB, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqB,
		Payload:   mustJSON(map[string]any{"owner_uid": loginA.UID}),
	})
	if err != nil {
		return fmt.Errorf("player B EnterFarm(A): %w", err)
	}
	seqB++
	var enterB enterFarmResponse
	if err := json.Unmarshal(enterBEnv.Payload, &enterB); err != nil {
		return fmt.Errorf("decode B EnterFarm payload: %w", err)
	}
	if enterB.Relation != "FRIEND" {
		return fmt.Errorf("player B relation = %q, want FRIEND", enterB.Relation)
	}
	if enterB.Snapshot.UnlockedPlots == 0 {
		return fmt.Errorf("player B EnterFarm(A) returned empty snapshot")
	}

	crop, ok := gameconfig.CropByID(1)
	if !ok {
		return fmt.Errorf("missing white radish crop config")
	}
	seasonMS := gameconfig.SeasonDurationMs(crop, 0, gameconfig.TimeProfileDemo)
	waterSpan := seasonMS * 35 / 100
	sleepUntilElapsed(plantedBAt, waterSpan+250)
	waterEnv, farmDeltaEnv, err := exchangeResponseWithPush(connA, gateway.Envelope{
		Cmd:       gateway.CommandWater,
		ClientSeq: seqA,
		Payload: mustJSON(map[string]any{
			"owner_uid": loginB.UID, "plot_index": 0, "arg": 0,
		}),
	}, gateway.CommandFarmDelta)
	if err != nil {
		return fmt.Errorf("player A help-water B across farms: %w", err)
	}
	if waterEnv.Err != errcode.OK {
		return fmt.Errorf("player A help-water B err = %d, want 0", waterEnv.Err)
	}
	seqA++
	var waterDelta farm.FarmDelta
	if err := json.Unmarshal(farmDeltaEnv.Payload, &waterDelta); err != nil {
		return fmt.Errorf("decode cross-Gateway FarmDelta: %w", err)
	}
	if waterDelta.OwnerUID != loginB.UID || waterDelta.FarmSeq == 0 {
		return fmt.Errorf("cross-Gateway FarmDelta = %#v, want owner=%d and farm_seq>0", waterDelta, loginB.UID)
	}

	sleepUntilElapsed(plantedBAt, seasonMS+500)
	matureEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": loginB.UID}),
	})
	if err != nil {
		return fmt.Errorf("player A refresh mature B farm: %w", err)
	}
	seqA++
	var mature enterFarmResponse
	if err := json.Unmarshal(matureEnv.Payload, &mature); err != nil {
		return fmt.Errorf("decode mature B farm: %w", err)
	}
	if len(mature.Snapshot.Plots) == 0 || mature.Snapshot.Plots[0].FinalYield == 0 {
		return fmt.Errorf("player B plot 0 final yield = 0 after maturity")
	}

	stealEnv, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandSteal,
		ClientSeq: seqA,
		Payload: mustJSON(map[string]any{
			"owner_uid": loginB.UID, "plot_index": 0, "crop_id": 1,
		}),
	})
	if err != nil {
		return fmt.Errorf("player A steal B across farms: %w", err)
	}
	seqA++
	var steal struct {
		Amount uint16 `json:"amount"`
	}
	if err := json.Unmarshal(stealEnv.Payload, &steal); err != nil || steal.Amount == 0 {
		return fmt.Errorf("decode cross-farm steal reward: amount=%d err=%v", steal.Amount, err)
	}

	selfAfterSteal, err := exchangeResponse(connA, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: seqA,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	})
	if err != nil {
		return fmt.Errorf("player A EnterFarm(self) after steal: %w", err)
	}
	seqA++
	var afterSteal enterFarmResponse
	if err := json.Unmarshal(selfAfterSteal.Payload, &afterSteal); err != nil {
		return fmt.Errorf("decode A state after steal: %w", err)
	}
	if got := afterSteal.Snapshot.Warehouse[string(farm.FruitItem(1))]; got < uint32(steal.Amount) {
		return fmt.Errorf("player A authoritative warehouse fruit = %d, want at least %d", got, steal.Amount)
	}
	owner := &smokePlayer{login: loginB, farm: farmB, conn: connB, seq: seqB}
	visitor := &smokePlayer{login: loginA, farm: farmA, conn: connA, seq: seqA}
	if err := runShardedDogIntercept(owner, visitor, gw1); err != nil {
		return fmt.Errorf("cross-Gateway dog intercept: %w", err)
	}
	return nil
}

// runShardedDogIntercept proves the Farm owning the dog can fan a personal
// compensation delta back to its client through a different Gateway than the
// visitor's connection.
func runShardedDogIntercept(owner, visitor *smokePlayer, debugBaseURL string) error {
	crop, ok := gameconfig.CropByID(stealCropID)
	if !ok {
		return fmt.Errorf("missing crop %d", stealCropID)
	}
	compensation := gameconfig.StealCompensation(crop)
	if err := ownerEarnForDog(owner, debugBaseURL, crop); err != nil {
		return fmt.Errorf("earn for dog: %w", err)
	}
	plotIndex := uint32(0)
	if err := ownerResetAndMature(owner, debugBaseURL, plotIndex); err != nil {
		return fmt.Errorf("prepare dog plot: %w", err)
	}
	if _, err := mustOwnerAction(owner, gateway.CommandBuy, map[string]any{
		"item_id": stealDogMuttItem, "quantity": 1,
	}); err != nil {
		return fmt.Errorf("buy dog: %w", err)
	}
	if env, err := ownerExchange(owner, gateway.CommandPetActivate, map[string]any{
		"dog_type": stealDogTypeMutt,
	}); err != nil {
		return fmt.Errorf("PetActivate: %w", err)
	} else if env.Err != errcode.OK {
		return fmt.Errorf("PetActivate err = %d", env.Err)
	}
	if err := visitorEnter(visitor, owner.login.UID); err != nil {
		return fmt.Errorf("visitor enter owner: %w", err)
	}

	for attempt := 0; attempt < stealMaxIntercept; attempt++ {
		if attempt > 0 {
			plotIndex = (plotIndex + 1) % uint32(gameconfig.InitialUnlockedPlots)
			if err := ownerResetAndMature(owner, debugBaseURL, plotIndex); err != nil {
				return fmt.Errorf("prepare intercept plot %d: %w", plotIndex, err)
			}
		}
		if _, err := mustOwnerAction(owner, gateway.CommandBuy, map[string]any{
			"item_id": stealDogFoodItem, "quantity": 80,
		}); err != nil {
			return fmt.Errorf("buy dog food: %w", err)
		}
		if env, err := ownerExchange(owner, gateway.CommandPetFeed, map[string]any{"grams": 80}); err != nil {
			return fmt.Errorf("PetFeed: %w", err)
		} else if env.Err != errcode.OK && env.Err != errcode.BowlFull {
			return fmt.Errorf("PetFeed err = %d", env.Err)
		}
		coinBefore, err := ownerSnapshotCoin(owner)
		if err != nil {
			return fmt.Errorf("owner coin before steal: %w", err)
		}
		env, err := exchangeStealEnvelope(visitor.conn, &visitor.seq, owner.login.UID, plotIndex, stealCropID)
		if err != nil {
			return fmt.Errorf("steal attempt %d: %w", attempt, err)
		}
		if env.Err != errcode.StealIntercepted {
			switch env.Err {
			case errcode.OK, errcode.StealQuotaExhausted, errcode.StealAlreadyDone:
				continue
			default:
				return fmt.Errorf("steal attempt %d err = %d", attempt, env.Err)
			}
		}

		var reward stealRewardResponse
		if err := json.Unmarshal(env.Payload, &reward); err != nil {
			return fmt.Errorf("decode intercept reward: %w", err)
		}
		if int64(reward.Compensation) != compensation || reward.DogType != stealDogTypeMutt {
			return fmt.Errorf("intercept reward = %#v, want compensation=%d dog=%d", reward, compensation, stealDogTypeMutt)
		}
		playerDeltaEnv, err := readServerPushUntil(owner.conn, gateway.CommandPlayerDelta)
		if err != nil {
			return fmt.Errorf("owner PlayerDelta: %w", err)
		}
		var playerDelta farm.PlayerDelta
		if err := json.Unmarshal(playerDeltaEnv.Payload, &playerDelta); err != nil {
			return fmt.Errorf("decode owner PlayerDelta: %w", err)
		}
		if playerDelta.Coin != coinBefore+compensation {
			return fmt.Errorf("owner PlayerDelta coin=%d, want %d", playerDelta.Coin, coinBefore+compensation)
		}
		fmt.Printf("smoke shards dog intercept: 1411 compensation=%d PlayerDelta coin=%d\n", reward.Compensation, playerDelta.Coin)
		return nil
	}
	return fmt.Errorf("no dog interception after %d attempts", stealMaxIntercept)
}

func readServerPushUntil(conn *websocket.Conn, wantCmd uint32) (gateway.Envelope, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return gateway.Envelope{}, err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for attempt := 0; attempt < 64; attempt++ {
		env, err := readServerPush(conn)
		if err != nil {
			return gateway.Envelope{}, err
		}
		if env.Cmd == wantCmd && env.ClientSeq == 0 && env.Err == errcode.OK {
			return env, nil
		}
	}
	return gateway.Envelope{}, fmt.Errorf("missing server push cmd=%d", wantCmd)
}

func prepareShardedFarm(conn *websocket.Conn, seq *uint32, verifyRewards bool) (time.Time, error) {
	enterEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: *seq,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	})
	*seq++
	if err != nil {
		return time.Time{}, fmt.Errorf("EnterFarm(self): %w", err)
	}
	var initial enterFarmResponse
	if err := json.Unmarshal(enterEnv.Payload, &initial); err != nil {
		return time.Time{}, fmt.Errorf("decode initial farm: %w", err)
	}

	buy, err := mustAction(conn, seq, gateway.CommandBuy, map[string]any{"item_id": 1, "quantity": 1})
	if err != nil {
		return time.Time{}, fmt.Errorf("buy: %w", err)
	}
	if _, err := mustAction(conn, seq, gateway.CommandTill, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 0,
	}); err != nil {
		return time.Time{}, fmt.Errorf("till: %w", err)
	}
	plant, err := mustAction(conn, seq, gateway.CommandPlant, map[string]any{
		"owner_uid": 0, "plot_index": 0, "arg": 1,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("plant: %w", err)
	}
	plantedAt := time.Now()
	if plant.FarmSeq <= buy.FarmSeq {
		return time.Time{}, fmt.Errorf("plant farm_seq=%d did not advance after buy=%d", plant.FarmSeq, buy.FarmSeq)
	}

	if _, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandPetStatus,
		ClientSeq: *seq,
		Payload:   emptyJSONObject,
	}); err != nil {
		return time.Time{}, fmt.Errorf("pet status: %w", err)
	}
	*seq++

	syncEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandSyncFarm,
		ClientSeq: *seq,
		Payload:   mustJSON(map[string]any{"owner_uid": 0, "from_seq": 0}),
	})
	*seq++
	if err != nil {
		return time.Time{}, fmt.Errorf("sync farm: %w", err)
	}
	var sync syncFarmResponse
	if err := json.Unmarshal(syncEnv.Payload, &sync); err != nil || sync.FarmSeq < plant.FarmSeq {
		return time.Time{}, fmt.Errorf("decode SyncFarm: farm_seq=%d want >=%d err=%v", sync.FarmSeq, plant.FarmSeq, err)
	}

	taskEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandTaskList,
		ClientSeq: *seq,
		Payload:   emptyJSONObject,
	})
	*seq++
	if err != nil {
		return time.Time{}, fmt.Errorf("TaskList: %w", err)
	}
	var taskList struct {
		Tasks []struct {
			ID       uint32 `json:"id"`
			Progress uint32 `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(taskEnv.Payload, &taskList); err != nil {
		return time.Time{}, fmt.Errorf("decode TaskList: %w", err)
	}
	plantProgress := uint32(0)
	for _, task := range taskList.Tasks {
		if task.ID == 1 {
			plantProgress = task.Progress
		}
	}
	if plantProgress == 0 {
		return time.Time{}, fmt.Errorf("real Plant event did not advance daily task")
	}
	if !verifyRewards {
		return plantedAt, nil
	}

	taskClaimEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandTaskClaim,
		ClientSeq: *seq,
		Payload:   mustJSON(map[string]any{"task_id": 1}),
	})
	*seq++
	if err != nil {
		return time.Time{}, fmt.Errorf("TaskClaim: %w", err)
	}
	var taskReward struct {
		Coin int64 `json:"coin"`
	}
	if err := json.Unmarshal(taskClaimEnv.Payload, &taskReward); err != nil ||
		taskReward.Coin <= 0 {
		return time.Time{}, fmt.Errorf("decode task reward: coin=%d err=%v", taskReward.Coin, err)
	}
	*seq++

	dailyEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandClaimDailyLogin,
		ClientSeq: *seq,
		Payload:   emptyJSONObject,
	})
	*seq++
	if err != nil {
		return time.Time{}, fmt.Errorf("ClaimDailyLogin: %w", err)
	}
	var dailyReward struct {
		Coin int64 `json:"coin"`
	}
	if err := json.Unmarshal(dailyEnv.Payload, &dailyReward); err != nil || dailyReward.Coin <= 0 {
		return time.Time{}, fmt.Errorf("decode daily reward: coin=%d err=%v", dailyReward.Coin, err)
	}
	*seq++
	afterEnv, err := exchangeResponse(conn, gateway.Envelope{
		Cmd:       gateway.CommandEnterFarm,
		ClientSeq: *seq,
		Payload:   mustJSON(map[string]any{"owner_uid": 0}),
	})
	*seq++
	if err != nil {
		return time.Time{}, fmt.Errorf("EnterFarm after direct rewards: %w", err)
	}
	var after enterFarmResponse
	if err := json.Unmarshal(afterEnv.Payload, &after); err != nil {
		return time.Time{}, fmt.Errorf("decode farm after direct rewards: %w", err)
	}
	crop, ok := gameconfig.CropByID(1)
	if !ok {
		return time.Time{}, fmt.Errorf("missing white radish crop config")
	}
	wantCoin := initial.Snapshot.Coin - int64(crop.SeedPrice) + taskReward.Coin + dailyReward.Coin
	if after.Snapshot.Coin != wantCoin {
		return time.Time{}, fmt.Errorf("coin after direct rewards=%d, want %d", after.Snapshot.Coin, wantCoin)
	}
	return plantedAt, nil
}

func sleepUntilElapsed(start time.Time, targetMS int64) {
	remaining := time.Duration(targetMS)*time.Millisecond - time.Since(start)
	if remaining > 0 {
		time.Sleep(remaining)
	}
}

func registerOnFarm(baseURL string, routes *sharding.RouteTable, wantFarm string) (authResponse, string, error) {
	const maxAttempts = 64
	for attempt := 0; attempt < maxAttempts; attempt++ {
		username, err := smokeUsername()
		if err != nil {
			return authResponse{}, "", err
		}
		if _, err := authenticate(baseURL+"/api/register", username, smokePassword); err != nil {
			return authResponse{}, "", fmt.Errorf("register %s: %w", username, err)
		}
		login, err := authenticate(baseURL+"/api/login", username, smokePassword)
		if err != nil {
			return authResponse{}, "", fmt.Errorf("login %s: %w", username, err)
		}
		farmID, err := routes.FarmID(login.UID)
		if err != nil {
			return authResponse{}, "", err
		}
		if farmID == wantFarm {
			return login, farmID, nil
		}
	}
	return authResponse{}, "", fmt.Errorf("after %d registers still no uid on %s", maxAttempts, wantFarm)
}

func loadSmokeRouteTable() (*sharding.RouteTable, error) {
	candidates := []string{
		getenv("FARM_ROUTE_TABLE", ""),
		"deploy/route-table.example.json",
		"../deploy/route-table.example.json",
		"../../deploy/route-table.example.json",
	}
	var lastErr error
	for _, path := range candidates {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		routes, err := sharding.LoadRouteTable(path)
		if err == nil {
			return routes, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("route table not found")
	}
	return nil, lastErr
}

var emptyJSONObject = json.RawMessage(`{}`)
