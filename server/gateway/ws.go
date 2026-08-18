package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"

	"github.com/gorilla/websocket"
)

var wsWriteBufferPool sync.Pool

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize: 4096, WriteBufferSize: 4096,
	WriteBufferPool: &wsWriteBufferPool,
	Subprotocols:    []string{BinarySubprotocol},
	CheckOrigin:     func(r *http.Request) bool { return r.Header.Get("Origin") == "" || sameOrigin(r) },
}

const maxPooledResponseEnvelope = maxWSMessageSize

var responseEnvelopePool = sync.Pool{New: func() any {
	buffer := make([]byte, 0, 4096)
	return &buffer
}}

var transportResponsePool = sync.Pool{New: func() any {
	return new(clientwire.WireResponse)
}}

const (
	maxWSMessageSize         = 64 << 10
	wsReadTimeout            = 90 * time.Second
	wsHeartbeatInterval      = 30 * time.Second
	wsWriteTimeout           = 10 * time.Second
	wsAuthValidationInterval = 30 * time.Second
	downstreamTimeout        = 6 * time.Second
)

// wsConnection contains transport state only. No business aggregate, friend
// cache or persistence controller is retained in Gateway.
type wsConnection struct {
	conn                 *websocket.Conn
	writer               wsWireWriter
	id                   uint64
	uid                  uint64
	token                string
	authed               bool
	nextAuthValidationAt atomic.Int64
	limiter              *connectionLimiter
	metrics              *telemetry.Metrics
	disconnectMu         sync.Mutex
	disconnectReason     string
	writeMu              sync.Mutex
	kickOnce             sync.Once

	roomMu            sync.Mutex
	roomUID           uint64
	roomSeq           uint64
	roomSeqKnown      bool
	roomSeqObservedAt int64
	holdFarmDeltas    bool
	heldFarmDeltas    []*publicv3.FarmDelta

	pushMu       sync.Mutex
	pushStarted  bool
	pushClosed   bool
	pushCh       chan []byte
	responseCh   chan wsWriteRequest
	pushStop     chan struct{}
	pushDone     chan struct{}
	pushCoalesce time.Duration

	heartbeatMu      sync.Mutex
	heartbeatClosed  bool
	heartbeatStarted bool
	heartbeatStop    chan struct{}
	heartbeatDone    chan struct{}
}

