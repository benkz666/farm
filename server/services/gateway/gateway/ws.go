package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/platform/connreg"
	"farm/server/platform/farm"
	"farm/server/platform/gameconf"
	"farm/server/platform/obs"
	"farm/server/platform/pkgerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/store"
	"farm/server/platform/wireenv"
)

var wsUpgrader = websocket.Upgrader{
	Subprotocols: []string{JSONSubprotocol},
	// Development accepts any origin; production must restrict this to trusted origins.
	CheckOrigin: func(*http.Request) bool { return true },
}

const (
	maxWSMessageSize = 64 << 10
	wsReadTimeout    = 90 * time.Second
	// wsHeartbeatInterval 必须明显小于读超时。使用 WebSocket 控制帧而非
	// 前端定时器，可避免后台标签页节流导致空闲连接被错误断开。
	wsHeartbeatInterval = 30 * time.Second
	// wsWriteTimeout 防止慢客户端的 TCP 接收窗口被写阻塞：WriteMessage 持有 writeMu，
	// 一旦卡住会连带阻塞房间广播循环与上游 Actor 的串行区。
	wsWriteTimeout = 10 * time.Second
)

// Lease renewal is driven by this read loop (no per-connection goroutine):
// after each authenticated request, Gateway renews connreg lifecycle + room
// leases. Each renew refreshes the Redis key fallback TTL (2*leaseTTL); members
// still expire by score at 1*leaseTTL. Unrenewed keys self-delete afterward.

type wsConnection struct {
	conn     *websocket.Conn
	id       uint64
	uid      uint64
	token    string
	authed   bool
	limiter  *connectionLimiter
	writeMu  sync.Mutex
	kickOnce sync.Once
	roomMu   sync.Mutex
	roomUID  uint64
	// holdFarmDeltas keeps a newly-entered client from observing a delta before
	// its EnterFarm snapshot has reached the wire.
	holdFarmDeltas    bool
	heldFarmDeltas    []farm.FarmDelta
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
	Token           string         `json:"token"`
	ResumeFarmUID   pkgjson.UID    `json:"resume_farm_uid"`
	ResumeFarmSeq   pkgjson.Uint64 `json:"resume_farm_seq"`
	ClientConfigVer uint32         `json:"client_config_ver"`
}

type handshakeResponse struct {
	UID pkgjson.UID `json:"uid"`
}

type pingRequest struct {
	ClientTime int64 `json:"client_time"`
}

type pongResponse struct {
	ClientTime int64 `json:"client_time"`
	ServerTime int64 `json:"server_time"`
}

type enterFarmRequest struct {
	OwnerUID pkgjson.UID `json:"owner_uid"`
}

type enterFarmResponse struct {
	Snapshot           any            `json:"snapshot"`
	FarmSeq            pkgjson.Uint64 `json:"farm_seq"`
	ServerTime         int64          `json:"server_time"`
	TimeProfile        string         `json:"time_profile"`
	TimeProfileMutable bool           `json:"time_profile_mutable"`
	Relation           string         `json:"relation"`
}

