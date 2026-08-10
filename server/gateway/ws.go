package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/domain/farm"
	"farm/server/gateway/presence"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/clientjson"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

var wsWriteBufferPool sync.Pool

var wsUpgrader = websocket.Upgrader{
	Subprotocols: []string{BinarySubprotocol},
	// A farm snapshot fits in this buffer in the common case. The pool lets
	// short-lived WebSocket connections reuse it instead of allocating one
	// write buffer per connection for its entire lifetime.
	WriteBufferSize: 8 << 10,
	WriteBufferPool: &wsWriteBufferPool,
	// Development accepts any origin; production must restrict this to trusted origins.
	CheckOrigin: func(*http.Request) bool { return true },
}

const maxPooledResponseEnvelope = maxWSMessageSize

var responseEnvelopePool = sync.Pool{New: func() any {
	buffer := make([]byte, 0, 4<<10)
	return &buffer
}}

const (
	maxWSMessageSize = 64 << 10
	wsReadTimeout    = 90 * time.Second
	// wsHeartbeatInterval 必须明显小于读超时。使用 WebSocket 控制帧而非
	// 前端定时器，可避免后台标签页节流导致空闲连接被错误断开。
	wsHeartbeatInterval = 30 * time.Second
	// wsWriteTimeout 防止慢客户端的 TCP 接收窗口被写阻塞：WriteMessage 持有 writeMu，
	// 一旦卡住会连带阻塞房间广播循环与上游 Actor 的串行区。
	wsWriteTimeout = 10 * time.Second
	// Redis 中的 session 与连接/房间租约不需要随每条业务命令刷新。
	// 30 秒一次既明显短于 2 分钟租约，也与服务端 WebSocket 心跳一致。
	wsAuthValidationInterval = 30 * time.Second
)

// Auth/session validation and distributed lease renewal are rate-limited per
// connection. Local ownership is still checked for every request, while Redis
// is touched at most once per wsAuthValidationInterval.

type wsConnection struct {
	conn                 *websocket.Conn
	writer               wsWireWriter // optional test seam; nil means use conn
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
	roomMu               sync.Mutex
	roomUID              uint64
	roomSeq              uint64
	roomSeqKnown         bool
	roomSeqObservedAt    int64
	// holdFarmDeltas keeps a newly-entered client from observing a delta before
	// its EnterFarm snapshot has reached the wire.
	holdFarmDeltas    bool
	heldFarmDeltas    []farm.FarmDelta
	pushMu            sync.Mutex
	pushStarted       bool
	pushClosed        bool
	pushCh            chan []byte
	responseCh        chan wsWriteRequest
	pushStop          chan struct{}
	pushDone          chan struct{}
	pushCoalesce      time.Duration // 0 => pushCoalesceWindow; tests may shorten
	taskNotifyMu      sync.Mutex
	taskNotifyReady   bool
	taskNotifyClosed  bool
	taskNotifyStarted bool
	taskNotifyMailbox map[taskNotifyMailboxKey]store.Task
	taskNotifyWake    chan struct{}
	taskNotifyStop    chan struct{}
	taskNotifyDone    chan struct{}
	mailNotifyMu      sync.Mutex
	mailNotifyReady   bool
	mailNotifyClosed  bool
	mailNotifyStarted bool
	mailNotifyPending string
	mailNotifyWake    chan struct{}
	mailNotifyStop    chan struct{}
	mailNotifyDone    chan struct{}
	heartbeatMu       sync.Mutex
	heartbeatClosed   bool
	heartbeatStarted  bool
	heartbeatStop     chan struct{}
	heartbeatDone     chan struct{}
}

type taskNotifyMailboxKey struct {
	dayKey int64
	taskID uint32
}

type handshakeRequest struct {
	Token           string            `json:"token"`
	ResumeFarmUID   clientjson.UID    `json:"resume_farm_uid"`
	ResumeFarmSeq   clientjson.Uint64 `json:"resume_farm_seq"`
	ClientConfigVer uint32            `json:"client_config_ver"`
}

type handshakeResponse struct {
	UID clientjson.UID `json:"uid"`
}

type pingRequest struct {
	ClientTime int64 `json:"client_time"`
}

