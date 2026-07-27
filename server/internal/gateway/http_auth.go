package gateway

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/bus"
	"farm/server/internal/connreg"
	"farm/server/internal/cross"
	"farm/server/internal/farmrpc"
	"farm/server/internal/pkgerr"
	"farm/server/internal/routing"
	"farm/server/internal/store"
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
	crossVisitor      *cross.Visitor
	crossPending      sync.Map
	nextCrossReqID    atomic.Uint64
	crossSubscribeErr error
}

// Option configures optional Gateway boundaries.
type Option func(*Gateway)

// WithFriendStore configures the friendship persistence boundary.
func WithFriendStore(friends store.FriendStore) Option {
	return func(gateway *Gateway) {
		gateway.friends = friends
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
	if g.allowDebug {
		mux.HandleFunc("/api/debug/advance", g.debugAdvance)
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
	return g.farmRPC.Execute(ctx, farmID, command)
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
		_ = g.connRegistry.Unregister(ctx, connection.uid, connection.id)
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
	connection.(*wsConnection).pushFarmDelta(request.Delta.OwnerUID, request.Delta)
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
	writeJSON(w, http.StatusOK, map[string]int64{"server_time": g.Now()})
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
