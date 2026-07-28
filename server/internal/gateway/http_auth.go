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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/bus"
	"farm/server/internal/connreg"
	"farm/server/internal/farmrpc"
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/routing"
	"farm/server/internal/store"
	"farm/server/internal/wireenv"
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
	auth              Authenticator
	sessions          store.SessionStore
	runtime           FarmRuntime
	farmRPC           farmrpc.Client
	routes            *routing.RouteTable
	friends           store.FriendStore
	inviteSecret      []byte
	now               func() int64
	offsetMs          atomic.Int64
	rooms             *RoomHub
	nextConnID        atomic.Uint64
	connRegistry      *connreg.Registry
	gatewayID         string
	connections       sync.Map
	pushToken         []byte
	allowDebug        bool
	crossBus          bus.EventBus
	crossEnabled      bool
	crossPending      sync.Map
	nextCrossReqID    atomic.Uint64
	crossSubscribeErr error
	stealHints        store.StealHintStore
	taskMail          store.TaskMailStore
	debugFarmURLs     map[string]string
	debugGatewayURLs  map[string]string
	debugFarmToken    string
	metrics           *obs.Metrics
}

// Option configures optional Gateway boundaries.
type Option func(*Gateway)

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
		auth:     auth,
		sessions: sessions,
		runtime:  runtime,
		rooms:    NewRoomHub(),
		now:      func() int64 { return time.Now().UnixMilli() },
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
	if g.allowDebug {
		mux.HandleFunc("/api/debug/advance", g.debugAdvance)
		mux.HandleFunc("/internal/v1/debug/advance", g.debugAdvanceLocal)
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
	if connection == nil || connection.id == 0 || connection.uid == 0 {
		return errors.New("gateway: invalid websocket connection")
	}
	if g.connRegistry == nil {
		return nil
	}
	if g.gatewayID == "" {
		return errors.New("gateway: connection registry requires a gateway ID")
	}
	g.connections.Store(connection.id, connection)
	if err := g.connRegistry.Register(ctx, connection.uid, connection.id, g.gatewayID); err != nil {
		g.connections.Delete(connection.id)
		return err
	}
	return nil
}

func (g *Gateway) unregisterConnection(ctx context.Context, connection *wsConnection) {
	if connection == nil || connection.id == 0 {
		return
	}
	g.connections.Delete(connection.id)
	if g.connRegistry != nil && connection.uid != 0 {
		_ = g.connRegistry.Unregister(ctx, connection.uid, connection.id, g.gatewayID)
	}
}

// renewConnectionLease best-effort extends lifecycle and current room leases.
// Failures are logged only: the already-handled WS command must not flip to error.
func (g *Gateway) renewConnectionLease(ctx context.Context, connection *wsConnection) {
	if g == nil || g.connRegistry == nil || connection == nil || !connection.authed || connection.uid == 0 || connection.id == 0 {
		return
	}
	if g.gatewayID == "" {
		return
	}
	if err := g.connRegistry.Register(ctx, connection.uid, connection.id, g.gatewayID); err != nil {
		obs.L().Error("gateway connreg renew lifecycle failed",
			"component", "gateway",
			"op", "connreg_renew_lifecycle",
			"uid", connection.uid,
			"conn_id", connection.id,
			"err", err.Error(),
		)
	}
	connection.roomMu.Lock()
	roomUID := connection.roomUID
	connection.roomMu.Unlock()
	if roomUID == 0 {
		return
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
	wsConn.pushFarmDelta(request.Delta.OwnerUID, request.Delta, encoded)
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
		wsConn.pushFarmDelta(delta.OwnerUID, delta, request.Envelope)
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

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResponse struct {
	UID   uint64 `json:"uid"`
	Token string `json:"token"`
	WSURL string `json:"ws_url"`
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
		UID:   uid,
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
