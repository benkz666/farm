// Package farmrpc implements the authenticated, internal Farm command boundary.
package farmrpc

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"farm/server/internal/actor"
	"farm/server/internal/connreg"
	"farm/server/internal/cross"
	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/obs"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

const commandPath = "/internal/v1/cmd"

// Operation identifies a Farm operation that a Gateway may invoke remotely.
type Operation string

const (
	OperationEnterFarm    Operation = "enter_farm"
	OperationTill         Operation = "till" // legacy alias used by older Gateways
	OperationPlotAction   Operation = "plot_action"
	OperationShop         Operation = "shop"
	OperationSyncFarm     Operation = "sync_farm"
	OperationPet          Operation = "pet"
	OperationCrossReserve Operation = "cross_reserve"
	OperationCrossSettle  Operation = "cross_settle"
	OperationTaskClaim    Operation = "task_claim"
	OperationDailyLogin   Operation = "daily_login_claim"
	OperationMailClaim    Operation = "mail_claim"
	OperationCodexList    Operation = "codex_list"
)

// CommandRequest is the HTTP JSON payload sent from a Gateway to the Farm
// authoritative for FarmUID. The Gateway authenticates the player first.
type CommandRequest struct {
	Operation  Operation       `json:"operation"`
	FarmUID    uint64          `json:"farm_uid"`
	Originator connreg.ConnRef `json:"originator,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
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
	FarmSeq      uint64                   `json:"farm_seq"`
	Patch        farm.PatchJSON           `json:"patch"`
	CodexRewards []farm.CodexRewardNotice `json:"codex_rewards,omitempty"`
}

type CodexListResponse struct {
	Entries []farm.CodexProgress `json:"entries"`
	Total   int                  `json:"total"`
}

// PlotActionRequest carries one owner-authoritative plot mutation.
type PlotActionRequest struct {
	OwnerUID  uint64              `json:"owner_uid"`
	PlotIndex uint32              `json:"plot_index"`
	Arg       uint32              `json:"arg"`
	Kind      farm.PlotActionKind `json:"kind"`
	Command   uint32              `json:"command"`
}

// ShopRequest carries one owner-authoritative Buy or Sell.
type ShopRequest struct {
	Buy      bool   `json:"buy"`
	ItemID   uint32 `json:"item_id"`
	Quantity uint32 `json:"quantity"`
	Command  uint32 `json:"command"`
}

// SyncFarmRequest asks the Actor delta ring for changes after FromSeq.
type SyncFarmRequest struct {
	FromSeq uint64 `json:"from_seq"`
}

// SyncFarmResponse mirrors the public SyncFarm payload.
type SyncFarmResponse struct {
	Deltas   []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq  uint64                 `json:"farm_seq"`
}

// PetOperation identifies the local pet mutation.
type PetOperation string

const (
	PetStatus   PetOperation = "status"
	PetActivate PetOperation = "activate"
	PetFeed     PetOperation = "feed"
)

// PetRequest carries PetStatus, PetActivate or PetFeed inputs.
type PetRequest struct {
	Kind    PetOperation `json:"kind"`
	DogType farm.DogType `json:"dog_type,omitempty"`
	Grams   uint32       `json:"grams,omitempty"`
}

// CrossSettleResponse returns the client reward and authoritative player state.
type CrossSettleResponse struct {
	Reward      cross.VisitorReward `json:"reward"`
	PlayerDelta *farm.PlayerDelta   `json:"player_delta,omitempty"`
}

// MailClaimRequest identifies one mail attachment.
type MailClaimRequest struct {
	MailID uint64 `json:"mail_id"`
}

// TaskClaimRequest identifies one completed daily task.
type TaskClaimRequest struct {
	TaskID uint32 `json:"task_id"`
}

// Runtime is the minimal Actor boundary needed by this RPC handler.
type Runtime interface {
	Do(uid uint64, fn func(*actor.FarmActor) error) error
}

// Handler serves authenticated Farm commands for a single physical Farm.
type Handler struct {
	runtime              Runtime
	token                []byte
	owns                 func(uint64) bool
	now                  func() int64
	deltaPublisher       DeltaPublisher
	playerDeltaPublisher PlayerDeltaPublisher
	stealHints           StealHintWriter
	taskProgress         TaskProgressWriter
	taskNotifyPublisher  TaskNotifyPublisher
	taskClaimer          TaskClaimer
	dailyLoginClaimer    DailyLoginClaimer
	mailClaimer          MailClaimer
	codexRewards         store.CodexRewardStore
	mailNotifyPublisher  MailNotifyPublisher
}

// StealHintWriter updates the weak-consistent FriendList stealable hint.
type StealHintWriter interface {
	SetStealHint(ctx context.Context, uid uint64, hasStealable bool) error
}

// TaskProgressWriter advances gameplay-backed daily tasks.
type TaskProgressWriter interface {
	AdvanceTask(ctx context.Context, uid uint64, dayKey int64, taskID, amount uint32) (store.TaskAdvanceResult, error)
}

// TaskClaimer atomically marks a completed task and credits its direct reward.
type TaskClaimer interface {
	ClaimTask(ctx context.Context, uid uint64, dayKey int64, taskID uint32) (store.TaskReward, error)
}

// DailyLoginClaimer atomically records and credits the daily login reward.
type DailyLoginClaimer interface {
	ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (store.TaskReward, error)
}

// MailClaimer atomically marks and credits a mail attachment in durable storage.
type MailClaimer interface {
	ClaimMail(ctx context.Context, uid uint64, mailID uint64) (store.Mail, error)
}

// Option configures optional Farm RPC behavior.
type Option func(*Handler)

// WithDeltaPublisher emits FarmDelta callbacks after authoritative mutations.
func WithDeltaPublisher(publisher DeltaPublisher) Option {
	return func(handler *Handler) {
		handler.deltaPublisher = publisher
	}
}

// WithStealHintWriter refreshes Redis stealable hints after farm mutations.
func WithStealHintWriter(hints StealHintWriter) Option {
	return func(handler *Handler) {
		handler.stealHints = hints
	}
}

// WithPlayerDeltaPublisher emits personal-state updates after Farm-side
// settlement such as cross actions and mail claims.
func WithPlayerDeltaPublisher(publisher PlayerDeltaPublisher) Option {
	return func(handler *Handler) {
		handler.playerDeltaPublisher = publisher
	}
}

// WithTaskProgressWriter connects successful Plant/Harvest events to tasks.
func WithTaskProgressWriter(tasks TaskProgressWriter) Option {
	return func(handler *Handler) {
		handler.taskProgress = tasks
	}
}

// WithTaskNotifyPublisher emits task progress snapshots after changed gameplay
// task records.
func WithTaskNotifyPublisher(publisher TaskNotifyPublisher) Option {
	return func(handler *Handler) {
		handler.taskNotifyPublisher = publisher
	}
}

// WithTaskClaimer enables Actor-serialized direct task reward claims.
func WithTaskClaimer(claimer TaskClaimer) Option {
	return func(handler *Handler) {
		handler.taskClaimer = claimer
	}
}

// WithDailyLoginClaimer enables Actor-serialized daily login reward claims.
func WithDailyLoginClaimer(claimer DailyLoginClaimer) Option {
	return func(handler *Handler) {
		handler.dailyLoginClaimer = claimer
	}
}

// WithMailClaimer enables Actor-serialized mail attachment claims.
func WithMailClaimer(claimer MailClaimer) Option {
	return func(handler *Handler) {
		handler.mailClaimer = claimer
	}
}

// WithCodexRewardStore enables idempotent per-crop plaque reward mails.
func WithCodexRewardStore(rewards store.CodexRewardStore) Option {
	return func(handler *Handler) {
		handler.codexRewards = rewards
	}
}

// WithMailNotifyPublisher notifies the owning Gateway after a reward mail is created.
func WithMailNotifyPublisher(publisher MailNotifyPublisher) Option {
	return func(handler *Handler) {
		handler.mailNotifyPublisher = publisher
	}
}

// NewHandler creates the /internal/v1/cmd handler. owns must only return true
// for uids assigned to this Farm instance by the loaded route table.
func NewHandler(runtime Runtime, token []byte, owns func(uint64) bool, now func() int64, options ...Option) *Handler {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if owns == nil {
		owns = func(uint64) bool { return false }
	}
	handler := &Handler{
		runtime: runtime,
		token:   append([]byte(nil), token...),
		owns:    owns,
		now:     now,
	}
	for _, option := range options {
		if option != nil {
			option(handler)
		}
	}
	return handler
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
		return h.enterFarm(request)
	case OperationTill:
		return h.till(request)
	case OperationPlotAction:
		return h.plotAction(request)
	case OperationShop:
		return h.shop(request)
	case OperationSyncFarm:
		return h.syncFarm(request)
	case OperationPet:
		return h.pet(request)
	case OperationCrossReserve:
		return h.crossReserve(request)
	case OperationCrossSettle:
		return h.crossSettle(request)
	case OperationTaskClaim:
		return h.taskClaim(request)
	case OperationDailyLogin:
		return h.dailyLoginClaim(request)
	case OperationMailClaim:
		return h.mailClaim(request)
	case OperationCodexList:
		return h.codexList(request)
	default:
		return CommandResponse{Err: pkgerr.BadRequest}
	}
}

func (h *Handler) enterFarm(command CommandRequest) CommandResponse {
	var response EnterFarmResponse
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		changes := farmActor.Aggregate.AdvanceAll(h.now())
		response = EnterFarmResponse{
			Snapshot:   farmActor.Aggregate.Snapshot(),
			FarmSeq:    farmActor.Aggregate.FarmSeq,
			ServerTime: h.now(),
		}
		if len(changes) > 0 {
			emitted := farm.FarmDelta{
				OwnerUID: command.FarmUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    changes,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
			stealable = farmActor.Aggregate.HasStealable()
			refreshHint = true
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	h.publishDelta(delta, command.Originator)
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func (h *Handler) till(command CommandRequest) CommandResponse {
	var request PlotActionRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		(request.OwnerUID != 0 && request.OwnerUID != command.FarmUID) ||
		request.PlotIndex > 255 || request.Arg > 0xFFFF {
		return CommandResponse{Err: pkgerr.BadRequest}
	}

	var result farm.ActionResult
	var response ActionResponse
	var delta *farm.FarmDelta
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
			emitted := farm.FarmDelta{
				OwnerUID: command.FarmUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    []farm.PlotChange{plotChange(uint8(request.PlotIndex), farmActor.Aggregate.Plots[request.PlotIndex])},
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if result.Err != pkgerr.OK {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func (h *Handler) plotAction(command CommandRequest) CommandResponse {
	var request PlotActionRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		(request.OwnerUID != 0 && request.OwnerUID != command.FarmUID) ||
		request.PlotIndex > 255 || request.Arg > 0xFFFF ||
		request.Kind < farm.Till || request.Kind > farm.Harvest {
		return CommandResponse{Err: pkgerr.BadRequest}
	}

	var result farm.ActionResult
	var response ActionResponse
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		beforeFarmSeq := farmActor.Aggregate.FarmSeq
		result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
			Kind:      request.Kind,
			PlotIndex: uint8(request.PlotIndex),
			Arg:       uint16(request.Arg),
		}, h.now())
		if result.Err == pkgerr.OK || (request.Kind == farm.Clear && result.Err == pkgerr.PlotNotCleanable) {
			response = ActionResponse{
				FarmSeq: farmActor.Aggregate.FarmSeq,
				Patch:   farmActor.Aggregate.PatchFromAction(result),
			}
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			emitted := farm.FarmDelta{
				OwnerUID: command.FarmUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots: []farm.PlotChange{plotChange(
					uint8(request.PlotIndex),
					farmActor.Aggregate.Plots[request.PlotIndex],
				)},
				ActorUID: command.FarmUID,
				Action:   request.Command,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
			stealable = farmActor.Aggregate.HasStealable()
			refreshHint = true
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if result.Err != pkgerr.OK && !(request.Kind == farm.Clear && result.Err == pkgerr.PlotNotCleanable) {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	if result.Err == pkgerr.OK {
		if response.Patch.Codex != nil && h.codexRewards != nil {
			rewards, rewardErr := h.codexRewards.IssueCodexRewards(context.Background(), command.FarmUID, *response.Patch.Codex)
			if rewardErr != nil {
				obs.L().Error("farmrpc issue codex rewards failed",
					"component", "farmrpc",
					"op", "issue_codex_rewards",
					"uid", command.FarmUID,
					"crop_id", response.Patch.Codex.CropID,
					"err", rewardErr.Error(),
				)
			} else {
				response.CodexRewards = rewards
				if len(rewards) > 0 {
					h.publishMailNotify(command.FarmUID, "codex_reward")
				}
			}
		}
		// 任务计数是旁路副作用：动作已在 Actor 里提交、Delta 已广播给房间，此时把
		// 响应改成 ERR_INTERNAL 会让发起者回滚一次真实发生的变更，比丢一次任务
		// 进度更糟。失败只记日志，不污染动作结果。
		if err := h.advanceGameplayTask(command.FarmUID, request.Kind); err != nil {
			obs.L().Error("farmrpc advance task failed",
				"component", "farmrpc",
				"op", "advance_task",
				"err", err.Error(),
			)
		}
	}
	return CommandResponse{Err: result.Err, Payload: marshalPayload(response)}
}

func (h *Handler) codexList(command CommandRequest) CommandResponse {
	if len(command.Payload) != 0 && string(command.Payload) != "{}" {
		var payload struct{}
		if err := decodeJSON(bytes.NewReader(command.Payload), &payload); err != nil {
			return CommandResponse{Err: pkgerr.BadRequest}
		}
	}
	var response CodexListResponse
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		response = CodexListResponse{
			Entries: farmActor.Aggregate.CodexSnapshot(),
			Total:   gameconf.CropCount,
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func (h *Handler) shop(command CommandRequest) CommandResponse {
	var request ShopRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		request.ItemID > 0xFFFF {
		return CommandResponse{Err: pkgerr.BadRequest}
	}

	var result farm.ActionResult
	var response ActionResponse
	var delta *farm.FarmDelta
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		beforeFarmSeq := farmActor.Aggregate.FarmSeq
		if request.Buy {
			result = farmActor.Aggregate.Buy(farm.BuyReq{
				ItemID: uint16(request.ItemID), Quantity: request.Quantity,
			})
		} else {
			result = farmActor.Aggregate.Sell(farm.SellReq{
				ItemID: uint16(request.ItemID), Quantity: request.Quantity,
			})
		}
		if result.Err == pkgerr.OK {
			// 同 Gateway 侧买卖：金币改动按 A 档同步落盘（架构 5.3 节）。
			farmActor.RequireFlush()
			response = ActionResponse{
				FarmSeq: farmActor.Aggregate.FarmSeq,
				Patch:   farmActor.Aggregate.PatchFromAction(result),
			}
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			emitted := farm.FarmDelta{
				OwnerUID: command.FarmUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				ActorUID: command.FarmUID,
				Action:   request.Command,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if result.Err != pkgerr.OK {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func (h *Handler) syncFarm(command CommandRequest) CommandResponse {
	var request SyncFarmRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	var response SyncFarmResponse
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		response.FarmSeq = farmActor.Aggregate.FarmSeq
		if request.FromSeq == response.FarmSeq {
			return nil
		}
		if request.FromSeq > response.FarmSeq {
			snapshot := farmActor.Aggregate.Snapshot()
			response.Snapshot = &snapshot
			return nil
		}
		deltas, ok := farmActor.Deltas.Since(request.FromSeq + 1)
		if !ok || len(deltas) == 0 {
			snapshot := farmActor.Aggregate.Snapshot()
			response.Snapshot = &snapshot
			return nil
		}
		response.Deltas = deltas
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(response)}
}

func (h *Handler) pet(command CommandRequest) CommandResponse {
	var request PetRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	var result farm.ActionResult
	var status farm.PetStatus
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		switch request.Kind {
		case PetStatus:
			result.Err = pkgerr.OK
		case PetActivate:
			result = farmActor.Aggregate.PetActivate(request.DogType)
		case PetFeed:
			result = farmActor.Aggregate.PetFeed(farm.PetFeedReq{Grams: request.Grams}, h.now())
		default:
			result.Err = pkgerr.BadRequest
		}
		status = farmActor.Aggregate.PetStatus(h.now())
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if result.Err != pkgerr.OK {
		return CommandResponse{Err: result.Err}
	}
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(status)}
}

func (h *Handler) crossReserve(command CommandRequest) CommandResponse {
	var reservation cross.VisitorReservation
	if err := decodeJSON(bytes.NewReader(command.Payload), &reservation); err != nil ||
		reservation.Action.VisitorUID != command.FarmUID {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	var code pkgerr.Code
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		now := h.now()
		code = cross.ReserveVisitor(farmActor.Aggregate, reservation, now)
		obs.L().Debug("farmrpc cross reserve",
			"component", "farmrpc",
			"op", "cross_reserve",
			"code", int(code),
		)
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	return CommandResponse{Err: code}
}

func (h *Handler) crossSettle(command CommandRequest) CommandResponse {
	var result cross.CrossResult
	if err := decodeJSON(bytes.NewReader(command.Payload), &result); err != nil ||
		result.VisitorUID != command.FarmUID {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	var response CrossSettleResponse
	var code pkgerr.Code
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		now := h.now()
		response.Reward, response.PlayerDelta, code = cross.SettleVisitor(farmActor.Aggregate, result, now)
		obs.L().Debug("farmrpc cross settle",
			"component", "farmrpc",
			"op", "cross_settle",
			"result_code", int(result.Code),
			"settle_code", int(code),
		)
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if response.PlayerDelta != nil {
		h.publishPlayerDelta(command.FarmUID, *response.PlayerDelta)
	}
	return CommandResponse{Err: code, Payload: marshalPayload(response)}
}

func (h *Handler) taskClaim(command CommandRequest) CommandResponse {
	var request TaskClaimRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil || request.TaskID == 0 {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	return h.claimTask(command, request.TaskID, false)
}

func (h *Handler) claimTask(command CommandRequest, taskID uint32, dailyLoginCompatibility bool) CommandResponse {
	if h.taskClaimer == nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	var reward store.TaskReward
	var claimErr error
	var playerDelta farm.PlayerDelta
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		dayKey := gameconf.LocalDayKey(h.now())
		reward, claimErr = h.taskClaimer.ClaimTask(context.Background(), command.FarmUID, dayKey, taskID)
		if claimErr != nil {
			return nil
		}
		farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if claimErr != nil {
		if dailyLoginCompatibility {
			return CommandResponse{Err: dailyLoginErrorCode(claimErr)}
		}
		return CommandResponse{Err: taskClaimErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(reward)}
}

func (h *Handler) dailyLoginClaim(command CommandRequest) CommandResponse {
	if err := decodeJSON(bytes.NewReader(command.Payload), &struct{}{}); err != nil {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	if h.taskClaimer != nil {
		return h.claimTask(command, store.TaskDailyLoginID, true)
	}
	if h.dailyLoginClaimer == nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	var reward store.TaskReward
	var claimErr error
	var playerDelta farm.PlayerDelta
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		dayKey := gameconf.LocalDayKey(h.now())
		reward, claimErr = h.dailyLoginClaimer.ClaimDailyLogin(context.Background(), command.FarmUID, dayKey)
		if claimErr != nil {
			return nil
		}
		farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if claimErr != nil {
		return CommandResponse{Err: dailyLoginErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(reward)}
}

func (h *Handler) mailClaim(command CommandRequest) CommandResponse {
	if h.mailClaimer == nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	var request MailClaimRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil || request.MailID == 0 {
		return CommandResponse{Err: pkgerr.BadRequest}
	}
	var mail store.Mail
	var playerDelta farm.PlayerDelta
	var claimErr error
	if err := h.runtime.Do(command.FarmUID, func(farmActor *actor.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		mail, claimErr = h.mailClaimer.ClaimMail(context.Background(), command.FarmUID, request.MailID)
		if claimErr != nil {
			return nil
		}
		farmActor.Aggregate.CreditMailReward(mail.AttachmentCoin)
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: pkgerr.Internal}
	}
	if claimErr != nil {
		return CommandResponse{Err: mailClaimErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: pkgerr.OK, Payload: marshalPayload(mail)}
}

func taskClaimErrorCode(err error) pkgerr.Code {
	switch {
	case errors.Is(err, store.ErrTaskNotComplete):
		return pkgerr.TaskNotComplete
	case errors.Is(err, store.ErrTaskAlreadyClaimed):
		return pkgerr.TaskAlreadyClaimed
	case errors.Is(err, store.ErrDailyLoginAlreadyClaimed):
		return pkgerr.DuplicateOK
	default:
		return pkgerr.Internal
	}
}

func dailyLoginErrorCode(err error) pkgerr.Code {
	if errors.Is(err, store.ErrTaskAlreadyClaimed) || errors.Is(err, store.ErrDailyLoginAlreadyClaimed) {
		return pkgerr.DuplicateOK
	}
	return taskClaimErrorCode(err)
}

func (h *Handler) advanceGameplayTask(uid uint64, kind farm.PlotActionKind) error {
	if h.taskProgress == nil {
		return nil
	}
	var taskID uint32
	switch kind {
	case farm.Plant:
		taskID = store.TaskPlantID
	case farm.Harvest:
		taskID = store.TaskHarvestID
	default:
		return nil
	}
	dayKey := gameconf.LocalDayKey(h.now())
	result, err := h.taskProgress.AdvanceTask(context.Background(), uid, dayKey, taskID, 1)
	if err != nil {
		return err
	}
	if result.Changed {
		h.publishTaskNotify(uid, result.Task)
	}
	return nil
}

func (h *Handler) publishPlayerDelta(uid uint64, delta farm.PlayerDelta) {
	if h.playerDeltaPublisher == nil {
		return
	}
	publisher := h.playerDeltaPublisher
	go func() {
		_ = publisher.PublishPlayerDelta(context.Background(), uid, delta)
	}()
}

func (h *Handler) publishTaskNotify(uid uint64, task store.Task) {
	if h.taskNotifyPublisher == nil || uid == 0 {
		return
	}
	publisher := h.taskNotifyPublisher
	go func() {
		if err := publisher.PublishTaskNotify(context.Background(), uid, task); err != nil {
			obs.L().Error("farmrpc TaskNotify publish failed",
				"component", "farmrpc",
				"op", "publish_task_notify",
				"uid", uid,
				"task_id", task.ID,
				"err", err.Error(),
			)
		}
	}()
}

func (h *Handler) publishMailNotify(uid uint64, kind string) {
	if h.mailNotifyPublisher == nil || uid == 0 {
		return
	}
	publisher := h.mailNotifyPublisher
	go func() {
		if err := publisher.PublishMailNotify(context.Background(), uid, kind); err != nil {
			obs.L().Error("farmrpc MailNotify publish failed",
				"component", "farmrpc",
				"op", "publish_mail_notify",
				"uid", uid,
				"kind", kind,
				"err", err.Error(),
			)
		}
	}()
}

func mailClaimErrorCode(err error) pkgerr.Code {
	switch {
	case errors.Is(err, store.ErrMailNotFound):
		return pkgerr.MailNotFound
	case errors.Is(err, store.ErrMailNoAttachment):
		return pkgerr.MailNoAttachment
	case errors.Is(err, store.ErrMailAlreadyClaimed):
		return pkgerr.MailAlreadyClaimed
	default:
		return pkgerr.Internal
	}
}

func (h *Handler) publishDelta(delta *farm.FarmDelta, originator connreg.ConnRef) {
	if delta == nil || h.deltaPublisher == nil {
		return
	}
	publisher := h.deltaPublisher
	emitted := *delta
	go func() {
		// Delta delivery is best effort; SyncFarm recovers a missed callback.
		if err := publisher.Publish(context.Background(), emitted, originator); err != nil {
			obs.L().Error("farmrpc delta publish failed",
				"component", "farmrpc",
				"op", "publish_delta",
				"err", err.Error(),
			)
		}
	}()
}

func (h *Handler) writeStealHint(uid uint64, hasStealable bool) {
	if h == nil || h.stealHints == nil || uid == 0 {
		return
	}
	_ = h.stealHints.SetStealHint(context.Background(), uid, hasStealable)
}

func plotChange(index uint8, plot farm.Plot) farm.PlotChange {
	return farm.PlotChangeOf(index, plot)
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
