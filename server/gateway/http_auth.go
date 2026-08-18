package gateway

import (
	"context"
	cryptorand "crypto/rand"
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

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientjson"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/grpcx"
	"farm/server/shared/presence"
	"farm/server/shared/sharding"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

// Authenticator is the authentication boundary used by HTTP handlers.
type Authenticator interface {
	Register(ctx context.Context, username, password string) (uint64, string, error)
	Login(ctx context.Context, username, password string) (uint64, string, error)
}

// SocialCommandClient is Social's complete public-command boundary.
type SocialCommandClient interface {
	ExecuteClientCommand(context.Context, *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error)
}

// FarmCommandClient is the transport contract used to route one typed command
// to a Farm shard. Gateway deliberately does not depend on the Farm package.
type FarmCommandClient interface {
	Execute(context.Context, string, *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error)
}

// SessionKickPusher is the transport callback for a connection owned by a
// different Gateway instance.
type SessionKickPusher interface {
	PushSessionKick(context.Context, presence.ConnRef, uint64, errcode.Code) error
}

// Gateway owns the HTTP and WebSocket transport adapters.
type Gateway struct {
	auth                      Authenticator
	sessions                  store.SessionStore
	farmRPC                   FarmCommandClient
	socialRPC                 SocialCommandClient
	routes                    *sharding.RouteTable
	now                       func() int64
	timeProfiles              *gameconfig.TimeProfileSwitch
	debugTimeProfileMu        sync.Mutex
	offsetMs                  atomic.Int64
	nextConnID                atomic.Uint64
	connRegistry              *presence.Registry
	gatewayID                 string
	connectionMu              sync.Mutex
	connections               sync.Map
	allowDebug                bool
	disableWSRateLimit        bool
	sessionKickPusher         SessionKickPusher
	afterConnectionRegistered func(*wsConnection) // test seam for pre-ready pushes
	debugFanout               *DebugFanout
	metrics                   *telemetry.Metrics
	apiDocs                   http.Handler
}

// Option configures optional Gateway boundaries.
type Option func(*Gateway)

// WithTimeProfile sets the process-wide authoritative farm clock profile.
// Every Gateway/Farm instance in one deployment must receive the same value.
func WithTimeProfile(profile string) Option {
	return func(gateway *Gateway) {
		if gameconfig.ValidTimeProfile(profile) {
			gateway.timeProfiles = gameconfig.NewTimeProfileSwitch(profile)
		}
	}
}

// WithTimeProfileSwitch shares the process-wide runtime profile switch with
// Farm RPC handlers and debug endpoints.
func WithTimeProfileSwitch(profiles *gameconfig.TimeProfileSwitch) Option {
	return func(gateway *Gateway) {
		if profiles != nil {
			gateway.timeProfiles = profiles
		}
	}
}

// WithSessionKickPusher forwards a replacement notice to an evicted connection
// when that connection is owned by another Gateway instance.
func WithSessionKickPusher(pusher SessionKickPusher) Option {
	return func(gateway *Gateway) {
		gateway.sessionKickPusher = pusher
	}
}

// WithFarmRPC enables routed Farm commands for a gateway-only process. The
// route table is immutable after startup, so concurrent WebSocket requests may
// safely share it.
func WithFarmRPC(client FarmCommandClient, routes *sharding.RouteTable) Option {
	return func(gateway *Gateway) {
		gateway.farmRPC = client
		gateway.routes = routes
	}
}

// WithSocialRPC configures the sole Social application boundary.
func WithSocialRPC(client SocialCommandClient) Option {
	return func(gateway *Gateway) { gateway.socialRPC = client }
}

// WithDebugTimeFanout fans /api/debug/advance to every Farm and peer Gateway
// over gRPC so sharded smoke keeps all process clocks aligned.
func WithDebugTimeFanout(
	pool *grpcx.Pool,
	farmTargets, gatewayTargets map[string]string,
	localGatewayID string,
	directories ...GatewayPeerDirectory,
) Option {
	return func(gateway *Gateway) {
		gateway.debugFanout = NewDebugFanout(pool, farmTargets, gatewayTargets, localGatewayID, directories...)
	}
}

// WithConnectionRegistry enables distributed WebSocket registration for this
// Gateway instance. gatewayID must match the ID used by Farm push sharding.
func WithConnectionRegistry(registry *presence.Registry, gatewayID string) Option {
	return func(gateway *Gateway) {
		gateway.connRegistry = registry
		gateway.gatewayID = strings.TrimSpace(gatewayID)
	}
}

// WithMetrics attaches Prometheus collectors for WS and FarmDelta instrumentation.
func WithMetrics(m *telemetry.Metrics) Option {
	return func(gateway *Gateway) {
		gateway.metrics = m
	}
}

// WithWSRateLimitDisabled bypasses the per-connection token bucket. It exists
// only for isolated capacity tests; production callers should keep the default
// limiter enabled.
func WithWSRateLimitDisabled() Option {
	return func(gateway *Gateway) {
		gateway.disableWSRateLimit = true
	}
}

// WithAPIDocs mounts an already environment-gated, offline documentation handler.
func WithAPIDocs(handler http.Handler) Option {
	return func(gateway *Gateway) {
		gateway.apiDocs = handler
	}
}

// New constructs the transport gateway from its application boundaries.
func New(auth Authenticator, sessions store.SessionStore, options ...Option) *Gateway {
	gateway := &Gateway{
		auth: auth, sessions: sessions,
		now:          func() int64 { return time.Now().UnixMilli() },
		timeProfiles: gameconfig.NewTimeProfileSwitch(gameconfig.TimeProfileDemo),
	}
	for _, option := range options {
		if option != nil {
			option(gateway)
		}
	}
	// Random non-sequential start so a restarted Gateway with the same
	// gatewayID cannot reuse conn_id=1 against leftover connreg leases.
	gateway.nextConnID.Store(connectionIDSeed())
	return gateway
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
	if g.allowDebug {
		mux.HandleFunc("/api/debug/advance", g.debugAdvance)
	}
	if g.apiDocs != nil {
		mux.Handle("/docs/", g.apiDocs)
		mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
		})
	}
	return mux
}

