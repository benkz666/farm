package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"farm/server/internal/actor"
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
	uid     uint64
	authed  bool
	limiter *connectionLimiter
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
	if g.sessions == nil || g.runtime == nil {
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
		limiter: newConnectionLimiter(),
	}
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
		if err := connection.respond(response); err != nil {
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
		var payload enterFarmRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		if payload.OwnerUID != 0 && payload.OwnerUID != connection.uid {
			response.Err = pkgerr.NotFriend
			return response
		}

		var enter enterFarmResponse
		if err := g.runtime.Do(connection.uid, func(farmActor *actor.FarmActor) error {
			if farmActor == nil || farmActor.Aggregate == nil {
				return errors.New("gateway: actor aggregate is nil")
			}
			enter = enterFarmResponse{
				Snapshot:   farmActor.Aggregate.Snapshot(),
				FarmSeq:    farmActor.Aggregate.FarmSeq,
				ServerTime: g.Now(),
				Relation:   "SELF",
			}
			return nil
		}); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Payload = marshalPayload(enter)
	case CommandTill, CommandClear, CommandPlant, CommandWater,
		CommandRemoveWeed, CommandRemovePest, CommandHarvest,
		CommandBuy, CommandSell:
		return g.handlePlotOrShop(connection, request)
	default:
		response.Err = pkgerr.BadRequest
	}
	return response
}

func (connection *wsConnection) respond(envelope Envelope) error {
	data, err := EncodeEnvelope(envelope)
	if err != nil {
		return err
	}
	return connection.conn.WriteMessage(websocket.TextMessage, data)
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