func (g *Gateway) serveWS(w http.ResponseWriter, r *http.Request) {
	if !supportsBinarySubprotocol(r) {
		writeHTTPError(w, errcode.BadRequest, http.StatusUpgradeRequired)
		return
	}
	if g.sessions == nil || g.farmRPC == nil || g.socialRPC == nil || g.routes == nil {
		writeHTTPError(w, errcode.Internal, http.StatusServiceUnavailable)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxWSMessageSize)
	if g.metrics != nil {
		g.metrics.WSConnections.Inc()
		defer g.metrics.WSConnections.Dec()
	}

	connection := wsConnection{
		conn: conn, id: allocateConnID(&g.nextConnID), limiter: newConnectionLimiter(), metrics: g.metrics,
	}
	defer func() {
		if g.metrics != nil {
			g.metrics.ObserveWSDisconnect(connection.finalDisconnectReason())
		}
	}()
	connection.startPushWriter()
	connection.installPongHandler(g, wsReadTimeout)
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		connection.setDisconnectReason("setup_error")
		return
	}
	defer func() {
		g.leaveFarm(&connection)
		connection.closePushWriter()
		connection.closeHeartbeat()
		if connection.authed {
			g.unregisterConnection(context.Background(), &connection)
		}
	}()

	for {
		messageType, data, readErr := conn.ReadMessage()
		if readErr != nil {
			connection.setDisconnectReason(classifyWSReadError(readErr))
			return
		}
		if messageType != websocket.BinaryMessage {
			_ = connection.respondWire(wireError(nil, errcode.BadRequest))
			continue
		}
		requests, decodeErr := clientwire.DecodeWireBatch(data)
		if decodeErr != nil {
			_ = connection.respondWire(wireError(nil, errcode.BadRequest))
			continue
		}

		responses := make([]*clientwire.WireResponse, 0, len(requests))
		postPushes := make([]*publicv3.WireEnvelope, 0, 4)
		releaseEnter := false
		enableAfterHandshake := false
		disconnectAfterWrite := false
		for _, request := range requests {
			handledAt := time.Now()
			response, pushes, disconnect := g.dispatchWireRequest(r.Context(), &connection, request)
			if response == nil {
				response = typedWireResponse(wireError(request, errcode.Internal))
			}
			if g.metrics != nil {
				g.metrics.ObserveWSRequest(request.GetCmd(), uint32(response.GetErr()), time.Since(handledAt))
				if request.GetCmd() == CommandHandshake && response.GetErr() != int32(errcode.OK) {
					g.metrics.ObserveWSHandshakeError(uint32(response.GetErr()))
				}
			}
			responses = append(responses, response)
			postPushes = append(postPushes, pushes...)
			releaseEnter = releaseEnter || request.GetCmd() == CommandEnterFarm && response.GetErr() == int32(errcode.OK)
			enableAfterHandshake = enableAfterHandshake || request.GetCmd() == CommandHandshake && response.GetErr() == int32(errcode.OK)
			if disconnect {
				disconnectAfterWrite = true
				break
			}
		}
		if len(responses) != 0 {
			if err := connection.respondWireBatch(responses); err != nil {
				connection.setDisconnectReason("response_write_error")
				return
			}
		}
		if releaseEnter {
			if err := connection.releaseHeldFarmDeltas(); err != nil {
				connection.setDisconnectReason("push_enqueue_error")
				return
			}
		}
		if enableAfterHandshake {
			connection.enableHeartbeat()
		}
		for _, push := range postPushes {
			if err := connection.enqueueWirePush(push); err != nil {
				connection.setDisconnectReason("push_enqueue_error")
				return
			}
		}
		if disconnectAfterWrite {
			return
		}
	}
}

