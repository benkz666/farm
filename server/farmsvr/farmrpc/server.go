// Package farmrpc implements the authenticated, internal Farm command boundary.
package farmrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/crossfarm"
	"farm/server/farmsvr/room"
	"farm/server/gateway/presence"
	"farm/server/shared/clientjson"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

// Operation identifies a Farm operation that a Gateway may invoke remotely.
type Operation string

const (
	OperationEnterFarm    Operation = "enter_farm"
	OperationPlotAction   Operation = "plot_action"
	OperationShop         Operation = "shop"
	OperationSyncFarm     Operation = "sync_farm"
	OperationPet          Operation = "pet"
	OperationCrossReserve Operation = "cross_reserve"
	OperationCrossSettle  Operation = "cross_settle"
	OperationTaskList     Operation = "task_list"
	OperationTaskClaim    Operation = "task_claim"
	OperationAdvanceTask  Operation = "advance_task"
	OperationDailyLogin   Operation = "daily_login_claim"
	OperationMailList     Operation = "mail_list"
	OperationMailRead     Operation = "mail_read"
	OperationMailDelete   Operation = "mail_delete"
	OperationMailClaim    Operation = "mail_claim"
	OperationCodexList    Operation = "codex_list"
)

// CommandRequest is the HTTP JSON payload sent from a Gateway to the Farm
// authoritative for FarmUID. The Gateway authenticates the player first.
type CommandRequest struct {
	Operation  Operation        `json:"operation"`
	FarmUID    uint64           `json:"farm_uid"`
	Originator presence.ConnRef `json:"originator,omitempty"`
	Payload    json.RawMessage  `json:"payload,omitempty"`
}

// CommandResponse preserves protocol-level errors inside a successful internal
// request so a Gateway can return the same error code to its client.
type CommandResponse struct {
	Err     errcode.Code    `json:"err"`
	Payload json.RawMessage `json:"payload"`
}

// EnterFarmResponse is the Farm-owned portion of an EnterFarm response.
// Relation remains a Gateway concern because it depends on social access.
type EnterFarmResponse struct {
	Snapshot    farm.FarmSnapshotJSON `json:"snapshot"`
	FarmSeq     clientjson.Uint64     `json:"farm_seq"`
	ServerTime  int64                 `json:"server_time"`
	TimeProfile string                `json:"time_profile"`
}

// ActionResponse is the Farm-owned result for a plot mutation.
type ActionResponse struct {
	FarmSeq      clientjson.Uint64        `json:"farm_seq"`
	Patch        farm.PatchJSON           `json:"patch"`
	CodexRewards []farm.CodexRewardNotice `json:"codex_rewards,omitempty"`
}

type CodexListResponse struct {
	Entries []farm.CodexProgress `json:"entries"`
	Total   int                  `json:"total"`
}

// TaskListResponse mirrors the public TaskList payload.
type TaskListResponse struct {
	Tasks   []store.Task `json:"tasks"`
	ResetAt int64        `json:"reset_at"`
}

// MailListResponse mirrors the public MailList payload.
type MailListResponse struct {
	Mails []store.Mail `json:"mails"`
}

// MailMutationRequest identifies one mail or a bulk mutation.
type MailMutationRequest struct {
	MailID uint64 `json:"mail_id,omitempty"`
	All    bool   `json:"all"`
}

// MailMutationResponse reports how many mails were affected.
type MailMutationResponse struct {
	Affected int64 `json:"affected"`
}

// AdvanceTaskRequest advances one calendar-day task counter.
type AdvanceTaskRequest struct {
	TaskID uint32 `json:"task_id"`
	Amount uint32 `json:"amount"`
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
	Deltas      []farm.FarmDelta       `json:"deltas,omitempty"`
	Snapshot    *farm.FarmSnapshotJSON `json:"snapshot,omitempty"`
	FarmSeq     clientjson.Uint64      `json:"farm_seq"`
	ServerTime  int64                  `json:"server_time"`
	TimeProfile string                 `json:"time_profile"`
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
	Reward      crossfarm.VisitorReward `json:"reward"`
	PlayerDelta *farm.PlayerDelta       `json:"player_delta,omitempty"`
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
	Do(uid uint64, fn func(*room.FarmActor) error) error
}