type pongResponse struct {
	ClientTime int64 `json:"client_time"`
	ServerTime int64 `json:"server_time"`
}

type enterFarmRequest struct {
	OwnerUID clientjson.UID `json:"owner_uid"`
}

type enterFarmResponse struct {
	Snapshot           any               `json:"snapshot"`
	FarmSeq            clientjson.Uint64 `json:"farm_seq"`
	ServerTime         int64             `json:"server_time"`
	TimeProfile        string            `json:"time_profile"`
	TimeProfileMutable bool              `json:"time_profile_mutable"`
	Relation           string            `json:"relation"`
}

func (g *Gateway) serveWS(w http.ResponseWriter, r *http.Request) {
	if !supportsBinarySubprotocol(r) {
		writeHTTPError(w, errcode.BadRequest, http.StatusUpgradeRequired)
		return
	}
	if g.sessions == nil || (g.runtime == nil && g.farmRPC == nil) {
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
		conn:    conn,
		id:      allocateConnID(&g.nextConnID),
		limiter: newConnectionLimiter(),
		metrics: g.metrics,
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
		connection.closeTaskNotify()
		connection.closeMailNotify()
		if connection.authed {
			g.unregisterConnection(context.Background(), &connection)
		}
	}()
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			connection.setDisconnectReason(classifyWSReadError(err))
			return
		}
		if messageType != websocket.BinaryMessage {
			_ = connection.respond(Envelope{Err: errcode.BadRequest, Payload: emptyPayload})
			continue
		}
		requests, err := clientwire.DecodeBinaryBatch(data)
		if err != nil {
			_ = connection.respond(Envelope{Err: errcode.BadRequest, Payload: emptyPayload})
			continue
		}
		responses := make([]Envelope, 0, len(requests))
		releaseEnter := false
		enableAfterHandshake := false
		disconnectAfterWrite := false
		for _, request := range requests {
			if request.ServerPayload {
				responses = append(responses, Envelope{Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: errcode.BadRequest, Payload: emptyPayload})
				continue
			}
			if !g.disableWSRateLimit && !connection.limiter.Allow() {
				responses = append(responses, Envelope{Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: errcode.RateLimited, Payload: emptyPayload})
				if connection.limiter.ShouldDisconnect() {
					connection.setDisconnectReason("rate_limit")
					disconnectAfterWrite = true
				}
				continue
			}
			if connection.authed && !g.validateAuthenticatedConnection(context.Background(), &connection) {
				responses = append(responses, Envelope{Cmd: request.Cmd, ClientSeq: request.ClientSeq, Err: errcode.Kicked, Payload: emptyPayload})
				disconnectAfterWrite = true
				connection.setDisconnectReason("session_replaced")
				if connection.metrics != nil {
					connection.metrics.ObserveWSSessionReplacement()
				}
				break
			}

			handledAt := time.Now()
			var wireFields gatewayPayloadFields
			var response Envelope
			if connection.authed && request.Cmd == CommandEnterFarm {
				response, wireFields = g.handleEnterFarmForWire(&connection, request)
			} else if connection.authed && request.Cmd == CommandSyncFarm {
				response, wireFields = g.handleSyncFarmForWireAt(&connection, request, handledAt)
			} else {
				response = g.handleWSRequest(&connection, request)
			}
			if g.metrics != nil {
				code := uint32(response.Err)
				if response.Cmd == 0 {
					code = 0
				}
				g.metrics.ObserveWSRequest(request.Cmd, code, time.Since(handledAt))
				if request.Cmd == CommandHandshake && response.Err != errcode.OK {
					g.metrics.ObserveWSHandshakeError(code)
				}
			}
			if response.Cmd == 0 {
				continue
			}
			if wireFields.enabled {
				if len(response.PreparedPayload) > 0 {
					var suffix []byte
					var appendErr error
					switch response.PreparedField {
					case clientwire.PreparedEnterFarmResponse:
						suffix, appendErr = clientwire.MarshalEnterFarmGatewaySuffix(wireFields.mutable, wireFields.relation)
					case clientwire.PreparedSyncFarmResponse:
						suffix = clientwire.MarshalSyncFarmGatewaySuffix(wireFields.mutable)
					default:
						appendErr = errors.New("gateway: invalid prepared Farm payload")
					}
					if appendErr != nil {
						connection.setDisconnectReason("encode_error")
						return
					}
					response.PreparedSuffix = suffix
				} else {
					payload, appendErr := appendTrustedGatewayPayloadFields(response.Payload, wireFields.mutable, wireFields.relation)
					if appendErr != nil {
						connection.setDisconnectReason("encode_error")
						return
					}
					response.Payload = payload
				}
			}
			responses = append(responses, response)
			releaseEnter = releaseEnter || request.Cmd == CommandEnterFarm && response.Err == errcode.OK
			enableAfterHandshake = enableAfterHandshake || request.Cmd == CommandHandshake && response.Err == errcode.OK
		}
		if len(responses) > 0 {
			if err := connection.respondBatch(responses); err != nil {
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
			connection.enableTaskNotify(g)
			connection.enableMailNotify(g)
		}
		if disconnectAfterWrite {
			return
		}
	}
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
	reason := connection.disconnectReason
	connection.disconnectMu.Unlock()
	if reason == "" {
		return "unknown"
	}
	return reason
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