func (g *Gateway) dispatchWireRequest(ctx context.Context, connection *wsConnection, request *publicv3.WireEnvelope) (*clientwire.WireResponse, []*publicv3.WireEnvelope, bool) {
	if !validClientEnvelope(request) {
		return typedWireResponse(wireError(request, errcode.BadRequest)), nil, false
	}
	if !g.disableWSRateLimit && !connection.limiter.Allow() {
		if g.metrics != nil {
			g.metrics.ObserveWSRateLimited("connection")
		}
		disconnect := connection.limiter.ShouldDisconnect()
		if disconnect {
			connection.setDisconnectReason("rate_limit")
		}
		return typedWireResponse(wireError(request, errcode.RateLimited)), nil, disconnect
	}
	if !connection.authed {
		if request.Cmd != CommandHandshake {
			return typedWireResponse(wireError(request, errcode.Unauthorized)), nil, false
		}
		return typedWireResponse(g.handleHandshake(connection, request)), nil, false
	}
	if !g.validateAuthenticatedConnection(context.Background(), connection) {
		connection.setDisconnectReason("session_replaced")
		if connection.metrics != nil {
			connection.metrics.ObserveWSSessionReplacement()
		}
		return typedWireResponse(wireError(request, errcode.Kicked)), nil, true
	}

	switch request.Cmd {
	case CommandHandshake:
		return typedWireResponse(wireError(request, errcode.BadRequest)), nil, false
	case CommandPing:
		payload := request.GetCommandRequest()
		return typedWireResponse(wireCommandResponse(request, errcode.OK, &publicv3.CommandResponse{
			ClientTime: payload.ClientTime, ServerTime: g.Now(),
		})), nil, false
	case CommandLeaveFarm:
		g.leaveFarm(connection)
		return typedWireResponse(wireCommandResponse(request, errcode.OK, &publicv3.CommandResponse{})), nil, false
	case CommandSetTimeProfile:
		return typedWireResponse(g.handleDebugWire(ctx, request)), nil, false
	}

	if isSocialCommand(request.Cmd) {
		callCtx, cancel := context.WithTimeout(ctx, downstreamTimeout)
		defer cancel()
		rpcResponse, err := g.executeSocialRPC(callCtx, g.clientCommandRequest(connection, request, connection.uid))
		if err != nil {
			return typedWireResponse(wireError(request, errcode.Internal)), nil, false
		}
		return downstreamWireResponse(rpcResponse), rpcResponse.Pushes, false
	}
	if !isFarmCommand(request.Cmd) {
		return typedWireResponse(wireError(request, errcode.BadRequest)), nil, false
	}

	routeUID, enterOwner, code := farmRoute(connection, request)
	if code != errcode.OK {
		return typedWireResponse(wireError(request, code)), nil, false
	}
	if request.Cmd == CommandSyncFarm {
		if local, ok := g.localCaughtUpSelfSync(connection, request, routeUID, time.Now()); ok {
			return downstreamWireResponse(local), nil, false
		}
	}
	preSubscribed := false
	if request.Cmd == CommandEnterFarm {
		if err := g.enterRoom(connection, enterOwner); err != nil {
			return typedWireResponse(wireError(request, errcode.Internal)), nil, false
		}
		preSubscribed = true
	}
	callCtx, cancel := context.WithTimeout(ctx, downstreamTimeout)
	defer cancel()
	rpcResponse, err := g.executeFarmRPC(callCtx, routeUID, g.clientCommandRequest(connection, request, routeUID))
	if err != nil {
		if preSubscribed {
			g.leaveFarm(connection)
		}
		return typedWireResponse(wireError(request, errcode.Internal)), nil, false
	}
	if preSubscribed && rpcResponse.Envelope.Err != int32(errcode.OK) {
		g.leaveFarm(connection)
	}
	g.applyRoomDirective(connection, rpcResponse)
	return downstreamWireResponse(rpcResponse), rpcResponse.Pushes, false
}

func typedWireResponse(envelope *publicv3.WireEnvelope) *clientwire.WireResponse {
	response := transportResponsePool.Get().(*clientwire.WireResponse)
	response.Envelope = envelope
	return response
}

func downstreamWireResponse(response *farmv1.ClientCommandResponse) *clientwire.WireResponse {
	if response == nil {
		return nil
	}
	transport := transportResponsePool.Get().(*clientwire.WireResponse)
	transport.Envelope = response.Envelope
	transport.PreparedPayload = response.PreparedPayload
	transport.PreparedField = response.PreparedField
	return transport
}

func (g *Gateway) handleHandshake(connection *wsConnection, request *publicv3.WireEnvelope) *publicv3.WireEnvelope {
	payload := request.GetCommandRequest()
	if payload == nil || payload.AuthToken == "" || payload.ClientConfigVer != gameconfig.ConfigVer {
		code := errcode.BadRequest
		if payload != nil && payload.AuthToken != "" {
			code = errcode.ConfigStale
		}
		return wireError(request, code)
	}
	uid, err := g.sessions.Get(context.Background(), payload.AuthToken)
	if err != nil {
		return wireError(request, sessionErrorCode(err))
	}
	if uid == 0 {
		return wireError(request, errcode.Unauthorized)
	}
	connection.uid, connection.token, connection.authed = uid, payload.AuthToken, true
	if err := g.registerConnection(context.Background(), connection); err != nil {
		connection.uid, connection.token, connection.authed = 0, "", false
		return wireError(request, errcode.Internal)
	}
	connection.scheduleNextAuthValidation(time.Now())
	return wireCommandResponse(request, errcode.OK, &publicv3.CommandResponse{Uid: uid})
}

