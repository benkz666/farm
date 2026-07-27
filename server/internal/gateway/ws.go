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

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

var wsUpgrader = websocket.Upgrader{
	Subprotocols: []string{JSONSubprotocol},
	// Development accepts any origin; production must restrict this to trusted origins.
	CheckOrigin: func(*http.Request) bool { return true },
}

const (
	maxWSMessageSize = 64 << 10
	wsReadTimeout    = 90 * time.Second
)

type wsConnection struct {
	conn    *websocket.Conn
	id      uint64
	uid     uint64
	authed  bool
	limiter *connectionLimiter
	writeMu sync.Mutex
	roomMu  sync.Mutex
	roomUID uint64
	// holdFarmDeltas keeps a newly-entered client from observing a delta before
	// its EnterFarm snapshot has reached the wire.
	holdFarmDeltas bool
	heldFarmDeltas []farm.FarmDelta
}

type handshakeRequest struct {
	Token           string `json:"token"`
	ResumeFarmUID   uint64 `json:"resume_farm_uid"`
	ResumeFarmSeq   uint64 `json:"resume_farm_seq"`
	ClientConfigVer uint32 `json:"client_config_ver"`
}

type handshakeResponse struct {
	UID uint64 `json:"uid"`
}

type pingRequest struct {
	ClientTime int64 `json:"client_time"`
}

type pongResponse struct {
	ClientTime int64 `json:"client_time"`
	ServerTime int64 `json:"server_time"`
}

type enterFarmRequest struct {
	OwnerUID uint64 `json:"owner_uid"`
}

type enterFarmResponse struct {
	Snapshot   any    `json:"snapshot"`
	FarmSeq    uint64 `json:"farm_seq"`
	ServerTime int64  `json:"server_time"`
	Relation   string `json:"relation"`
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

	connection := wsConnection{
		conn:    conn,
		id:      g.nextConnID.Add(1),
		limiter: newConnectionLimiter(),
	}
	defer func() {
		g.leaveFarm(&connection)
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

		response := g.handleWSRequest(&connection, request)
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
		if err := g.registerConnection(context.Background(), connection); err != nil {
			connection.uid = 0
			response.Err = pkgerr.Internal
			return response
		}
		connection.authed = true
		response.Payload = marshalPayload(handshakeResponse{UID: uid})
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
		CommandRemoveFriend, CommandAddFriendByUID, CommandSearchUser:
		return g.handleFriendRequest(connection, request)
	case CommandTaskList, CommandTaskClaim, CommandMailList, CommandMailClaim, CommandClaimDailyLogin:
		return g.handleTaskMailRequest(connection, request)
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
		if err := connection.respondLocked(Envelope{
			Cmd:       CommandFarmDelta,
			ClientSeq: 0,
			Payload:   marshalPayload(delta),
		}); err != nil {
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
	return connection.conn.WriteMessage(websocket.TextMessage, data)
}

func (connection *wsConnection) pushFarmDelta(ownerUID uint64, delta farm.FarmDelta) {
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
		return
	}
	_ = connection.respondLocked(Envelope{
		Cmd:       CommandFarmDelta,
		ClientSeq: 0,
		Payload:   marshalPayload(delta),
	})
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
	if errors.Is(err, store.ErrSessionNotFound) {
		return pkgerr.Unauthorized
	}
	return pkgerr.Internal
}
