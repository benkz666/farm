// Package farmrpc implements the authenticated, internal Farm command boundary.
package farmrpc

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/farm"
	"farm/server/internal/pkgerr"
)

const commandPath = "/internal/v1/cmd"

// Operation identifies a Farm operation that a Gateway may invoke remotely.
type Operation string

const (
	OperationEnterFarm Operation = "enter_farm"
	OperationTill      Operation = "till"
)

// CommandRequest is the HTTP JSON payload sent from a Gateway to the Farm
// authoritative for FarmUID. The Gateway authenticates the player first.
type CommandRequest struct {
	Operation Operation       `json:"operation"`
	FarmUID   uint64          `json:"farm_uid"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// CommandResponse preserves protocol-level errors inside a successful internal
// request so a Gateway can return the same error code to its client.
type CommandResponse struct {
	Err     pkgerr.Code     `json:"err"`
	Payload json.RawMessage `json:"payload"`
}

// EnterFarmResponse is the Farm-owned portion of an EnterFarm response.
// Relation remains a Gateway concern because it depends on social access.
type EnterFarmResponse struct {
	Snapshot   farm.FarmSnapshotJSON `json:"snapshot"`
	FarmSeq    uint64                `json:"farm_seq"`
	ServerTime int64                 `json:"server_time"`
}

// ActionResponse is the Farm-owned result for a plot mutation.
type ActionResponse struct {
	FarmSeq uint64         `json:"farm_seq"`
	Patch   farm.PatchJSON `json:"patch"`
}

type tillRequest struct {
	OwnerUID  uint64 `json:"owner_uid"`
	PlotIndex uint32 `json:"plot_index"`
	Arg       uint32 `json:"arg"`
}

// Runtime is the minimal Actor boundary needed by this RPC handler.
type Runtime interface {
	Do(uid uint64, fn func(*actor.FarmActor) error) error
}

// Handler serves authenticated Farm commands for a single physical Farm.
type Handler struct {
	runtime Runtime
	token   []byte
	owns    func(uint64) bool
	now     func() int64
}

// NewHandler creates the /internal/v1/cmd handler. owns must only return true
// for uids assigned to this Farm instance by the loaded route table.
func NewHandler(runtime Runtime, token []byte, owns func(uint64) bool, now func() int64) *Handler {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if owns == nil {
		owns = func(uint64) bool { return false }
	}
	return &Handler{
		runtime: runtime,
		token:   append([]byte(nil), token...),
		owns:    owns,
		now:     now,
	}
}

// ServeHTTP accepts only HTTP JSON POST requests from trusted Gateways.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r.Header.Get("Authorization")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var request CommandRequest
	if err := decodeJSON(io.LimitReader(r.Body, 64<<10), &request); err != nil {
		writeResponse(w, http.StatusBadRequest, CommandResponse{Err: pkgerr.BadRequest})
		return
	}
	if request.FarmUID == 0 || !h.owns(request.FarmUID) {
		writeResponse(w, http.StatusNotFound, CommandResponse{Err: pkgerr.BadRequest})
		return
	}

	response := h.execute(request)
	writeResponse(w, http.StatusOK, response)
}

func (h *Handler) authorized(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) || len(h.token) == 0 {
		return false
	}
	value := []byte(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
	return len(value) == len(h.token) && subtle.ConstantTimeCompare(value, h.token) == 1
}

func (h *Handler) execute(request CommandRequest) CommandResponse {
	if h.runtime == nil {
		return CommandResponse{Err: pkgerr.Internal}
	}

	switch request.Operation {
	case OperationEnterFarm:
		return h.enterFarm(request.FarmUID)
	case OperationTill:
		return h.till(request)
	default:
		return CommandResponse{Err: pkgerr.BadRequest}
	}
}

func (h *Handler) enterFarm(uid uint64) CommandResponse {
	var response EnterFarmResponse
	if err := h.runtime.Do(uid, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		farmActor.Aggregate.AdvanceAll(h.now())
		response = EnterFarmResponse{
			Snapshot:   farmActor.Aggregate.Snapshot(),
			FarmSeq:    farmActor.Aggregate.FarmSeq,
			ServerTime: h.now(),
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func (h *Handler) till(command CommandRequest) CommandResponse {
	var request tillRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		(request.OwnerUID != 0 && request.OwnerUID != command.FarmUID) ||
		request.PlotIndex > 255 || request.Arg > 0xFFFF {
		return CommandResponse{Err: pkgerr.BadRequest}
	}

	var result farm.ActionResult
	var response ActionResponse
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
			Kind:      farm.Till,
			PlotIndex: uint8(request.PlotIndex),
			Arg:       uint16(request.Arg),
		}, h.now())
		if result.Err == pkgerr.OK {
			response = ActionResponse{
				FarmSeq: farmActor.Aggregate.FarmSeq,
				Patch:   farmActor.Aggregate.PatchFromAction(result),
			}
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if result.Err != pkgerr.OK {
		return CommandResponse{Err: result.Err}
	}
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("farmrpc: trailing JSON value")
	}
	return nil
}

func writeResponse(w http.ResponseWriter, status int, response CommandResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func marshalPayload(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}