func (g *Gateway) executeFarmRPC(ctx context.Context, routeUID uint64, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	if g.farmRPC == nil || g.routes == nil {
		return nil, errors.New("gateway: farm RPC is not configured")
	}
	farmID, err := g.routes.FarmID(routeUID)
	if err != nil {
		return nil, err
	}
	if request == nil || request.Envelope == nil {
		return nil, errors.New("gateway: nil Farm command")
	}
	request.RouteUid = routeUID
	response, err := g.farmRPC.Execute(ctx, farmID, request)
	if err != nil {
		return nil, err
	}
	if !validDownstreamResponse(request, response) {
		return nil, errors.New("gateway: malformed Farm response")
	}
	return response, nil
}

func (g *Gateway) executeSocialRPC(ctx context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	if g.socialRPC == nil || request == nil || request.Envelope == nil {
		return nil, errors.New("gateway: Social RPC is not configured")
	}
	response, err := g.socialRPC.ExecuteClientCommand(ctx, request)
	if err != nil {
		return nil, err
	}
	if !validDownstreamResponse(request, response) {
		return nil, errors.New("gateway: malformed Social response")
	}
	return response, nil
}

func validDownstreamResponse(request *farmv1.ClientCommandRequest, response *farmv1.ClientCommandResponse) bool {
	if request == nil || request.Envelope == nil || response == nil || response.Envelope == nil ||
		response.Envelope.Cmd != request.Envelope.Cmd ||
		response.Envelope.ClientSeq != request.Envelope.ClientSeq {
		return false
	}
	hasPrepared := len(response.PreparedPayload) != 0 || response.PreparedField != 0
	if !hasPrepared {
		return response.Envelope.Payload != nil
	}
	return request.PreferPrepared && response.Envelope.Err == int32(errcode.OK) &&
		response.Envelope.Payload == nil && len(response.PreparedPayload) != 0 &&
		clientwire.IsPreparedResponseFieldForCommand(response.PreparedField, response.Envelope.Cmd)
}