func (g *Gateway) handleDebugWire(ctx context.Context, request *publicv3.WireEnvelope) *publicv3.WireEnvelope {
	if !g.allowDebug {
		return wireError(request, errcode.BadRequest)
	}
	payload := request.GetCommandRequest()
	if payload == nil || !gameconfig.ValidTimeProfile(payload.TimeProfile) {
		return wireError(request, errcode.BadRequest)
	}
	if err := g.switchTimeProfile(ctx, payload.TimeProfile); err != nil {
		return wireError(request, errcode.Internal)
	}
	return wireCommandResponse(request, errcode.OK, &publicv3.CommandResponse{
		TimeProfile: g.TimeProfile(), TimeProfileMutable: true,
	})
}

func validClientEnvelope(request *publicv3.WireEnvelope) bool {
	if request == nil || request.Err != 0 || request.Cmd == 0 || request.ClientSeq == 0 {
		return false
	}
	switch request.Payload.(type) {
	case *publicv3.WireEnvelope_EnterFarmRequest:
		return request.Cmd == CommandEnterFarm
	case *publicv3.WireEnvelope_SyncFarmRequest:
		return request.Cmd == CommandSyncFarm
	case *publicv3.WireEnvelope_CommandRequest:
		return request.Cmd != CommandEnterFarm && request.Cmd != CommandSyncFarm
	default:
		return false
	}
}

func isSocialCommand(command uint32) bool { return command >= 400 && command <= 418 && command%2 == 0 }

func isFarmCommand(command uint32) bool {
	return command == CommandEnterFarm || command == CommandSyncFarm ||
		command >= 206 && command <= 222 && command%2 == 0 ||
		command == CommandBuy || command == CommandSell ||
		command >= CommandPetStatus && command <= CommandPetFeed && command%2 == 0 ||
		command >= CommandTaskList && command <= CommandClaimDailyLogin && command%2 == 0
}

func farmRoute(connection *wsConnection, request *publicv3.WireEnvelope) (routeUID, enterOwner uint64, code errcode.Code) {
	switch request.Cmd {
	case CommandEnterFarm:
		payload := request.GetEnterFarmRequest()
		if payload == nil {
			return 0, 0, errcode.BadRequest
		}
		ownerUID := payload.OwnerUid
		if ownerUID == 0 {
			ownerUID = connection.uid
		}
		return ownerUID, ownerUID, errcode.OK
	case CommandSyncFarm:
		ownerUID := connection.currentRoom()
		if ownerUID == 0 {
			return 0, 0, errcode.BadRequest
		}
		return ownerUID, 0, errcode.OK
	default:
		return connection.uid, 0, errcode.OK
	}
}

// localCaughtUpSelfSync restores the transport-level fast path that was lost
// when Gateway business behavior moved into Farm. It never reads an aggregate,
// Redis or MySQL and is restricted to the authenticated player's own room. A
// short freshness lease forces periodic authoritative Farm Syncs, where lazy
// time advancement and delta recovery still happen.
func (g *Gateway) localCaughtUpSelfSync(
	connection *wsConnection,
	request *publicv3.WireEnvelope,
	ownerUID uint64,
	observedAt time.Time,
) (*farmv1.ClientCommandResponse, bool) {
	if g == nil || connection == nil || request == nil || ownerUID == 0 ||
		connection.uid != ownerUID {
		return nil, false
	}
	payload := request.GetSyncFarmRequest()
	if payload == nil || payload.OwnerUid != 0 && payload.OwnerUid != ownerUID {
		return nil, false
	}
	farmSeq, ok := connection.matchesFreshRoomWatermark(ownerUID, payload.FromSeq, observedAt)
	if !ok {
		return nil, false
	}
	prepared := clientwire.MarshalSyncFarmCaughtUpPayload(
		farmSeq, g.Now(), g.TimeProfile(), false,
	)
	return &farmv1.ClientCommandResponse{
		Envelope: &publicv3.WireEnvelope{
			Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: int32(errcode.OK),
		},
		RoomUid: ownerUID, RoomSeq: farmSeq,
		PreparedPayload: prepared, PreparedField: clientwire.PreparedSyncFarmResponse,
	}, true
}