func (g *Gateway) serveWS(w http.ResponseWriter, r *http.Request) {
	if !supportsJSONSubprotocol(r) {
		writeHTTPError(w, pkgerr.BadRequest, http.StatusUpgradeRequired)
		return
	}
	if g.sessions == nil || (g.runtime == nil && g.farmRPC == nil) {
		writeHTTPError(w, pkgerr.Internal, http.StatusServiceUnavailable)
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
	}
	connection.installPongHandler(g, wsReadTimeout)
	defer func() {
		g.leaveFarm(&connection)
		connection.closeHeartbeat()
		connection.closeTaskNotify()
		connection.closeMailNotify()
		if connection.authed {
			g.unregisterConnection(context.Background(), &connection)
		}
	}()
	for {
		if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
			return
		}
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			_ = connection.respond(Envelope{Err: pkgerr.BadRequest, Payload: emptyPayload})
			continue
		}
		request, err := DecodeEnvelope(data)
		if err != nil {
			_ = connection.respond(Envelope{Err: pkgerr.BadRequest, Payload: emptyPayload})
			continue
		}
		if !connection.limiter.Allow() {
			if err := connection.respond(Envelope{
				Cmd:       request.Cmd,
				ClientSeq: request.ClientSeq,
				Err:       pkgerr.RateLimited,
				Payload:   emptyPayload,
			}); err != nil {
				return
			}
			if connection.limiter.ShouldDisconnect() {
				return
			}
			continue
		}

		if connection.authed {
			sessionUID, sessionErr := g.sessions.Get(context.Background(), connection.token)
			if sessionErr != nil || sessionUID != connection.uid {
				code := sessionErrorCode(sessionErr)
				if sessionErr == nil {
					code = pkgerr.Kicked
				}
				_ = connection.respond(Envelope{
					Cmd:       request.Cmd,
					ClientSeq: request.ClientSeq,
					Err:       code,
					Payload:   emptyPayload,
				})
				return
			}
			if !g.renewConnectionLease(context.Background(), &connection) {
				_ = connection.respond(Envelope{
					Cmd:       request.Cmd,
					ClientSeq: request.ClientSeq,
					Err:       pkgerr.Kicked,
					Payload:   emptyPayload,
				})
				return
			}
		}

		handledAt := time.Now()
		response := g.handleWSRequest(&connection, request)
		if g.metrics != nil {
			code := uint32(response.Err)
			if response.Cmd == 0 {
				code = 0
			}
			g.metrics.ObserveWSRequest(request.Cmd, code, time.Since(handledAt))
		}
		if response.Cmd == 0 {
			// Cross-farm actions respond only after CrossResult settles the
			// visitor reservation; emitting here would acknowledge too early.
			continue
		}
		var respondErr error
		if request.Cmd == CommandEnterFarm && response.Err == pkgerr.OK {
			respondErr = connection.respondEnterFarm(response)
		} else {
			respondErr = connection.respond(response)
		}
		if respondErr != nil {
			return
		}
		if request.Cmd == CommandHandshake && response.Err == pkgerr.OK {
			connection.enableHeartbeat()
			connection.enableTaskNotify(g)
			connection.enableMailNotify(g)
		}
	}
}

var emptyPayload = json.RawMessage(`{}`)

func supportsJSONSubprotocol(r *http.Request) bool {
	for _, subprotocol := range websocket.Subprotocols(r) {
		if subprotocol == JSONSubprotocol {
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
			response.Err = pkgerr.Unauthorized
			return response
		}
		var payload handshakeRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.Token == "" {
			response.Err = pkgerr.BadRequest
			return response
		}
		if payload.ClientConfigVer != gameconf.ConfigVer {
			response.Err = pkgerr.ConfigStale
			return response
		}
		uid, err := g.sessions.Get(context.Background(), payload.Token)
		if err != nil {
			response.Err = sessionErrorCode(err)
			return response
		}
		if uid == 0 {
			response.Err = pkgerr.Unauthorized
			return response
		}
		connection.uid = uid
		connection.token = payload.Token
		connection.authed = true
		if err := g.registerConnection(context.Background(), connection); err != nil {
			connection.authed = false
			connection.uid = 0
			connection.token = ""
			if errors.Is(err, connreg.ErrAlreadyConnected) {
				response.Err = pkgerr.Kicked
			} else {
				response.Err = pkgerr.Internal
			}
			return response
		}
		response.Payload = marshalPayload(handshakeResponse{UID: pkgjson.UID(uid)})
		return response
	}

	switch request.Cmd {
	case CommandPing:
		var payload pingRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		response.Payload = marshalPayload(pongResponse{
			ClientTime: payload.ClientTime,
			ServerTime: g.Now(),
		})
	case CommandEnterFarm:
		return g.handleEnterFarm(connection, request)
	case CommandLeaveFarm:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
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
		response.Err = pkgerr.BadRequest
	}
	return response
}

func (connection *wsConnection) respond(envelope Envelope) error {
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	return connection.respondLocked(envelope)
}

func (connection *wsConnection) kick(reason pkgerr.Code) {
	if connection == nil || connection.conn == nil {
		return
	}
	connection.kickOnce.Do(func() {
		payload := marshalPayload(struct {
			Reason pkgerr.Code `json:"reason"`
		}{Reason: reason})
		_ = connection.respond(Envelope{
			Cmd:       CommandSessionKick,
			ClientSeq: 0,
			Err:       pkgerr.OK,
			Payload:   payload,
		})
		_ = connection.conn.Close()
	})
}