var emptyPayload = json.RawMessage(`{}`)

func supportsBinarySubprotocol(r *http.Request) bool {
	for _, subprotocol := range websocket.Subprotocols(r) {
		if subprotocol == BinarySubprotocol {
			return true
		}
	}
	return false
}

func (g *Gateway) handleWSRequest(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}

	if !connection.authed {
		if request.Cmd != CommandHandshake {
			response.Err = errcode.Unauthorized
			return response
		}
		var payload handshakeRequest
		if request.CommandRequest != nil {
			payload = handshakeRequest{
				Token:           request.CommandRequest.AuthToken,
				ResumeFarmUID:   clientjson.UID(request.CommandRequest.ResumeFarmUid),
				ResumeFarmSeq:   clientjson.Uint64(request.CommandRequest.ResumeFarmSeq),
				ClientConfigVer: request.CommandRequest.ClientConfigVer,
			}
		} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		if payload.Token == "" {
			response.Err = errcode.BadRequest
			return response
		}
		if payload.ClientConfigVer != gameconfig.ConfigVer {
			response.Err = errcode.ConfigStale
			return response
		}
		uid, err := g.sessions.Get(context.Background(), payload.Token)
		if err != nil {
			response.Err = sessionErrorCode(err)
			return response
		}
		if uid == 0 {
			response.Err = errcode.Unauthorized
			return response
		}
		connection.uid = uid
		connection.token = payload.Token
		connection.authed = true
		if err := g.registerConnection(context.Background(), connection); err != nil {
			connection.authed = false
			connection.uid = 0
			connection.token = ""
			if errors.Is(err, presence.ErrAlreadyConnected) {
				response.Err = errcode.Kicked
			} else {
				response.Err = errcode.Internal
			}
			return response
		}
		connection.scheduleNextAuthValidation(time.Now())
		response.Payload = marshalPayload(handshakeResponse{UID: clientjson.UID(uid)})
		response.CommandResponse = &publicv3.CommandResponse{Uid: uid}
		return response
	}

	switch request.Cmd {
	case CommandPing:
		var payload pingRequest
		if request.CommandRequest != nil {
			payload.ClientTime = request.CommandRequest.ClientTime
		} else if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = errcode.BadRequest
			return response
		}
		pong := pongResponse{
			ClientTime: payload.ClientTime,
			ServerTime: g.Now(),
		}
		response.Payload = marshalPayload(pong)
		response.CommandResponse = &publicv3.CommandResponse{ClientTime: pong.ClientTime, ServerTime: pong.ServerTime}
	case CommandEnterFarm:
		return g.handleEnterFarm(connection, request)
	case CommandLeaveFarm:
		if request.CommandRequest == nil {
			if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
				response.Err = errcode.BadRequest
				return response
			}
		}
		response.CommandResponse = &publicv3.CommandResponse{}
		g.leaveFarm(connection)
	case CommandSyncFarm:
		return g.handleSyncFarm(connection, request)
	case CommandTill, CommandClear, CommandPlant, CommandWater,
		CommandRemoveWeed, CommandRemovePest, CommandFertilize, CommandHarvest,
		CommandSteal, CommandBuy, CommandSell:
		return g.handlePlotOrShop(connection, request)
	case CommandPetStatus, CommandPetActivate, CommandPetFeed:
		return g.handlePet(connection, request)
	case CommandFriendList, CommandGenShareLink, CommandAcceptInvite,
		CommandRemoveFriend, CommandAddFriendByUID, CommandSearchUser,
		CommandRequestFriend, CommandListFriendRequests,
		CommandAcceptFriendRequest, CommandRejectFriendRequest:
		return g.handleFriendRequest(connection, request)
	case CommandTaskList, CommandTaskClaim, CommandMailList, CommandMailRead,
		CommandMailClaim, CommandMailDelete, CommandCodexList, CommandClaimDailyLogin:
		return g.handleTaskMailRequest(connection, request)
	case CommandSetTimeProfile:
		return g.handleSetTimeProfile(connection, request)
	default:
		response.Err = errcode.BadRequest
	}
	return response
}

