package gateway

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/platform/actor"
	"farm/server/platform/bus"
	"farm/server/platform/connreg"
	"farm/server/platform/farmrpc"
	"farm/server/platform/gameconf"
	"farm/server/platform/obs"
	"farm/server/platform/pkgerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/routing"
	"farm/server/platform/store"
	"farm/server/platform/wireenv"
)

// Authenticator is the authentication boundary used by HTTP handlers.
type Authenticator interface {
	Register(ctx context.Context, username, password string) (uint64, string, error)
	Login(ctx context.Context, username, password string) (uint64, string, error)
}

// FarmRuntime is the Actor boundary used by EnterFarm.
type FarmRuntime interface {
	Do(uid uint64, fn func(*actor.FarmActor) error) error
}

// Gateway owns the HTTP and WebSocket transport adapters.
type Gateway struct {
	auth                      Authenticator
	sessions                  store.SessionStore
	runtime                   FarmRuntime
	farmRPC                   farmrpc.Client
	routes                    *routing.RouteTable
	friends                   store.FriendStore
	inviteSecret              []byte
	now                       func() int64
	timeProfiles              *gameconf.TimeProfileSwitch
	debugTimeProfileMu        sync.Mutex
	offsetMs                  atomic.Int64
	rooms                     *RoomHub
	nextConnID                atomic.Uint64
	connRegistry              *connreg.Registry
	gatewayID                 string
	connectionMu              sync.Mutex
	connections               sync.Map
	pushToken                 []byte
	allowDebug                bool
	crossBus                  bus.EventBus
	crossEnabled              bool
	crossPending              sync.Map
	nextCrossReqID            atomic.Uint64
	crossSubscribeErr         error
	stealHints                store.StealHintStore
	taskMail                  store.TaskMailStore
	codexRewards              store.CodexRewardStore
	taskNotifyFanout          farmrpc.TaskNotifyPublisher
	sessionKickPusher         farmrpc.SessionKickPusher
	taskNotifyDelivery        func(*wsConnection, store.Task) error
	mailNotifyDelivery        func(*wsConnection, string) error
	afterConnectionRegistered func(*wsConnection) // test seam for pre-ready pushes
	debugFarmURLs             map[string]string
	debugGatewayURLs          map[string]string
	debugFarmToken            string
	metrics                   *obs.Metrics
}

// Option configures optional Gateway boundaries.
type Option func(*Gateway)

// WithTimeProfile sets the process-wide authoritative farm clock profile.
// Every Gateway/Farm instance in one deployment must receive the same value.
func WithTimeProfile(profile string) Option {
	return func(gateway *Gateway) {
		if gameconf.ValidTimeProfile(profile) {
			gateway.timeProfiles = gameconf.NewTimeProfileSwitch(profile)
		}
	}
}

// WithTimeProfileSwitch shares the process-wide runtime profile switch with
// Farm RPC handlers and debug endpoints.
func WithTimeProfileSwitch(profiles *gameconf.TimeProfileSwitch) Option {
	return func(gateway *Gateway) {
		if profiles != nil {
			gateway.timeProfiles = profiles
		}
	}
}

// WithFriendStore configures the friendship persistence boundary.
func WithFriendStore(friends store.FriendStore) Option {
	return func(gateway *Gateway) {
		gateway.friends = friends
	}
}

// WithStealHintStore configures the weak-consistent stealable-farm hint store.
func WithStealHintStore(hints store.StealHintStore) Option {
	return func(gateway *Gateway) {
		gateway.stealHints = hints
	}
}

// WithTaskMailStore configures task, mail and daily-login persistence.
func WithTaskMailStore(taskMail store.TaskMailStore) Option {
	return func(gateway *Gateway) {
		gateway.taskMail = taskMail
	}
}

// WithCodexRewardStore enables idempotent per-crop plaque reward mails.
func WithCodexRewardStore(rewards store.CodexRewardStore) Option {
	return func(gateway *Gateway) {
		gateway.codexRewards = rewards
	}
}

// WithTaskNotifyFanout forwards local Gateway-owned task updates to every
// connection leased for the player across Gateway instances.
func WithTaskNotifyFanout(publisher farmrpc.TaskNotifyPublisher) Option {
	return func(gateway *Gateway) {
		gateway.taskNotifyFanout = publisher
	}
}