// Handler serves authenticated Farm commands for a single physical Farm.
type Handler struct {
	runtime              Runtime
	token                []byte
	owns                 func(uint64) bool
	now                  func() int64
	timeProfiles         *gameconfig.TimeProfileSwitch
	deltaPublisher       DeltaPublisher
	playerDeltaPublisher PlayerDeltaPublisher
	stealHints           StealHintWriter
	taskMail             store.TaskMailStore
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

// WithTimeProfile configures the server-authoritative growth profile. It is a
// process-level setting and is never accepted from browser action payloads.
func WithTimeProfile(profile string) Option {
	return func(handler *Handler) {
		if gameconfig.ValidTimeProfile(profile) {
			handler.timeProfiles = gameconfig.NewTimeProfileSwitch(profile)
		}
	}
}

// WithTimeProfileSwitch shares a runtime-switchable authoritative profile with
// the process bootstrap. It is used only by the debug hot-switch surface.
func WithTimeProfileSwitch(profiles *gameconfig.TimeProfileSwitch) Option {
	return func(handler *Handler) {
		if profiles != nil {
			handler.timeProfiles = profiles
		}
	}
}

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

// WithTaskMailStore enables list/read/delete mail operations that stay on the
// Farm HTTP boundary without entering the Actor.
func WithTaskMailStore(taskMail store.TaskMailStore) Option {
	return func(handler *Handler) {
		handler.taskMail = taskMail
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

// NewHandler constructs the transport-neutral farm command executor. owns must
// only return true for uids assigned to this Farm instance by the route table.
func NewHandler(runtime Runtime, token []byte, owns func(uint64) bool, now func() int64, options ...Option) *Handler {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if owns == nil {
		owns = func(uint64) bool { return false }
	}
	handler := &Handler{
		runtime:      runtime,
		token:        append([]byte(nil), token...),
		owns:         owns,
		now:          now,
		timeProfiles: gameconfig.NewTimeProfileSwitch(gameconfig.TimeProfileDemo),
	}
	for _, option := range options {
		if option != nil {
			option(handler)
		}
	}
	return handler
}

// Execute runs one Gateway-authorized command against the local farm runtime.
func (h *Handler) Execute(request CommandRequest) CommandResponse {
	if h.runtime == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if request.FarmUID == 0 || h.owns != nil && !h.owns(request.FarmUID) {
		return CommandResponse{Err: errcode.BadRequest}
	}

	switch request.Operation {
	case OperationEnterFarm:
		return h.enterFarm(request)
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
	case OperationTaskList:
		return h.taskList(request)
	case OperationTaskClaim:
		return h.taskClaim(request)
	case OperationAdvanceTask:
		return h.advanceTaskCommand(request)
	case OperationDailyLogin:
		return h.dailyLoginClaim(request)
	case OperationMailList:
		return h.mailList(request)
	case OperationMailRead:
		return h.mailRead(request)
	case OperationMailDelete:
		return h.mailDelete(request)
	case OperationMailClaim:
		return h.mailClaim(request)
	case OperationCodexList:
		return h.codexList(request)
	default:
		return CommandResponse{Err: errcode.BadRequest}
	}
}

func (h *Handler) enterFarm(command CommandRequest) CommandResponse {
	var response EnterFarmResponse
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		changes := farmActor.Aggregate.AdvanceAllWithProfile(h.now(), h.timeProfiles.Get())
		response = EnterFarmResponse{
			Snapshot:    farmActor.Aggregate.Snapshot(),
			FarmSeq:     clientjson.Uint64(farmActor.Aggregate.FarmSeq),
			ServerTime:  h.now(),
			TimeProfile: h.timeProfiles.Get(),
		}
		if len(changes) > 0 {
			// 进入农场时惰性推进出的成熟/枯萎是权威状态，必须在响应前落盘。
			farmActor.RequireFlush()
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
		return CommandResponse{Err: errcode.Internal}
	}
	h.publishDelta(delta, command.Originator)
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(response)}
}

func (h *Handler) plotAction(command CommandRequest) CommandResponse {
	var request PlotActionRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		(request.OwnerUID != 0 && request.OwnerUID != command.FarmUID) ||
		request.PlotIndex > 255 || request.Arg > 0xFFFF ||
		request.Kind < farm.Till || request.Kind > farm.Harvest {
		return CommandResponse{Err: errcode.BadRequest}
	}

	var result farm.ActionResult
	var response ActionResponse
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		beforeFarmSeq := farmActor.Aggregate.FarmSeq
		result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
			Kind:        request.Kind,
			PlotIndex:   uint8(request.PlotIndex),
			Arg:         uint16(request.Arg),
			TimeProfile: h.timeProfiles.Get(),
		}, h.now())
		if result.Err == errcode.OK {
			response = ActionResponse{
				FarmSeq: clientjson.Uint64(farmActor.Aggregate.FarmSeq),
				Patch:   farmActor.Aggregate.PatchFromAction(result),
			}
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			farmActor.MarkDirty()
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
		return CommandResponse{Err: errcode.Internal}
	}
	if result.Err != errcode.OK {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	if result.Err == errcode.OK {
		if response.Patch.Codex != nil && h.codexRewards != nil {
			rewards, rewardErr := h.codexRewards.IssueCodexRewards(context.Background(), command.FarmUID, *response.Patch.Codex)
			if rewardErr != nil {
				telemetry.L().Error("farmrpc issue codex rewards failed",
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
			telemetry.L().Error("farmrpc advance task failed",
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
			return CommandResponse{Err: errcode.BadRequest}
		}
	}
	var response CodexListResponse
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		response = CodexListResponse{
			Entries: farmActor.Aggregate.CodexSnapshot(),
			Total:   gameconfig.CropCount,
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(response)}
}

func (h *Handler) shop(command CommandRequest) CommandResponse {
	var request ShopRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		request.ItemID > 0xFFFF {
		return CommandResponse{Err: errcode.BadRequest}
	}

	var result farm.ActionResult
	var response ActionResponse
	var delta *farm.FarmDelta
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
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
		if result.Err == errcode.OK {
			// 同 Gateway 侧买卖：金币改动按 A 档同步落盘（架构 5.3 节）。
			farmActor.RequireFlush()
			response = ActionResponse{
				FarmSeq: clientjson.Uint64(farmActor.Aggregate.FarmSeq),
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
		return CommandResponse{Err: errcode.Internal}
	}
	if result.Err != errcode.OK {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	if !request.Buy {
		if err := h.advanceSellTask(command.FarmUID); err != nil {
			telemetry.L().Error("farmrpc advance sell task failed",
				"component", "farmrpc",
				"op", "advance_task",
				"err", err.Error(),
			)
		}
	}
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(response)}
}

func (h *Handler) syncFarm(command CommandRequest) CommandResponse {
	var request SyncFarmRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var response SyncFarmResponse
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		now := h.now()
		changes := farmActor.Aggregate.AdvanceAllWithProfile(now, h.timeProfiles.Get())
		if len(changes) > 0 {
			// SyncFarm 是客户端在风险窗口和成熟点触发的惰性推进屏障。
			farmActor.RequireFlush()
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
		response.FarmSeq = clientjson.Uint64(farmActor.Aggregate.FarmSeq)
		response.ServerTime = now
		response.TimeProfile = h.timeProfiles.Get()
		if request.FromSeq == uint64(response.FarmSeq) {
			return nil
		}
		if request.FromSeq > uint64(response.FarmSeq) {
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
		return CommandResponse{Err: errcode.Internal}
	}
	h.publishDelta(delta, command.Originator)
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(response)}
}

func (h *Handler) pet(command CommandRequest) CommandResponse {
	var request PetRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var result farm.ActionResult
	var status farm.PetStatus
	var delta *farm.FarmDelta
	now := h.now()
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		beforeFarmSeq := farmActor.Aggregate.FarmSeq
		switch request.Kind {
		case PetStatus:
			result.Err = errcode.OK
		case PetActivate:
			result = farmActor.Aggregate.PetActivateWithProfile(request.DogType, now, h.timeProfiles.Get())
		case PetFeed:
			result = farmActor.Aggregate.PetFeedWithProfile(farm.PetFeedReq{Grams: request.Grams}, now, h.timeProfiles.Get())
		default:
			result.Err = errcode.BadRequest
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			farmActor.MarkDirty()
			guardDog := farm.GuardDogSnapshotOf(farmActor.Aggregate.Pet)
			emitted := farm.FarmDelta{
				OwnerUID: command.FarmUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				GuardDog: &guardDog,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
		}
		status = farmActor.Aggregate.PetStatus(now)
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if result.Err != errcode.OK {
		return CommandResponse{Err: result.Err}
	}
	if request.Kind == PetFeed {
		if err := h.advancePetFeedTask(command.FarmUID); err != nil {
			telemetry.L().Error("farmrpc advance pet feed task failed",
				"component", "farmrpc",
				"op", "advance_task",
				"err", err.Error(),
			)
		}
	}
	h.publishDelta(delta, command.Originator)
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(status)}
}

func (h *Handler) crossReserve(command CommandRequest) CommandResponse {
	var reservation crossfarm.VisitorReservation
	if err := decodeJSON(bytes.NewReader(command.Payload), &reservation); err != nil ||
		reservation.Action.VisitorUID != command.FarmUID {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var code errcode.Code
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		now := h.now()
		code = crossfarm.ReserveVisitor(farmActor.Aggregate, reservation, now)
		if code == errcode.OK {
			farmActor.RequireFlush()
		}
		telemetry.L().Debug("farmrpc cross reserve",
			"component", "farmrpc",
			"op", "cross_reserve",
			"code", int(code),
		)
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	return CommandResponse{Err: code}
}

func (h *Handler) crossSettle(command CommandRequest) CommandResponse {
	var result crossfarm.CrossResult
	if err := decodeJSON(bytes.NewReader(command.Payload), &result); err != nil ||
		result.VisitorUID != command.FarmUID {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var response CrossSettleResponse
	var code errcode.Code
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		now := h.now()
		response.Reward, response.PlayerDelta, code = crossfarm.SettleVisitor(farmActor.Aggregate, result, now)
		// 重投看到 Timeout 也可能只是前一次 commit 结果不确定，仍需 durable barrier。
		farmActor.RequireFlush()
		telemetry.L().Debug("farmrpc cross settle",
			"component", "farmrpc",
			"op", "cross_settle",
			"result_code", int(result.Code),
			"settle_code", int(code),
		)
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if response.PlayerDelta != nil {
		h.publishPlayerDelta(command.FarmUID, *response.PlayerDelta)
	}
	return CommandResponse{Err: code, Payload: marshalPayload(response)}
}

func (h *Handler) taskList(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if err := decodeJSON(bytes.NewReader(command.Payload), &struct{}{}); err != nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	now := h.now()
	dayKey := gameconfig.LocalDayKey(now)
	tasks, err := h.taskMail.ListTasks(context.Background(), command.FarmUID, dayKey)
	if err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{
		Err: errcode.OK,
		Payload: marshalPayload(TaskListResponse{
			Tasks:   tasks,
			ResetAt: gameconfig.NextLocalDayResetMs(now),
		}),
	}
}

func (h *Handler) mailList(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if err := decodeJSON(bytes.NewReader(command.Payload), &struct{}{}); err != nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	mails, err := h.taskMail.ListMails(context.Background(), command.FarmUID)
	if err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(MailListResponse{Mails: mails})}
}

func (h *Handler) mailRead(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	var request MailMutationRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		(!request.All && request.MailID == 0) ||
		(request.All && request.MailID != 0) {
		return CommandResponse{Err: errcode.BadRequest}
	}
	affected, err := h.taskMail.MarkMailsRead(context.Background(), command.FarmUID, request.MailID)
	if err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{
		Err:     errcode.OK,
		Payload: marshalPayload(MailMutationResponse{Affected: affected}),
	}
}

func (h *Handler) mailDelete(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	var request MailMutationRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		(!request.All && request.MailID == 0) ||
		(request.All && request.MailID != 0) {
		return CommandResponse{Err: errcode.BadRequest}
	}
	affected, err := h.taskMail.DeleteMails(context.Background(), command.FarmUID, request.MailID)
	if err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{
		Err:     errcode.OK,
		Payload: marshalPayload(MailMutationResponse{Affected: affected}),
	}
}

func (h *Handler) advanceTaskCommand(command CommandRequest) CommandResponse {
	var request AdvanceTaskRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil ||
		request.TaskID == 0 || request.Amount == 0 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	if err := h.advanceTask(command.FarmUID, request.TaskID, request.Amount); err != nil {
		telemetry.L().Error("farmrpc advance task failed",
			"component", "farmrpc",
			"op", "advance_task",
			"uid", command.FarmUID,
			"task_id", request.TaskID,
			"err", err.Error(),
		)
	}
	return CommandResponse{Err: errcode.OK}
}

func (h *Handler) taskClaim(command CommandRequest) CommandResponse {
	var request TaskClaimRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil || request.TaskID == 0 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	return h.claimTask(command, request.TaskID, false)
}

func (h *Handler) claimTask(command CommandRequest, taskID uint32, dailyLoginCompatibility bool) CommandResponse {
	if h.taskClaimer == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	var reward store.TaskReward
	var claimErr error
	var playerDelta farm.PlayerDelta
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		dayKey := gameconfig.LocalDayKey(h.now())
		reward, claimErr = h.taskClaimer.ClaimTask(context.Background(), command.FarmUID, dayKey, taskID)
		if claimErr != nil {
			return nil
		}
		// 低频例外：ClaimTask 在 Actor 锁内先写 MySQL 再同步内存，保证「DB 已增、内存随后一致」。
		// 移出 Actor 需独立 outbox/saga，与跨农场热路径 QPS 无关，本轮保留。
		farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
		farmActor.MarkDirty()
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if claimErr != nil {
		if dailyLoginCompatibility {
			return CommandResponse{Err: dailyLoginErrorCode(claimErr)}
		}
		return CommandResponse{Err: taskClaimErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(reward)}
}

func (h *Handler) dailyLoginClaim(command CommandRequest) CommandResponse {
	if err := decodeJSON(bytes.NewReader(command.Payload), &struct{}{}); err != nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	if h.taskClaimer != nil {
		return h.claimTask(command, store.TaskDailyLoginID, true)
	}
	if h.dailyLoginClaimer == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	var reward store.TaskReward
	var claimErr error
	var playerDelta farm.PlayerDelta
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		dayKey := gameconfig.LocalDayKey(h.now())
		reward, claimErr = h.dailyLoginClaimer.ClaimDailyLogin(context.Background(), command.FarmUID, dayKey)
		if claimErr != nil {
			return nil
		}
		farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
		farmActor.MarkDirty()
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if claimErr != nil {
		return CommandResponse{Err: dailyLoginErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(reward)}
}

func (h *Handler) mailClaim(command CommandRequest) CommandResponse {
	if h.mailClaimer == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	var request MailClaimRequest
	if err := decodeJSON(bytes.NewReader(command.Payload), &request); err != nil || request.MailID == 0 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var mail store.Mail
	var playerDelta farm.PlayerDelta
	var claimErr error
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		mail, claimErr = h.mailClaimer.ClaimMail(context.Background(), command.FarmUID, request.MailID)
		if claimErr != nil {
			return nil
		}
		// 低频例外：ClaimMail 在 Actor 锁内先写 MySQL 再同步内存，保证附件入账一致。
		farmActor.Aggregate.CreditMailReward(mail.AttachmentCoin)
		farmActor.MarkDirty()
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if claimErr != nil {
		return CommandResponse{Err: mailClaimErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: errcode.OK, Payload: marshalPayload(mail)}
}

func taskClaimErrorCode(err error) errcode.Code {
	switch {
	case errors.Is(err, store.ErrTaskNotComplete):
		return errcode.TaskNotComplete
	case errors.Is(err, store.ErrTaskAlreadyClaimed):
		return errcode.TaskAlreadyClaimed
	case errors.Is(err, store.ErrDailyLoginAlreadyClaimed):
		return errcode.DuplicateOK
	default:
		return errcode.Internal
	}
}

func dailyLoginErrorCode(err error) errcode.Code {
	if errors.Is(err, store.ErrTaskAlreadyClaimed) || errors.Is(err, store.ErrDailyLoginAlreadyClaimed) {
		return errcode.DuplicateOK
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
	case farm.Water:
		taskID = store.TaskWaterID
	case farm.Fertilize:
		taskID = store.TaskFertilizeID
	case farm.Till:
		taskID = store.TaskTillID
	case farm.Weed:
		taskID = store.TaskWeedID
	case farm.Pest:
		taskID = store.TaskPestID
	default:
		return nil
	}
	dayKey := gameconfig.LocalDayKey(h.now())
	result, err := h.taskProgress.AdvanceTask(context.Background(), uid, dayKey, taskID, 1)
	if err != nil {
		return err
	}
	if result.Changed {
		h.publishTaskNotify(uid, result.Task)
	}
	return nil
}

func (h *Handler) advanceSellTask(uid uint64) error {
	return h.advanceTask(uid, store.TaskSellID, 1)
}

func (h *Handler) advancePetFeedTask(uid uint64) error {
	return h.advanceTask(uid, store.TaskFeedDogID, 1)
}

func taskListErrorCode(err error) errcode.Code {
	if err != nil {
		return errcode.Internal
	}
	return errcode.OK
}

func (h *Handler) advanceTask(uid uint64, taskID, amount uint32) error {
	if h.taskProgress == nil {
		return nil
	}
	result, err := h.taskProgress.AdvanceTask(
		context.Background(), uid, gameconfig.LocalDayKey(h.now()), taskID, amount,
	)
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
	if err := h.taskNotifyPublisher.PublishTaskNotify(context.Background(), uid, task); err != nil {
		telemetry.L().Error("farmrpc TaskNotify publish failed",
			"component", "farmrpc",
			"op", "publish_task_notify",
			"uid", uid,
			"task_id", task.ID,
			"err", err.Error(),
		)
	}
}

func (h *Handler) publishMailNotify(uid uint64, kind string) {
	if h.mailNotifyPublisher == nil || uid == 0 {
		return
	}
	publisher := h.mailNotifyPublisher
	go func() {
		if err := publisher.PublishMailNotify(context.Background(), uid, kind); err != nil {
			telemetry.L().Error("farmrpc MailNotify publish failed",
				"component", "farmrpc",
				"op", "publish_mail_notify",
				"uid", uid,
				"kind", kind,
				"err", err.Error(),
			)
		}
	}()
}

func mailClaimErrorCode(err error) errcode.Code {
	switch {
	case errors.Is(err, store.ErrMailNotFound):
		return errcode.MailNotFound
	case errors.Is(err, store.ErrMailNoAttachment):
		return errcode.MailNoAttachment
	case errors.Is(err, store.ErrMailAlreadyClaimed):
		return errcode.MailAlreadyClaimed
	default:
		return errcode.Internal
	}
}

func (h *Handler) publishDelta(delta *farm.FarmDelta, originator presence.ConnRef) {
	if delta == nil || h.deltaPublisher == nil {
		return
	}
	publisher := h.deltaPublisher
	emitted := *delta
	go func() {
		// Delta delivery is best effort; SyncFarm recovers a missed callback.
		if err := publisher.Publish(context.Background(), emitted, originator); err != nil {
			telemetry.L().Error("farmrpc delta publish failed",
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

func marshalPayload(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}