func (connection *wsConnection) respond(envelope Envelope) error {
	return connection.respondBatch([]Envelope{envelope})
}

func (connection *wsConnection) respondBatch(envelopes []Envelope) error {
	for index := range envelopes {
		if err := clientwire.PrepareCommandResponse(&envelopes[index]); err != nil {
			return err
		}
	}
	pooled := responseEnvelopePool.Get().(*[]byte)
	buffer := (*pooled)[:0]
	data, err := clientwire.AppendBinaryBatch(buffer, envelopes)
	if err != nil {
		releaseResponseEnvelope(pooled, buffer)
		return err
	}
	err = connection.writeResponse(data)
	releaseResponseEnvelope(pooled, data)
	return err
}

func releaseResponseEnvelope(pooled *[]byte, buffer []byte) {
	if pooled == nil || cap(buffer) > maxPooledResponseEnvelope {
		return
	}
	*pooled = buffer[:0]
	responseEnvelopePool.Put(pooled)
}

func (connection *wsConnection) kick(reason errcode.Code) {
	if connection == nil || connection.conn == nil {
		return
	}
	connection.kickOnce.Do(func() {
		connection.setDisconnectReason("session_replaced")
		if connection.metrics != nil {
			connection.metrics.ObserveWSSessionReplacement()
		}
		payload := marshalPayload(struct {
			Reason errcode.Code `json:"reason"`
		}{Reason: reason})
		_ = connection.respond(Envelope{
			Cmd:         CommandSessionKick,
			ClientSeq:   0,
			Err:         errcode.OK,
			Payload:     payload,
			SessionKick: &publicv3.SessionKick{Reason: int32(reason)},
		})
		connection.markPushClosed()
		_ = connection.conn.Close()
	})
}

// respondEnterFarm writes the snapshot response before releasing deltas that
// arrived after entering the room. Held deltas are enqueued (not folded) after
// the response so FarmSeq order stays contiguous and never precedes the snapshot.
func (connection *wsConnection) respondEnterFarm(envelope Envelope) error {
	return connection.respondEnterFarmWithFields(envelope, gatewayPayloadFields{})
}

func (connection *wsConnection) respondEnterFarmWithFields(envelope Envelope, fields gatewayPayloadFields) error {
	var err error
	if fields.enabled {
		err = connection.respondWithGatewayFields(envelope, fields)
	} else {
		err = connection.respond(envelope)
	}
	if err != nil {
		return err
	}

	return connection.releaseHeldFarmDeltas()
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
			connection.roomMu.Lock()
			if connection.roomUID == delta.OwnerUID {
				connection.observeRoomDeltaLocked(delta.FarmSeq)
			}
			connection.roomMu.Unlock()
			data, encodeErr := clientwire.EncodeFarmDeltaRecord(delta)
			if encodeErr != nil {
				connection.roomMu.Lock()
				connection.holdFarmDeltas = false
				connection.heldFarmDeltas = nil
				connection.roomMu.Unlock()
				return encodeErr
			}
			if enqueueErr := connection.enqueuePush(data); enqueueErr != nil {
				connection.roomMu.Lock()
				connection.holdFarmDeltas = false
				connection.heldFarmDeltas = nil
				connection.roomMu.Unlock()
				connection.dropSlowConnection()
				return enqueueErr
			}
		}
	}
}