// WithSessionKickPusher forwards a replacement notice to an evicted connection
// when that connection is owned by another Gateway instance.
func WithSessionKickPusher(pusher farmrpc.SessionKickPusher) Option {
	return func(gateway *Gateway) {
		gateway.sessionKickPusher = pusher
	}
}

// WithInviteSecret configures the HMAC secret used for sharing invitations.
func WithInviteSecret(secret []byte) Option {
	secret = append([]byte(nil), secret...)
	return func(gateway *Gateway) {
		gateway.inviteSecret = secret
	}
}

// WithFarmRPC enables routed Farm commands for a gateway-only process. The
// route table is immutable after startup, so concurrent WebSocket requests may
// safely share it.
func WithFarmRPC(client farmrpc.Client, routes *routing.RouteTable) Option {
	return func(gateway *Gateway) {
		gateway.farmRPC = client
		gateway.routes = routes
	}
}

// WithDebugTimeFanout fans /api/debug/advance to every Farm and peer Gateway
// so sharded smoke keeps all process clocks aligned. Peer Gateways receive the
// local-only internal endpoint to avoid recursive fan-out.
func WithDebugTimeFanout(farmURLs, gatewayURLs map[string]string, token string) Option {
	farms := make(map[string]string, len(farmURLs))
	for id, endpoint := range farmURLs {
		farms[id] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	gateways := make(map[string]string, len(gatewayURLs))
	for id, endpoint := range gatewayURLs {
		gateways[id] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return func(gateway *Gateway) {
		gateway.debugFarmURLs = farms
		gateway.debugGatewayURLs = gateways
		gateway.debugFarmToken = strings.TrimSpace(token)
	}
}

// WithConnectionRegistry enables distributed WebSocket registration for this
// Gateway instance. gatewayID must match the ID used by Farm push routing.
func WithConnectionRegistry(registry *connreg.Registry, gatewayID string) Option {
	return func(gateway *Gateway) {
		gateway.connRegistry = registry
		gateway.gatewayID = strings.TrimSpace(gatewayID)
	}
}

// WithInternalPushToken configures authentication for Farm-to-Gateway Delta
// callbacks. An empty token keeps the push endpoint disabled.
func WithInternalPushToken(token string) Option {
	return func(gateway *Gateway) {
		gateway.pushToken = []byte(strings.TrimSpace(token))
	}
}

// WithMetrics attaches Prometheus collectors for WS and FarmDelta instrumentation.
func WithMetrics(m *obs.Metrics) Option {
	return func(gateway *Gateway) {
		gateway.metrics = m
		if gateway.rooms != nil {
			gateway.rooms.metrics = m
		}
	}
}

// New constructs the transport gateway from its application boundaries.
func New(auth Authenticator, sessions store.SessionStore, runtime FarmRuntime, options ...Option) *Gateway {
	gateway := &Gateway{
		auth:         auth,
		sessions:     sessions,
		runtime:      runtime,
		rooms:        NewRoomHub(),
		now:          func() int64 { return time.Now().UnixMilli() },
		timeProfiles: gameconf.NewTimeProfileSwitch(gameconf.TimeProfileDemo),
	}
	for _, option := range options {
		if option != nil {
			option(gateway)
		}
	}
	// Random non-sequential start so a restarted Gateway with the same
	// gatewayID cannot reuse conn_id=1 against leftover connreg leases.
	gateway.nextConnID.Store(connectionIDSeed())
	gateway.nextCrossReqID.Store(crossRequestSeed())
	gateway.startCrossResultConsumer()
	return gateway
}

func crossRequestSeed() uint64 {
	var seed [8]byte
	if _, err := cryptorand.Read(seed[:]); err == nil {
		return binary.LittleEndian.Uint64(seed[:])
	}
	// crypto/rand failures are exceptional. A time-based fallback preserves
	// availability; the per-Gateway sequence still prevents local collisions.
	return uint64(time.Now().UnixNano())
}

// connectionIDSeed returns a crypto-random 64-bit counter base. The first
// allocateConnID call yields seed+1 (skipping 0 on wrap), so process restarts
// do not collide with low IDs left in Redis by a crashed peer process.
func connectionIDSeed() uint64 {
	var seed [8]byte
	if _, err := cryptorand.Read(seed[:]); err == nil {
		return binary.LittleEndian.Uint64(seed[:])
	}
	return uint64(time.Now().UnixNano())
}

// allocateConnID returns the next local connection identity. Public protocol
// conn_id remains uint64; 0 is reserved/invalid and is never issued.
func allocateConnID(counter *atomic.Uint64) uint64 {
	for {
		id := counter.Add(1)
		if id != 0 {
			return id
		}
	}
}

// EnableDebugTime 打开 /api/debug/advance（仅非生产冒烟；由 FARM_ALLOW_DEBUG_TIME 门控）。
func (g *Gateway) EnableDebugTime() {
	g.allowDebug = true
}

// SetClock 注入可测时钟（毫秒）；测试用。nil 恢复为真实时间。
func (g *Gateway) SetClock(now func() int64) {
	if now == nil {
		g.now = func() int64 { return time.Now().UnixMilli() }
		return
	}
	g.now = now
}

// Now 返回服务端逻辑时间（真实/注入时钟 + debug 偏移）。
func (g *Gateway) Now() int64 {
	return g.now() + g.offsetMs.Load()
}

// TimeProfile returns the current server-authoritative runtime profile.
func (g *Gateway) TimeProfile() string {
	return g.timeProfiles.Get()
}

// Handler returns the complete HTTP routing surface of the gateway.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", g.register)
	mux.HandleFunc("/api/login", g.login)
	mux.HandleFunc("/i/", g.inviteLanding)
	mux.HandleFunc("/ws", g.serveWS)
	mux.HandleFunc("/internal/v1/push/farm-delta", g.receiveFarmDelta)
	mux.HandleFunc("/internal/v1/push/farm-delta-batch", g.receiveFarmDeltaBatch)
	mux.HandleFunc("/internal/v1/push/player-delta", g.receivePlayerDelta)
	mux.HandleFunc("/internal/v1/push/task-notify", g.receiveTaskNotify)
	mux.HandleFunc("/internal/v1/push/session-kick", g.receiveSessionKick)
	if g.allowDebug {
		mux.HandleFunc("/api/debug/advance", g.debugAdvance)
		mux.HandleFunc("/internal/v1/debug/advance", g.debugAdvanceLocal)
		mux.HandleFunc("/internal/v1/debug/time-profile", g.debugTimeProfileLocal)
	}
	return mux
}

func (g *Gateway) executeFarmRPC(ctx context.Context, uid uint64, command farmrpc.CommandRequest) (farmrpc.CommandResponse, error) {
	if g.farmRPC == nil || g.routes == nil {
		return farmrpc.CommandResponse{}, errors.New("gateway: farm RPC is not configured")
	}
	farmID, err := g.routes.FarmID(uid)
	if err != nil {
		return farmrpc.CommandResponse{}, err
	}
	command.FarmUID = uid
	if command.Originator.ConnID != 0 && command.Originator.GatewayID == "" {
		command.Originator.GatewayID = g.gatewayID
	}
	return g.farmRPC.Execute(ctx, farmID, command)
}

func (g *Gateway) connectionRef(connection *wsConnection) connreg.ConnRef {
	if connection == nil {
		return connreg.ConnRef{}
	}
	return connreg.ConnRef{ConnID: connection.id, GatewayID: g.gatewayID}
}

func (g *Gateway) registerConnection(ctx context.Context, connection *wsConnection) error {
	if connection == nil || connection.id == 0 || connection.uid == 0 || !connection.authed {
		return errors.New("gateway: invalid websocket connection")
	}
	g.connectionMu.Lock()
	if g.connRegistry == nil {
		var evicted []*wsConnection
		g.connections.Range(func(_, value any) bool {
			existing, ok := value.(*wsConnection)
			if ok && existing != nil && existing.uid == connection.uid && existing.id != connection.id {
				g.connections.Delete(existing.id)
				evicted = append(evicted, existing)
			}
			return true
		})
		g.connections.Store(connection.id, connection)
		g.connectionMu.Unlock()
		g.afterRegisterConnection(connection)
		for _, existing := range evicted {
			existing.kick(pkgerr.Kicked)
		}
		return nil
	}
	if g.gatewayID == "" {
		g.connectionMu.Unlock()
		return errors.New("gateway: connection registry requires a gateway ID")
	}
	evicted, err := g.connRegistry.ReplaceConnection(ctx, connection.uid, connection.id, g.gatewayID)
	if err != nil {
		g.connectionMu.Unlock()
		return err
	}
	g.connections.Store(connection.id, connection)
	g.connectionMu.Unlock()
	g.afterRegisterConnection(connection)
	g.kickReplacedConnections(ctx, connection.uid, evicted)
	return nil
}

func (g *Gateway) kickReplacedConnections(ctx context.Context, uid uint64, refs []connreg.ConnRef) {
	for _, ref := range refs {
		if ref.ConnID == 0 {
			continue
		}
		if ref.GatewayID == g.gatewayID {
			value, ok := g.connections.Load(ref.ConnID)
			if !ok {
				continue
			}
			connection, ok := value.(*wsConnection)
			if ok && connection != nil && connection.uid == uid {
				connection.kick(pkgerr.Kicked)
			}
			continue
		}
		if g.sessionKickPusher == nil {
			obs.L().Error("gateway session kick pusher is not configured",
				"component", "gateway",
				"op", "session_kick",
				"uid", uid,
				"gateway_id", ref.GatewayID,
				"conn_id", ref.ConnID,
			)
			continue
		}
		if err := g.sessionKickPusher.PushSessionKick(ctx, ref, uid, pkgerr.Kicked); err != nil {
			obs.L().Error("gateway session kick push failed",
				"component", "gateway",
				"op", "session_kick",
				"uid", uid,
				"gateway_id", ref.GatewayID,
				"conn_id", ref.ConnID,
				"err", err.Error(),
			)
		}
	}
}

func (g *Gateway) afterRegisterConnection(connection *wsConnection) {
	if g.afterConnectionRegistered != nil {
		g.afterConnectionRegistered(connection)
	}
}

func (g *Gateway) unregisterConnection(ctx context.Context, connection *wsConnection) {
	if connection == nil || connection.id == 0 {
		return
	}
	g.connectionMu.Lock()
	defer g.connectionMu.Unlock()
	g.connections.Delete(connection.id)
	if g.connRegistry != nil && connection.uid != 0 {
		_ = g.connRegistry.Unregister(ctx, connection.uid, connection.id, g.gatewayID)
	}
}

// renewConnectionLease verifies that this connection still owns the player's
// lifecycle lease, then best-effort extends its lifecycle and room leases.
// False means a newer connection has replaced it and no command may execute.
func (g *Gateway) renewConnectionLease(ctx context.Context, connection *wsConnection) bool {
	if g == nil || connection == nil || !connection.authed || connection.uid == 0 || connection.id == 0 {
		return false
	}
	if g.connRegistry == nil {
		value, ok := g.connections.Load(connection.id)
		if ok {
			return value == connection
		}
		replaced := false
		g.connections.Range(func(_, value any) bool {
			current, valid := value.(*wsConnection)
			if valid && current != nil && current.uid == connection.uid {
				replaced = true
				return false
			}
			return true
		})
		return !replaced
	}
	if g.gatewayID == "" {
		return false
	}
	if err := g.connRegistry.Register(ctx, connection.uid, connection.id, g.gatewayID); err != nil {
		if errors.Is(err, connreg.ErrAlreadyConnected) {
			return false
		}
		obs.L().Error("gateway connreg renew lifecycle failed",
			"component", "gateway",
			"op", "connreg_renew_lifecycle",
			"uid", connection.uid,
			"conn_id", connection.id,
			"err", err.Error(),
		)
		return true
	}
	connection.roomMu.Lock()
	roomUID := connection.roomUID
	connection.roomMu.Unlock()
	if roomUID == 0 {
		return true
	}
	if err := g.connRegistry.Subscribe(ctx, roomUID, connection.id, g.gatewayID); err != nil {
		obs.L().Error("gateway connreg renew room failed",
			"component", "gateway",
			"op", "connreg_renew_room",
			"uid", connection.uid,
			"owner_uid", roomUID,
			"conn_id", connection.id,
			"err", err.Error(),
		)
	}
	return true
}

func (g *Gateway) receiveFarmDelta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.authorizedPush(r.Header.Get("Authorization")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request farmrpc.DeltaPushRequest
	if err := decodeJSON(io.LimitReader(r.Body, 64<<10), &request); err != nil ||
		request.ConnectionID == 0 || request.Delta.OwnerUID == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	connection, ok := g.connections.Load(request.ConnectionID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	wsConn := connection.(*wsConnection)
	if !wsConn.subscribedTo(request.Delta.OwnerUID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	encoded, err := wireenv.EncodeFarmDelta(request.Delta)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := wsConn.pushFarmDelta(request.Delta.OwnerUID, request.Delta, encoded); err != nil {
		obs.L().Warn("gateway FarmDelta push failed",
			"component", "gateway",
			"op", "push_farm_delta",
			"owner_uid", request.Delta.OwnerUID,
			"farm_seq", request.Delta.FarmSeq,
			"err", err.Error(),
		)
		_ = wsConn.conn.Close()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) receiveFarmDeltaBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.authorizedPush(r.Header.Get("Authorization")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request farmrpc.PushBatch
	if err := decodeJSON(io.LimitReader(r.Body, 1<<20), &request); err != nil ||
		len(request.ConnIDs) == 0 || len(request.Envelope) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Strict validation before any fan-out: malformed pre-encoded frames must
	// never be written to clients.
	delta, err := wireenv.DecodeFarmDelta(request.Envelope)
	if err != nil || delta.OwnerUID == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Invalid / stale conn_ids are skipped; they must not fail the whole batch.
	// Duplicate conn_ids are ignored so one connection is not written twice.
	seen := make(map[uint64]struct{}, len(request.ConnIDs))
	var firstWriteErr error
	for _, connID := range request.ConnIDs {
		if connID == 0 {
			continue
		}
		if _, dup := seen[connID]; dup {
			continue
		}
		seen[connID] = struct{}{}
		connection, ok := g.connections.Load(connID)
		if !ok {
			continue
		}
		wsConn := connection.(*wsConnection)
		if !wsConn.subscribedTo(delta.OwnerUID) {
			continue
		}
		if err := wsConn.pushFarmDelta(delta.OwnerUID, delta, request.Envelope); err != nil {
			if firstWriteErr == nil {
				firstWriteErr = err
			}
			obs.L().Warn("gateway batched FarmDelta push failed",
				"component", "gateway",
				"op", "push_farm_delta_batch",
				"owner_uid", delta.OwnerUID,
				"farm_seq", delta.FarmSeq,
				"err", err.Error(),
			)
			_ = wsConn.conn.Close()
		}
	}
	if firstWriteErr != nil {
		// Farm 会对 5xx 做有限重试；其他已成功连接可能收到重复 Delta，
		// 客户端按 farm_seq 幂等忽略，避免局部写失败变成永久丢失。
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) receivePlayerDelta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.authorizedPush(r.Header.Get("Authorization")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request farmrpc.PlayerDeltaPushRequest
	if err := decodeJSON(io.LimitReader(r.Body, 64<<10), &request); err != nil ||
		request.ConnectionID == 0 || request.UID == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	connection, ok := g.connections.Load(request.ConnectionID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	wsConnection := connection.(*wsConnection)
	if wsConnection.uid == request.UID {
		wsConnection.pushPlayerDelta(request.Delta)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) receiveTaskNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.authorizedPush(r.Header.Get("Authorization")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request farmrpc.TaskNotifyPushRequest
	if err := decodeJSON(io.LimitReader(r.Body, 64<<10), &request); err != nil ||
		request.ConnectionID == 0 || request.UID == 0 || !isTaskNotifyID(request.Task.ID) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	connection, ok := g.connections.Load(request.ConnectionID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	wsConnection := connection.(*wsConnection)
	if wsConnection.uid == request.UID && wsConnection.authed {
		wsConnection.enqueueTaskNotify(request.Task)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) receiveSessionKick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.authorizedPush(r.Header.Get("Authorization")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var request farmrpc.SessionKickPushRequest
	if err := decodeJSON(io.LimitReader(r.Body, 64<<10), &request); err != nil ||
		request.ConnectionID == 0 || request.UID == 0 || request.Reason != pkgerr.Kicked {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	value, ok := g.connections.Load(request.ConnectionID)
	if ok {
		connection, valid := value.(*wsConnection)
		if valid && connection != nil && connection.authed && connection.uid == request.UID {
			connection.kick(request.Reason)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) authorizedPush(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) || len(g.pushToken) == 0 {
		return false
	}
	value := []byte(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
	return len(value) == len(g.pushToken) && subtle.ConstantTimeCompare(value, g.pushToken) == 1
}

type debugAdvanceRequest struct {
	MS int64 `json:"ms"`
}

type debugTimeProfileRequest struct {
	TimeProfile string `json:"time_profile"`
}

func (g *Gateway) debugAdvance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusBadRequest)
		return
	}
	var req debugAdvanceRequest
	if err := json.Unmarshal(body, &req); err != nil || req.MS <= 0 {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusBadRequest)
		return
	}
	g.offsetMs.Add(req.MS)
	if err := g.fanoutDebugAdvance(r.Context(), req.MS); err != nil {
		writeHTTPError(w, pkgerr.Internal, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"server_time": g.Now()})
}

// debugAdvanceLocal advances only this Gateway's clock. Used by peer fan-out
// so /api/debug/advance does not recurse across the Gateway mesh.
func (g *Gateway) debugAdvanceLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if g.debugFarmToken != "" {
		want := "Bearer " + g.debugFarmToken
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req debugAdvanceRequest
	if err := json.Unmarshal(body, &req); err != nil || req.MS <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.offsetMs.Add(req.MS)
	writeJSON(w, http.StatusOK, map[string]int64{"server_time": g.Now()})
}

// debugTimeProfileLocal updates only this Gateway process. Gateway peers use
// it during debug fan-out; the bearer token prevents public direct access.
func (g *Gateway) debugTimeProfileLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if g.debugFarmToken != "" {
		want := "Bearer " + g.debugFarmToken
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	var req debugTimeProfileRequest
	if err := decodeJSON(io.LimitReader(r.Body, 4<<10), &req); err != nil ||
		!gameconf.ValidTimeProfile(req.TimeProfile) ||
		!g.timeProfiles.Set(req.TimeProfile) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"time_profile": g.TimeProfile()})
}

func (g *Gateway) fanoutDebugAdvance(ctx context.Context, ms int64) error {
	body, err := json.Marshal(debugAdvanceRequest{MS: ms})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(label, url string) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("gateway: build debug advance for %s: %w", label, err)
		}
		if g.debugFarmToken != "" {
			request.Header.Set("Authorization", "Bearer "+g.debugFarmToken)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("gateway: debug advance %s: %w", label, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("gateway: debug advance %s HTTP %d", label, response.StatusCode)
		}
		return nil
	}
	for farmID, endpoint := range g.debugFarmURLs {
		if err := post(farmID, endpoint+"/internal/v1/debug/advance"); err != nil {
			return err
		}
	}
	for peerID, endpoint := range g.debugGatewayURLs {
		if peerID == g.gatewayID {
			continue
		}
		if err := post(peerID, endpoint+"/internal/v1/debug/advance"); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) switchTimeProfile(ctx context.Context, profile string) error {
	if !gameconf.ValidTimeProfile(profile) {
		return errors.New("gateway: invalid debug time profile")
	}
	g.debugTimeProfileMu.Lock()
	defer g.debugTimeProfileMu.Unlock()

	previous := g.TimeProfile()
	if previous == profile {
		return nil
	}
	if err := g.fanoutDebugTimeProfile(ctx, profile); err != nil {
		// 每个远端端点会在收到 POST 时立即切换。如果中途失败，必须把所有
		// 目标恢复为旧值（而不是只恢复已知成功者：断连可能发生在对方已提交
		// 但响应尚未返回之后）。这样重试会收敛到一个全服档位。
		if rollbackErr := g.fanoutDebugTimeProfile(ctx, previous); rollbackErr != nil {
			return fmt.Errorf("gateway: switch time profile: %w; rollback to %q: %v", err, previous, rollbackErr)
		}
		return err
	}
	if !g.timeProfiles.Set(profile) {
		return errors.New("gateway: set local debug time profile")
	}
	return nil
}

func (g *Gateway) fanoutDebugTimeProfile(ctx context.Context, profile string) error {
	body, err := json.Marshal(debugTimeProfileRequest{TimeProfile: profile})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(label, url string) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("gateway: build debug time profile for %s: %w", label, err)
		}
		if g.debugFarmToken != "" {
			request.Header.Set("Authorization", "Bearer "+g.debugFarmToken)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("gateway: debug time profile %s: %w", label, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("gateway: debug time profile %s HTTP %d", label, response.StatusCode)
		}
		return nil
	}
	for _, target := range g.debugTimeProfileTargets() {
		if err := post(target.label, target.url); err != nil {
			return err
		}
	}
	return nil
}

type debugTimeProfileTarget struct {
	label string
	url   string
}

// debugTimeProfileTargets produces a stable fan-out order. Apart from making
// failures reproducible, it lets a failed switch deterministically run the
// rollback pass across exactly the same Farm and Gateway endpoints.
func (g *Gateway) debugTimeProfileTargets() []debugTimeProfileTarget {
	targets := make([]debugTimeProfileTarget, 0, len(g.debugFarmURLs)+len(g.debugGatewayURLs))
	for farmID, endpoint := range g.debugFarmURLs {
		targets = append(targets, debugTimeProfileTarget{
			label: "farm " + farmID,
			url:   endpoint + "/internal/v1/debug/time-profile",
		})
	}
	for gatewayID, endpoint := range g.debugGatewayURLs {
		if gatewayID == g.gatewayID {
			continue
		}
		targets = append(targets, debugTimeProfileTarget{
			label: "gateway " + gatewayID,
			url:   endpoint + "/internal/v1/debug/time-profile",
		})
	}
	sort.Slice(targets, func(left, right int) bool {
		return targets[left].label < targets[right].label
	})
	return targets
}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResponse struct {
	UID   pkgjson.UID `json:"uid"`
	Token string      `json:"token"`
	WSURL string      `json:"ws_url"`
}

type errorResponse struct {
	Err pkgerr.Code `json:"err"`
}

func (g *Gateway) register(w http.ResponseWriter, r *http.Request) {
	g.handleAuth(w, r, func(ctx context.Context, username, password string) (uint64, string, error) {
		return g.auth.Register(ctx, username, password)
	})
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request) {
	g.handleAuth(w, r, func(ctx context.Context, username, password string) (uint64, string, error) {
		return g.auth.Login(ctx, username, password)
	})
}

func (g *Gateway) handleAuth(
	w http.ResponseWriter,
	r *http.Request,
	authenticate func(context.Context, string, string) (uint64, string, error),
) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusMethodNotAllowed)
		return
	}

	var request authRequest
	if err := decodeJSON(r.Body, &request); err != nil || strings.TrimSpace(request.Username) == "" || request.Password == "" {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusBadRequest)
		return
	}

	uid, token, err := authenticate(r.Context(), request.Username, request.Password)
	if err != nil {
		code := errorCode(err)
		status := http.StatusBadRequest
		if code == pkgerr.Internal {
			status = http.StatusInternalServerError
		}
		writeHTTPError(w, code, status)
		return
	}

	writeJSON(w, http.StatusOK, authResponse{
		UID:   pkgjson.UID(uid),
		Token: token,
		WSURL: websocketURL(r),
	})
}

func decodeJSON(body io.Reader, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("gateway: request body has trailing JSON")
	}
	return nil
}

func websocketURL(r *http.Request) string {
	scheme := "ws"
	if r.TLS != nil {
		scheme = "wss"
	}
	return scheme + "://" + r.Host + "/ws"
}

func errorCode(err error) pkgerr.Code {
	var code pkgerr.Code
	if errors.As(err, &code) {
		return code
	}
	return pkgerr.Internal
}

func writeHTTPError(w http.ResponseWriter, code pkgerr.Code, status int) {
	writeJSON(w, status, errorResponse{Err: code})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