// respondEnterFarm writes the snapshot response before releasing deltas that
// arrived after entering the room. writeMu makes the response and its flush an
// indivisible wire-order operation with concurrent room broadcasts.
func (connection *wsConnection) respondEnterFarm(envelope Envelope) error {
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if err := connection.respondLocked(envelope); err != nil {
		return err
	}

	connection.roomMu.Lock()
	held := connection.heldFarmDeltas
	connection.heldFarmDeltas = nil
	connection.holdFarmDeltas = false
	connection.roomMu.Unlock()

	for _, delta := range held {
		// Hold release encodes per buffered delta; normal broadcasts reuse
		// pre-encoded bytes and must not marshal per connection.
		data, err := wireenv.EncodeFarmDelta(delta)
		if err != nil {
			return err
		}
		if err := connection.writeEncodedLocked(data); err != nil {
			return err
		}
	}
	return nil
}

func (connection *wsConnection) respondLocked(envelope Envelope) error {
	data, err := EncodeEnvelope(envelope)
	if err != nil {
		return err
	}
	return connection.writeEncodedLocked(data)
}

func (connection *wsConnection) writeEncodedLocked(data []byte) error {
	if err := connection.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	return connection.conn.WriteMessage(websocket.TextMessage, data)
}

// pushFarmDelta delivers a room delta. encoded must be the once-encoded public
// Envelope; active connections WriteMessage it directly. Connections in hold
// cache the structured delta and encode only when the EnterFarm Rsp is flushed.
func (connection *wsConnection) pushFarmDelta(ownerUID uint64, delta farm.FarmDelta, encoded []byte) error {
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()

	connection.roomMu.Lock()
	receiving := connection.roomUID == ownerUID
	holding := connection.holdFarmDeltas
	if receiving && holding {
		connection.heldFarmDeltas = append(connection.heldFarmDeltas, copyFarmDelta(delta))
	}
	connection.roomMu.Unlock()
	if !receiving || holding {
		return nil
	}
	if len(encoded) == 0 {
		var err error
		encoded, err = wireenv.EncodeFarmDelta(delta)
		if err != nil {
			return err
		}
	}
	return connection.writeEncodedLocked(encoded)
}

// pushPlayerDelta delivers state owned by this connection's authenticated
// player. Unlike FarmDelta, it is independent of the farm room being viewed.
func (connection *wsConnection) pushPlayerDelta(delta farm.PlayerDelta) {
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	_ = connection.respondLocked(Envelope{
		Cmd:       CommandPlayerDelta,
		ClientSeq: 0,
		Payload:   marshalPayload(delta),
	})
}

func (connection *wsConnection) pushMailNotify(kind string) error {
	if connection == nil {
		return errors.New("gateway: nil MailNotify connection")
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	return connection.respondLocked(Envelope{
		Cmd:       CommandMailNotify,
		ClientSeq: 0,
		Payload: marshalPayload(struct {
			Kind string `json:"kind"`
		}{Kind: kind}),
	})
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
			obs.L().Debug("gateway MailNotify delivery failed",
				"component", "gateway",
				"op", "deliver_mail_notify",
				"uid", connection.uid,
				"conn_id", connection.id,
				"kind", kind,
				"err", err.Error(),
			)
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
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	return connection.respondLocked(Envelope{
		Cmd:       CommandTaskNotify,
		ClientSeq: 0,
		Payload:   marshalPayload(task),
	})
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
			obs.L().Debug("gateway TaskNotify delivery failed",
				"component", "gateway",
				"op", "deliver_task_notify",
				"uid", connection.uid,
				"conn_id", connection.id,
				"task_id", task.ID,
				"err", err.Error(),
			)
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

func sessionErrorCode(err error) pkgerr.Code {
	if errors.Is(err, store.ErrSessionReplaced) {
		return pkgerr.Kicked
	}
	if errors.Is(err, store.ErrSessionNotFound) {
		return pkgerr.Unauthorized
	}
	return pkgerr.Internal
}