func (connection *wsConnection) respondWithGatewayFields(envelope Envelope, fields gatewayPayloadFields) error {
	payload, err := appendTrustedGatewayPayloadFields(envelope.Payload, fields.mutable, fields.relation)
	if err != nil {
		return err
	}
	envelope.Payload = payload
	return connection.respond(envelope)
}

// respondLocked remains the direct-write fallback for unit seams and callers
// created without startPushWriter. Production responses use responseCh so the
// connection has exactly one WebSocket data writer.
func (connection *wsConnection) respondLocked(envelope Envelope) error {
	if err := clientwire.PrepareCommandResponse(&envelope); err != nil {
		return err
	}
	pooled := responseEnvelopePool.Get().(*[]byte)
	buffer := (*pooled)[:0]
	data, err := clientwire.AppendBinaryBatch(buffer, []Envelope{envelope})
	if err != nil {
		releaseResponseEnvelope(pooled, buffer)
		return remapWireenvError(err)
	}
	err = connection.writeEncodedLocked(data)
	releaseResponseEnvelope(pooled, data)
	return err
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

// pushFarmDelta delivers a room delta. encoded is a once-encoded binary record;
// active connections share it in their queues. Connections in hold cache the
// structured delta and encode only after the EnterFarm response is flushed.
func (connection *wsConnection) pushFarmDelta(ownerUID uint64, delta farm.FarmDelta, encoded []byte) error {
	connection.roomMu.Lock()
	receiving := connection.roomUID == ownerUID
	holding := connection.holdFarmDeltas
	if receiving && holding {
		connection.heldFarmDeltas = append(connection.heldFarmDeltas, copyFarmDelta(delta))
	} else if receiving {
		connection.observeRoomDeltaLocked(delta.FarmSeq)
	}
	connection.roomMu.Unlock()
	if !receiving || holding {
		return nil
	}
	if len(encoded) == 0 {
		var err error
		encoded, err = clientwire.EncodeFarmDeltaRecord(delta)
		if err != nil {
			return err
		}
	}
	return connection.enqueuePush(encoded)
}

// pushPlayerDelta delivers state owned by this connection's authenticated
// player. Unlike FarmDelta, it is independent of the farm room being viewed.
func (connection *wsConnection) pushPlayerDelta(delta farm.PlayerDelta) error {
	return connection.pushPlayerDeltaProto(clientwire.PlayerDeltaToProto(delta))
}

func (connection *wsConnection) pushPlayerDeltaProto(delta *publicv3.PlayerDelta) error {
	if delta == nil {
		return errors.New("gateway: nil PlayerDelta")
	}
	data, err := clientwire.EncodeTrustedBinaryRecord(Envelope{
		Cmd:         CommandPlayerDelta,
		ClientSeq:   0,
		PlayerDelta: delta,
	})
	if err != nil {
		return err
	}
	return connection.enqueuePush(data)
}

func (connection *wsConnection) pushMailNotify(kind string) error {
	if connection == nil {
		return errors.New("gateway: nil MailNotify connection")
	}
	data, err := clientwire.EncodeTrustedBinaryRecord(Envelope{
		Cmd:        CommandMailNotify,
		ClientSeq:  0,
		MailNotify: &publicv3.MailNotify{Kind: kind},
	})
	if err != nil {
		return err
	}
	return connection.enqueuePush(data)
}

// enqueueMailNotify 只保留最新通知种类。MailNotify 不承载邮件内容，客户端收到
// 任意一条后都会重拉列表，因此有界单槽既隔离慢连接，也不会丢失状态真相。
func (connection *wsConnection) enqueueMailNotify(kind string) bool {
	if connection == nil || kind == "" {
		return false
	}
	connection.mailNotifyMu.Lock()
	defer connection.mailNotifyMu.Unlock()
	if connection.mailNotifyClosed {
		return false
	}
	connection.mailNotifyPending = kind
	if connection.mailNotifyReady {
		connection.signalMailNotifyLocked()
	}
	return true
}

// enableMailNotify 在 Handshake 响应写入后启动 dispatcher，保证认证响应先于并发通知。
func (connection *wsConnection) enableMailNotify(gateway *Gateway) {
	if connection == nil {
		return
	}
	connection.mailNotifyMu.Lock()
	if connection.mailNotifyClosed || connection.mailNotifyStarted {
		connection.mailNotifyMu.Unlock()
		return
	}
	connection.mailNotifyReady = true
	connection.mailNotifyStarted = true
	connection.mailNotifyWake = make(chan struct{}, 1)
	connection.mailNotifyStop = make(chan struct{})
	connection.mailNotifyDone = make(chan struct{})
	if connection.mailNotifyPending != "" {
		connection.signalMailNotifyLocked()
	}
	connection.mailNotifyMu.Unlock()
	go connection.runMailNotifyMailbox(gateway)
}

func (connection *wsConnection) closeMailNotify() {
	if connection == nil {
		return
	}
	connection.mailNotifyMu.Lock()
	if connection.mailNotifyClosed {
		connection.mailNotifyMu.Unlock()
		return
	}
	connection.mailNotifyClosed = true
	if connection.mailNotifyStarted {
		close(connection.mailNotifyStop)
	}
	connection.mailNotifyPending = ""
	connection.mailNotifyMu.Unlock()
}

func (connection *wsConnection) runMailNotifyMailbox(gateway *Gateway) {
	defer close(connection.mailNotifyDone)
	for {
		connection.mailNotifyMu.Lock()
		if connection.mailNotifyClosed {
			connection.mailNotifyMu.Unlock()
			return
		}
		kind := connection.mailNotifyPending
		if kind == "" {
			wake := connection.mailNotifyWake
			stop := connection.mailNotifyStop
			connection.mailNotifyMu.Unlock()
			select {
			case <-wake:
			case <-stop:
				return
			}
			continue
		}
		connection.mailNotifyPending = ""
		connection.mailNotifyMu.Unlock()

		deliver := gateway.mailNotifyDelivery
		if deliver == nil {
			deliver = func(connection *wsConnection, kind string) error {
				return connection.pushMailNotify(kind)
			}
		}
		if err := deliver(connection, kind); err != nil {
			telemetry.L().Debug("gateway MailNotify delivery failed",
				"component", "gateway",
				"op", "deliver_mail_notify",
				"uid", connection.uid,
				"conn_id", connection.id,
				"kind", kind,
				"err", err.Error(),
			)
			if errors.Is(err, errPushQueueFull) || errors.Is(err, errPushClosed) {
				connection.dropSlowConnection()
			}
		}
	}
}

func (connection *wsConnection) signalMailNotifyLocked() {
	select {
	case connection.mailNotifyWake <- struct{}{}:
	default:
	}
}

func (connection *wsConnection) pushTaskNotify(task store.Task) error {
	if connection == nil {
		return errors.New("gateway: nil TaskNotify connection")
	}
	data, err := clientwire.EncodeTrustedBinaryRecord(Envelope{
		Cmd:        CommandTaskNotify,
		ClientSeq:  0,
		TaskNotify: clientwire.TaskToProto(task),
	})
	if err != nil {
		return err
	}
	return connection.enqueuePush(data)
}

// enqueueTaskNotify merges the newest state for one server-defined daily task
// ID. A mailbox is intentionally retained before ready so a push racing
// a successful Handshake is emitted only after its response reaches the wire.
func (connection *wsConnection) enqueueTaskNotify(task store.Task) bool {
	if connection == nil || !isTaskNotifyID(task.ID) {
		return false
	}
	connection.taskNotifyMu.Lock()
	defer connection.taskNotifyMu.Unlock()
	if connection.taskNotifyClosed {
		return false
	}
	if connection.taskNotifyMailbox == nil {
		connection.taskNotifyMailbox = make(map[taskNotifyMailboxKey]store.Task, store.RandomDailyTaskCount+1)
	}
	connection.taskNotifyMailbox[taskNotifyMailboxKey{dayKey: task.DayKey, taskID: task.ID}] = task
	if connection.taskNotifyReady {
		connection.signalTaskNotifyLocked()
	}
	return true
}

// enableTaskNotify starts the one bounded mailbox dispatcher only after the
// Handshake response has been written successfully.
func (connection *wsConnection) enableTaskNotify(gateway *Gateway) {
	if connection == nil {
		return
	}
	connection.taskNotifyMu.Lock()
	if connection.taskNotifyClosed || connection.taskNotifyStarted {
		connection.taskNotifyMu.Unlock()
		return
	}
	connection.taskNotifyReady = true
	connection.taskNotifyStarted = true
	connection.taskNotifyWake = make(chan struct{}, 1)
	connection.taskNotifyStop = make(chan struct{})
	connection.taskNotifyDone = make(chan struct{})
	connection.taskNotifyMu.Unlock()
	go connection.runTaskNotifyMailbox(gateway)
}

func (connection *wsConnection) closeTaskNotify() {
	if connection == nil {
		return
	}
	connection.taskNotifyMu.Lock()
	if connection.taskNotifyClosed {
		connection.taskNotifyMu.Unlock()
		return
	}
	connection.taskNotifyClosed = true
	if connection.taskNotifyStarted {
		close(connection.taskNotifyStop)
	}
	connection.taskNotifyMailbox = nil
	connection.taskNotifyMu.Unlock()
}

func (connection *wsConnection) runTaskNotifyMailbox(gateway *Gateway) {
	defer close(connection.taskNotifyDone)
	for {
		connection.taskNotifyMu.Lock()
		if connection.taskNotifyClosed {
			connection.taskNotifyMu.Unlock()
			return
		}
		task, ok := connection.takeTaskNotifyLocked()
		if !ok {
			wake := connection.taskNotifyWake
			stop := connection.taskNotifyStop
			connection.taskNotifyMu.Unlock()
			select {
			case <-wake:
			case <-stop:
				return
			}
			continue
		}
		connection.taskNotifyMu.Unlock()

		deliver := gateway.taskNotifyDelivery
		if deliver == nil {
			deliver = func(connection *wsConnection, task store.Task) error {
				return connection.pushTaskNotify(task)
			}
		}
		if err := deliver(connection, task); err != nil {
			telemetry.L().Debug("gateway TaskNotify delivery failed",
				"component", "gateway",
				"op", "deliver_task_notify",
				"uid", connection.uid,
				"conn_id", connection.id,
				"task_id", task.ID,
				"err", err.Error(),
			)
			if errors.Is(err, errPushQueueFull) || errors.Is(err, errPushClosed) {
				connection.dropSlowConnection()
			}
		}
	}
}

func (connection *wsConnection) takeTaskNotifyLocked() (store.Task, bool) {
	var (
		selectedKey taskNotifyMailboxKey
		selected    bool
		task        store.Task
	)
	for key, candidate := range connection.taskNotifyMailbox {
		if !selected || key.dayKey < selectedKey.dayKey ||
			(key.dayKey == selectedKey.dayKey && key.taskID < selectedKey.taskID) {
			selectedKey = key
			selected = true
			task = candidate
		}
	}
	if !selected {
		return store.Task{}, false
	}
	delete(connection.taskNotifyMailbox, selectedKey)
	return task, true
}

func (connection *wsConnection) signalTaskNotifyLocked() {
	select {
	case connection.taskNotifyWake <- struct{}{}:
	default:
	}
}

func isTaskNotifyID(taskID uint32) bool {
	return store.IsDailyTaskID(taskID)
}

func (connection *wsConnection) subscribedTo(ownerUID uint64) bool {
	if connection == nil || ownerUID == 0 {
		return false
	}
	connection.roomMu.Lock()
	defer connection.roomMu.Unlock()
	return connection.roomUID == ownerUID
}

func unmarshalPayload(payload json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("gateway: payload has trailing JSON")
	}
	return nil
}

func marshalPayload(payload any) json.RawMessage {
	data, err := json.Marshal(payload)
	if err != nil {
		return emptyPayload
	}
	return data
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