func (g *Gateway) applyRoomDirective(connection *wsConnection, response *farmv1.ClientCommandResponse) {
	if response == nil {
		return
	}
	switch response.RoomAction {
	case farmv1.RoomAction_ROOM_ACTION_SUBSCRIBE:
		if response.RoomUid != 0 && connection.currentRoom() != response.RoomUid {
			if err := g.enterRoom(connection, response.RoomUid); err != nil {
				g.leaveFarm(connection)
				return
			}
		}
		connection.setRoomWatermark(response.RoomUid, response.RoomSeq)
	case farmv1.RoomAction_ROOM_ACTION_UNSUBSCRIBE:
		g.leaveFarm(connection)
	default:
		if response.RoomUid != 0 && response.RoomSeq != 0 {
			connection.setRoomWatermark(response.RoomUid, response.RoomSeq)
		}
	}
}

func wireError(request *publicv3.WireEnvelope, code errcode.Code) *publicv3.WireEnvelope {
	return wireCommandResponse(request, code, &publicv3.CommandResponse{})
}

func wireCommandResponse(request *publicv3.WireEnvelope, code errcode.Code, response *publicv3.CommandResponse) *publicv3.WireEnvelope {
	var command, sequence uint32
	if request != nil {
		command, sequence = request.Cmd, request.ClientSeq
	}
	if response == nil {
		response = &publicv3.CommandResponse{}
	}
	return &publicv3.WireEnvelope{Cmd: command, ClientSeq: sequence, Err: int32(code),
		Payload: &publicv3.WireEnvelope_CommandResponse{CommandResponse: response}}
}

func (connection *wsConnection) respondWire(envelope *publicv3.WireEnvelope) error {
	return connection.respondWireBatch([]*clientwire.WireResponse{typedWireResponse(envelope)})
}

func (connection *wsConnection) respondWireBatch(responses []*clientwire.WireResponse) error {
	defer releaseTransportResponses(responses)
	pooled := responseEnvelopePool.Get().(*[]byte)
	buffer := (*pooled)[:0]
	data, err := clientwire.AppendWireResponses(buffer, responses)
	if err != nil {
		releaseResponseEnvelope(pooled, buffer)
		return err
	}
	err = connection.writeResponse(data)
	releaseResponseEnvelope(pooled, data)
	return err
}

func releaseTransportResponses(responses []*clientwire.WireResponse) {
	for _, response := range responses {
		if response == nil {
			continue
		}
		response.Envelope = nil
		response.PreparedPayload = nil
		response.PreparedField = 0
		transportResponsePool.Put(response)
	}
}

func releaseResponseEnvelope(pooled *[]byte, buffer []byte) {
	if pooled == nil || cap(buffer) > maxPooledResponseEnvelope {
		return
	}
	*pooled = buffer[:0]
	responseEnvelopePool.Put(pooled)
}

func (connection *wsConnection) enqueueWirePush(envelope *publicv3.WireEnvelope) error {
	record, err := clientwire.EncodeWireRecord(envelope)
	if err != nil {
		return err
	}
	return connection.enqueuePush(record)
}

func (connection *wsConnection) setDisconnectReason(reason string) {
	if connection == nil || reason == "" {
		return
	}
	connection.disconnectMu.Lock()
	if connection.disconnectReason == "" {
		connection.disconnectReason = reason
	}
	connection.disconnectMu.Unlock()
}

func (connection *wsConnection) finalDisconnectReason() string {
	if connection == nil {
		return "unknown"
	}
	connection.disconnectMu.Lock()
	defer connection.disconnectMu.Unlock()
	if connection.disconnectReason == "" {
		return "unknown"
	}
	return connection.disconnectReason
}

func classifyWSReadError(err error) string {
	if err == nil {
		return "unknown"
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure:
			return "client_normal"
		case websocket.CloseGoingAway:
			return "client_going_away"
		case websocket.CloseNoStatusReceived:
			return "client_no_status"
		default:
			return "client_close_error"
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "read_timeout"
	}
	return "read_error"
}