func (g *Gateway) clientCommandRequest(connection *wsConnection, envelope *publicv3.WireEnvelope, routeUID uint64) *farmv1.ClientCommandRequest {
	request := &farmv1.ClientCommandRequest{
		Uid: connection.uid, RouteUid: routeUID, ActiveFarmUid: connection.currentRoom(), Envelope: envelope,
		PreferPrepared: true,
	}
	if connection.id != 0 && g.gatewayID != "" {
		request.Originator = &farmv1.ConnRef{ConnId: connection.id, GatewayId: g.gatewayID}
	}
	return request
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
			existing.kick(errcode.Kicked)
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

func (g *Gateway) kickReplacedConnections(ctx context.Context, uid uint64, refs []presence.ConnRef) {
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
				connection.kick(errcode.Kicked)
			}
			continue
		}
		if g.sessionKickPusher == nil {
			telemetry.L().Error("gateway session kick pusher is not configured",
				"component", "gateway",
				"op", "session_kick",
				"uid", uid,
				"gateway_id", ref.GatewayID,
				"conn_id", ref.ConnID,
			)
			continue
		}
		if err := g.sessionKickPusher.PushSessionKick(ctx, ref, uid, errcode.Kicked); err != nil {
			telemetry.L().Error("gateway session kick push failed",
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
	value, ok := g.connections.Load(connection.id)
	if !ok || value != connection {
		return false
	}
	if g.connRegistry == nil {
		return true
	}
	if g.gatewayID == "" {
		return false
	}
	if err := g.connRegistry.Register(ctx, connection.uid, connection.id, g.gatewayID); err != nil {
		if errors.Is(err, presence.ErrAlreadyConnected) {
			return false
		}
		telemetry.L().Error("gateway connreg renew lifecycle failed",
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
		telemetry.L().Error("gateway connreg renew room failed",
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

// validateAuthenticatedConnection keeps the hot request path local while
// periodically revalidating the session and distributed ownership in Redis.
// Concurrent request/Pong callbacks share one atomic deadline, so only one of
// them performs the refresh for a given interval.
func (g *Gateway) validateAuthenticatedConnection(ctx context.Context, connection *wsConnection) bool {
	if g == nil || connection == nil || !connection.authed || connection.uid == 0 || connection.id == 0 {
		return false
	}
	value, ok := g.connections.Load(connection.id)
	if !ok || value != connection {
		return false
	}
	if !connection.claimAuthValidation(time.Now()) {
		return true
	}
	sessionUID, err := g.sessions.Get(ctx, connection.token)
	if err != nil || sessionUID != connection.uid {
		return false
	}
	return g.renewConnectionLease(ctx, connection)
}

func (connection *wsConnection) scheduleNextAuthValidation(now time.Time) {
	if connection == nil {
		return
	}
	// Connections often arrive in a short burst after deployment or a load-test
	// ramp. Spread their first Redis validation across one full interval to avoid
	// a synchronized renewal spike. The latest refresh remains comfortably
	// inside presence.DefaultLeaseTTL (2 minutes).
	jitterSlots := uint64(wsAuthValidationInterval / time.Millisecond)
	var jitter time.Duration
	if jitterSlots > 0 {
		// Knuth's multiplicative hash spreads consecutive connection IDs.
		jitter = time.Duration((connection.id*2_654_435_761)%jitterSlots) * time.Millisecond
	}
	connection.nextAuthValidationAt.Store(now.Add(wsAuthValidationInterval + jitter).UnixNano())
}

func (connection *wsConnection) claimAuthValidation(now time.Time) bool {
	if connection == nil {
		return false
	}
	for {
		next := connection.nextAuthValidationAt.Load()
		if next > now.UnixNano() {
			return false
		}
		if connection.nextAuthValidationAt.CompareAndSwap(
			next,
			now.Add(wsAuthValidationInterval).UnixNano(),
		) {
			return true
		}
	}
}

type debugAdvanceRequest struct {
	MS int64 `json:"ms"`
}

func (g *Gateway) debugAdvance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, errcode.BadRequest, http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeHTTPError(w, errcode.BadRequest, http.StatusBadRequest)
		return
	}
	var req debugAdvanceRequest
	if err := json.Unmarshal(body, &req); err != nil || req.MS <= 0 {
		writeHTTPError(w, errcode.BadRequest, http.StatusBadRequest)
		return
	}
	g.offsetMs.Add(req.MS)
	if g.debugFanout != nil {
		if err := g.debugFanout.Advance(r.Context(), req.MS); err != nil {
			writeHTTPError(w, errcode.Internal, http.StatusBadGateway)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int64{"server_time": g.Now()})
}

func (g *Gateway) switchTimeProfile(ctx context.Context, profile string) error {
	if !gameconfig.ValidTimeProfile(profile) {
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
	if g.debugFanout == nil {
		return nil
	}
	return g.debugFanout.SetTimeProfile(ctx, profile)
}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResponse struct {
	UID   clientjson.UID `json:"uid"`
	Token string         `json:"token"`
	WSURL string         `json:"ws_url"`
}

type errorResponse struct {
	Err errcode.Code `json:"err"`
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
		writeHTTPError(w, errcode.BadRequest, http.StatusMethodNotAllowed)
		return
	}

	var request authRequest
	if err := decodeJSON(r.Body, &request); err != nil || strings.TrimSpace(request.Username) == "" || request.Password == "" {
		writeHTTPError(w, errcode.BadRequest, http.StatusBadRequest)
		return
	}

	uid, token, err := authenticate(r.Context(), request.Username, request.Password)
	if err != nil {
		code := errorCode(err)
		status := http.StatusBadRequest
		if code == errcode.Internal {
			status = http.StatusInternalServerError
		}
		writeHTTPError(w, code, status)
		return
	}

	writeJSON(w, http.StatusOK, authResponse{
		UID:   clientjson.UID(uid),
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

func errorCode(err error) errcode.Code {
	var code errcode.Code
	if errors.As(err, &code) {
		return code
	}
	return errcode.Internal
}

func writeHTTPError(w http.ResponseWriter, code errcode.Code, status int) {
	writeJSON(w, status, errorResponse{Err: code})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