func supportsBinarySubprotocol(r *http.Request) bool {
	for _, subprotocol := range websocket.Subprotocols(r) {
		if subprotocol == BinarySubprotocol {
			return true
		}
	}
	return false
}

func (connection *wsConnection) kick(reason errcode.Code) {
	if connection == nil || connection.wire() == nil {
		return
	}
	connection.kickOnce.Do(func() {
		connection.setDisconnectReason("session_replaced")
		if connection.metrics != nil {
			connection.metrics.ObserveWSSessionReplacement()
		}
		_ = connection.respondWire(&publicv3.WireEnvelope{
			Cmd:     CommandSessionKick,
			Payload: &publicv3.WireEnvelope_SessionKick{SessionKick: &publicv3.SessionKick{Reason: int32(reason)}},
		})
		connection.markPushClosed()
		_ = connection.wire().Close()
	})
}

func (connection *wsConnection) releaseHeldFarmDeltas() error {
	for {
		connection.roomMu.Lock()
		held := connection.heldFarmDeltas
		connection.heldFarmDeltas = nil
		if len(held) == 0 {
			connection.holdFarmDeltas = false
			connection.roomMu.Unlock()
			return nil
		}
		connection.roomMu.Unlock()
		for _, delta := range held {
			if delta == nil {
				continue
			}
			connection.roomMu.Lock()
			if connection.roomUID == delta.OwnerUid {
				connection.observeRoomDeltaLocked(delta.FarmSeq)
			}
			connection.roomMu.Unlock()
			if err := connection.enqueueWirePush(&publicv3.WireEnvelope{
				Cmd:     CommandFarmDelta,
				Payload: &publicv3.WireEnvelope_FarmDelta{FarmDelta: delta},
			}); err != nil {
				connection.roomMu.Lock()
				connection.holdFarmDeltas = false
				connection.heldFarmDeltas = nil
				connection.roomMu.Unlock()
				connection.dropSlowConnection()
				return err
			}
		}
	}
}

func (connection *wsConnection) writeEncodedLocked(data []byte) error {
	writer := connection.wire()
	if writer == nil {
		return errors.New("gateway: nil websocket writer")
	}
	if err := writer.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	return writer.WriteMessage(websocket.BinaryMessage, data)
}

func (connection *wsConnection) pushFarmDelta(ownerUID uint64, delta *publicv3.FarmDelta, encoded []byte) error {
	if delta == nil || delta.OwnerUid != ownerUID {
		return errors.New("gateway: invalid FarmDelta")
	}
	connection.roomMu.Lock()
	receiving, holding := connection.roomUID == ownerUID, connection.holdFarmDeltas
	if receiving && holding {
		connection.heldFarmDeltas = append(connection.heldFarmDeltas, delta)
	} else if receiving {
		connection.observeRoomDeltaLocked(delta.FarmSeq)
	}
	connection.roomMu.Unlock()
	if !receiving || holding {
		return nil
	}
	if len(encoded) == 0 {
		var err error
		encoded, err = clientwire.EncodeWireRecord(&publicv3.WireEnvelope{
			Cmd:     CommandFarmDelta,
			Payload: &publicv3.WireEnvelope_FarmDelta{FarmDelta: delta},
		})
		if err != nil {
			return err
		}
	}
	return connection.enqueuePush(encoded)
}

func (connection *wsConnection) subscribedTo(ownerUID uint64) bool {
	if connection == nil || ownerUID == 0 {
		return false
	}
	connection.roomMu.Lock()
	defer connection.roomMu.Unlock()
	return connection.roomUID == ownerUID
}

func sessionErrorCode(err error) errcode.Code {
	if errors.Is(err, store.ErrSessionReplaced) {
		return errcode.Kicked
	}
	if errors.Is(err, store.ErrSessionNotFound) {
		return errcode.Unauthorized
	}
	return errcode.Internal
}
