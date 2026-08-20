// Package farmrpc implements the authenticated, internal Farm command boundary.
package farmrpc

import (
	"context"
	"errors"
	"sync"
	"time"

	"farm/server/domain/farm"
	"farm/server/farmsvr/room"
	publicv3 "farm/server/gen/farm/public/v3"
	"farm/server/shared/clientwire"
	"farm/server/shared/errcode"
	"farm/server/shared/gameconfig"
	"farm/server/shared/outbox"
	"farm/server/shared/presence"
	"farm/server/shared/store"
	"farm/server/shared/telemetry"
)

// Operation identifies an in-process Farm application operation. The external
// RPC boundary is the typed ClientCommandRequest Protobuf contract.
type Operation string

const (
	OperationEnterFarm  Operation = "enter_farm"
	OperationPlotAction Operation = "plot_action"
	OperationShop       Operation = "shop"
	OperationSyncFarm   Operation = "sync_farm"
	OperationPet        Operation = "pet"
	OperationTaskList   Operation = "task_list"
	OperationTaskClaim  Operation = "task_claim"
	OperationDailyLogin Operation = "daily_login_claim"
	OperationMailList   Operation = "mail_list"
	OperationMailRead   Operation = "mail_read"
	OperationMailDelete Operation = "mail_delete"
	OperationMailClaim  Operation = "mail_claim"
	OperationCodexList  Operation = "codex_list"
)

// CommandRequest is Farm's typed in-process application command. The public
// request remains Protobuf from Gateway through ClientHandler to this layer.
type CommandRequest struct {
	Context        context.Context
	Operation      Operation
	FarmUID        uint64
	Originator     presence.ConnRef
	PreferPrepared bool
	ClientCommand  uint32
	ClientRequest  *publicv3.CommandRequest
	SyncRequest    *publicv3.SyncFarmRequest
}

// CommandResponse preserves protocol-level errors inside a successful internal
// request so a Gateway can return the same error code to its client.
type CommandResponse struct {
	Err               errcode.Code
	FarmSeq           uint64
	PreparedPayload   []byte
	PreparedField     uint32
	SyncCaughtUp      bool
	SyncServerTime    int64
	SyncTimeProfile   string
	EnterFarmResponse *publicv3.EnterFarmResponse
	SyncFarmResponse  *publicv3.SyncFarmResponse
	ClientResponse    *publicv3.CommandResponse
}

// syncFarmExecution keeps the many values shared with the Actor callback in
// one reusable object. The callback crosses a goroutine boundary, so capturing
// each local separately made a large closure escape on every hot SyncFarm.
type syncFarmExecution struct {
	farmUID          uint64
	fromSeq          uint64
	preferPrepared   bool
	deltas           []farm.FarmDelta
	farmSeq          uint64
	serverTime       int64
	timeProfile      string
	snapshotProto    *publicv3.FarmSnapshot
	snapshotPayload  []byte
	delta            *farm.FarmDelta
	stealable        bool
	refreshStealHint bool
}

var syncFarmExecutionPool = sync.Pool{New: func() any { return new(syncFarmExecution) }}

// PlotActionRequest carries one owner-authoritative plot mutation.
type plotActionRequest struct {
	OwnerUID  uint64
	PlotIndex uint32
	Arg       uint32
	Kind      farm.PlotActionKind
	Command   uint32
}

// ShopRequest carries one owner-authoritative Buy or Sell.
type shopRequest struct {
	Buy      bool
	ItemID   uint32
	Quantity uint32
	Command  uint32
}

type actionCommandResult struct {
	FarmSeq      uint64
	Patch        farm.PatchJSON
	CodexRewards []farm.CodexRewardNotice
}

// PetOperation identifies the local pet mutation.
type PetOperation string

const (
	PetStatus   PetOperation = "status"
	PetActivate PetOperation = "activate"
	PetFeed     PetOperation = "feed"
)

// Runtime is the minimal Actor boundary needed by this RPC handler.
type Runtime interface {
	Do(uid uint64, fn func(*room.FarmActor) error) error
}

type residentRuntime interface {
	Runtime
	IsResident(uid uint64) bool
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
	bundleJournalEffects bool
	advanceScheduler     *farmAdvanceScheduler
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

// taskStateClaimer persists the Actor-authoritative post-claim economy in the
// same transaction as the task marker. Journal-backed production storage uses
// it to avoid a full projection barrier; simple test/legacy stores retain the
// TaskClaimer fallback above.
type taskStateClaimer interface {
	ClaimTaskAtState(
		ctx context.Context,
		uid uint64,
		dayKey int64,
		taskID uint32,
		state store.DirectClaimState,
	) (store.TaskReward, error)
}

// DailyLoginClaimer atomically records and credits the daily login reward.
type DailyLoginClaimer interface {
	ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (store.TaskReward, error)
}

// MailClaimer atomically marks and credits a mail attachment in durable storage.
type MailClaimer interface {
	ClaimMail(ctx context.Context, uid uint64, mailID uint64) (store.Mail, error)
}

type mailStateClaimer interface {
	ClaimMailAtState(
		ctx context.Context,
		uid uint64,
		mailID uint64,
		state store.DirectClaimState,
	) (store.Mail, error)
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

// WithBundledJournalSideEffects puts task/codex side effects in the same
// durable Farm mutation record. Enable only when the configured Farm store is
// backed by FarmWriteJournal; transactional test stores keep the legacy path.
func WithBundledJournalSideEffects() Option {
	return func(handler *Handler) {
		handler.bundleJournalEffects = true
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
	handler.advanceScheduler = newFarmAdvanceScheduler(handler.now, handler.advanceScheduled)
	return handler
}

// Shutdown stops the process-local farm timing heap. Runtime shutdown remains
// owned by the Farm process bootstrap.
func (h *Handler) Shutdown() {
	if h != nil && h.advanceScheduler != nil {
		h.advanceScheduler.Close()
	}
}

func (h *Handler) scheduleAdvance(uid uint64, aggregate *farm.Aggregate) {
	if h == nil || h.advanceScheduler == nil || aggregate == nil {
		return
	}
	h.advanceScheduler.Schedule(uid, aggregate.NextAdvanceAt(h.now()))
}

// advanceScheduled performs the same authoritative transition as SyncFarm.
// A resident Actor is used directly. If idle eviction already unloaded it, the
// shared room lease decides whether it should be reconstructed for an online
// subscriber; offline farms stay lazy and advance on their next EnterFarm.
func (h *Handler) advanceScheduled(uid uint64) {
	if h == nil || h.runtime == nil || uid == 0 {
		return
	}
	resident, canCheckResident := h.runtime.(residentRuntime)
	if canCheckResident && !resident.IsResident(uid) {
		checker, ok := h.deltaPublisher.(ActiveFarmChecker)
		if ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			active, err := checker.HasActiveFarm(ctx, uid)
			cancel()
			if err != nil {
				h.advanceScheduler.Schedule(uid, h.now()+advanceRetryDelay.Milliseconds())
				return
			}
			if !active {
				return
			}
		}
	}

	var delta *farm.FarmDelta
	var stealable bool
	var next int64
	err := h.runtime.Do(uid, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		now := h.now()
		changes := farmActor.Aggregate.AdvanceAllWithProfile(now, h.timeProfiles.Get())
		if len(changes) > 0 {
			farmActor.RequireFlush()
			emitted := farm.FarmDelta{
				OwnerUID: uid,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    changes,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
			stealable = farmActor.Aggregate.HasStealable()
		}
		next = farmActor.Aggregate.NextAdvanceAt(now)
		return nil
	})
	if err != nil {
		h.advanceScheduler.Schedule(uid, h.now()+advanceRetryDelay.Milliseconds())
		return
	}
	h.advanceScheduler.Schedule(uid, next)
	h.publishDelta(delta, presence.ConnRef{})
	if delta != nil {
		h.writeStealHint(uid, stealable)
	}
}

// ScheduleAdvanceAt lets other authoritative Farm write paths refresh the same
// process-local boundary timer using a deadline computed inside their Actor.
func (h *Handler) ScheduleAdvanceAt(uid uint64, due int64) {
	if h != nil && h.advanceScheduler != nil {
		h.advanceScheduler.Schedule(uid, due)
	}
}

// Execute runs one Farm-validated command against the local runtime.
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
	case OperationTaskList:
		return h.taskList(request)
	case OperationTaskClaim:
		return h.taskClaim(request)
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
	var snapshotProto *publicv3.FarmSnapshot
	var snapshotPayload []byte
	var farmSeq uint64
	var serverTime int64
	var timeProfile string
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		serverTime = h.now()
		timeProfile = h.timeProfiles.Get()
		changes := farmActor.Aggregate.AdvanceAllWithProfile(serverTime, timeProfile)
		h.scheduleAdvance(command.FarmUID, farmActor.Aggregate)
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
		var err error
		if command.PreferPrepared {
			snapshotPayload, err = farmActor.EncodedSnapshotProto()
		} else {
			snapshotProto, err = farmActor.SnapshotProto()
		}
		if err != nil {
			return err
		}
		farmSeq = farmActor.Aggregate.FarmSeq
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	h.publishDelta(delta, command.Originator)
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	if command.PreferPrepared {
		prepared, err := clientwire.MarshalEnterFarmResponsePayload(
			snapshotPayload, farmSeq, serverTime, timeProfile, false, "SELF",
		)
		if err != nil {
			return CommandResponse{Err: errcode.Internal}
		}
		return CommandResponse{
			Err: errcode.OK, FarmSeq: farmSeq,
			PreparedPayload: prepared, PreparedField: clientwire.PreparedEnterFarmResponse,
		}
	}
	return CommandResponse{
		Err: errcode.OK, FarmSeq: farmSeq,
		EnterFarmResponse: &publicv3.EnterFarmResponse{
			Snapshot: snapshotProto, FarmSeq: farmSeq, ServerTime: serverTime,
			TimeProfile: timeProfile,
		},
	}
}

func (h *Handler) plotAction(command CommandRequest) CommandResponse {
	if command.ClientRequest == nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	kind, ok := plotActionKindForCommand(command.ClientCommand)
	if !ok {
		return CommandResponse{Err: errcode.BadRequest}
	}
	request := plotActionRequest{
		OwnerUID: command.ClientRequest.OwnerUid, PlotIndex: command.ClientRequest.PlotIndex,
		Arg: command.ClientRequest.Arg, Kind: kind, Command: command.ClientCommand,
	}
	if (request.OwnerUID != 0 && request.OwnerUID != command.FarmUID) ||
		request.PlotIndex > 255 || request.Arg > 0xFFFF ||
		request.Kind < farm.Till || request.Kind > farm.Harvest {
		return CommandResponse{Err: errcode.BadRequest}
	}

	var result farm.ActionResult
	var response actionCommandResult
	var delta *farm.FarmDelta
	var stealable bool
	var refreshHint bool
	var taskNotify *store.Task
	taskID := gameplayTaskID(request.Kind)
	dayKey := gameconfig.LocalDayKey(h.now())
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		if h.bundleJournalEffects && taskID != 0 {
			if err := h.ensureActorTasks(commandContext(command), command.FarmUID, dayKey, farmActor); err != nil {
				return err
			}
		}
		beforeFarmSeq := farmActor.Aggregate.FarmSeq
		result = farmActor.Aggregate.ApplyPlotAction(farm.PlotAction{
			Kind:        request.Kind,
			PlotIndex:   uint8(request.PlotIndex),
			Arg:         uint16(request.Arg),
			TimeProfile: h.timeProfiles.Get(),
		}, h.now())
		if result.Err == errcode.OK {
			response = actionCommandResult{
				FarmSeq: farmActor.Aggregate.FarmSeq,
				Patch:   farmActor.Aggregate.PatchFromAction(result),
			}
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			includeItems := len(result.Patch.Items) != 0
			farmActor.RecordItemCounts(result.Patch.Items)
			farmActor.MarkPlotDirty(
				uint8(request.PlotIndex), includeItems, request.Kind == farm.Harvest,
			)
			plots := make([]farm.PlotChange, 0, 1)
			if response.Patch.Plot != nil {
				// Reuse the one authoritative plot projection produced for the
				// response instead of projecting the same Aggregate row again.
				plots = append(plots, farm.PlotChange(*response.Patch.Plot))
			}
			emitted := farm.FarmDelta{
				OwnerUID: command.FarmUID,
				FarmSeq:  farmActor.Aggregate.FarmSeq,
				Plots:    plots,
				ActorUID: command.FarmUID,
				Action:   request.Command,
			}
			farmActor.Deltas.Append(emitted)
			delta = &emitted
			// Water/weed/pest/fertilize cannot change whether a mature crop is
			// stealable. Only Harvest can remove that eligibility on this local
			// action path; maturity transitions are handled by the scheduler.
			if request.Kind == farm.Harvest {
				stealable = farmActor.Aggregate.HasStealable()
				refreshHint = true
			}
		}
		if result.Err == errcode.OK && h.bundleJournalEffects {
			if taskID != 0 && h.taskProgress != nil {
				advanced, _ := farmActor.AdvanceTaskState(dayKey, taskID, 1)
				if advanced.Changed {
					farmActor.RecordTaskAdvance(outbox.TaskAdvance{DayKey: dayKey, TaskID: taskID, Amount: 1})
					copy := advanced.Task
					taskNotify = &copy
				}
			}
			if response.Patch.Codex != nil && h.codexRewards != nil {
				farmActor.RecordCodexChange(response.Patch.Codex.CropID)
				farmActor.RecordCodexReward(*response.Patch.Codex)
				response.CodexRewards = store.PreviewCodexRewardNotices(*response.Patch.Codex)
			}
		}
		h.scheduleAdvance(command.FarmUID, farmActor.Aggregate)
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if result.Err != errcode.OK {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	if taskNotify != nil {
		h.publishTaskNotify(command.FarmUID, *taskNotify)
	}
	if refreshHint {
		h.writeStealHint(command.FarmUID, stealable)
	}
	if result.Err == errcode.OK && !h.bundleJournalEffects {
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
	return CommandResponse{
		Err: result.Err, FarmSeq: response.FarmSeq,
		ClientResponse: clientwire.NewActionCommandResponse(
			response.FarmSeq, response.Patch, response.CodexRewards,
		),
	}
}

func (h *Handler) codexList(command CommandRequest) CommandResponse {
	if command.ClientRequest == nil || command.ClientCommand != 612 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var entries []farm.CodexProgress
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		entries = farmActor.Aggregate.CodexSnapshot()
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	return CommandResponse{
		Err:            errcode.OK,
		ClientResponse: clientwire.NewCodexListCommandResponse(entries, uint32(gameconfig.CropCount)),
	}
}

func (h *Handler) shop(command CommandRequest) CommandResponse {
	if command.ClientRequest == nil || command.ClientCommand != 302 && command.ClientCommand != 304 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	request := shopRequest{
		Buy: command.ClientCommand == 302, ItemID: command.ClientRequest.ItemId,
		Quantity: command.ClientRequest.Quantity, Command: command.ClientCommand,
	}
	if request.ItemID > 0xFFFF {
		return CommandResponse{Err: errcode.BadRequest}
	}

	var result farm.ActionResult
	var response actionCommandResult
	var delta *farm.FarmDelta
	var taskNotify *store.Task
	dayKey := gameconfig.LocalDayKey(h.now())
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		if !request.Buy && h.bundleJournalEffects {
			if err := h.ensureActorTasks(commandContext(command), command.FarmUID, dayKey, farmActor); err != nil {
				return err
			}
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
			// The committer preserves per-UID ordering and batches this reduced
			// economy write with other shop operations.
			farmActor.RequireEconomyFlush()
			farmActor.RecordItemCounts(result.Patch.Items)
			if !request.Buy && h.bundleJournalEffects && h.taskProgress != nil {
				advanced, _ := farmActor.AdvanceTaskState(dayKey, store.TaskSellID, 1)
				if advanced.Changed {
					farmActor.RecordTaskAdvance(outbox.TaskAdvance{DayKey: dayKey, TaskID: store.TaskSellID, Amount: 1})
					copy := advanced.Task
					taskNotify = &copy
				}
			}
			response = actionCommandResult{
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
		return CommandResponse{Err: errcode.Internal}
	}
	if result.Err != errcode.OK {
		return CommandResponse{Err: result.Err}
	}
	h.publishDelta(delta, command.Originator)
	if taskNotify != nil {
		h.publishTaskNotify(command.FarmUID, *taskNotify)
	}
	if !request.Buy && !h.bundleJournalEffects {
		if err := h.advanceSellTask(command.FarmUID); err != nil {
			telemetry.L().Error("farmrpc advance sell task failed",
				"component", "farmrpc",
				"op", "advance_task",
				"err", err.Error(),
			)
		}
	}
	return CommandResponse{
		Err: errcode.OK, FarmSeq: response.FarmSeq,
		ClientResponse: clientwire.NewActionCommandResponse(
			response.FarmSeq, response.Patch, response.CodexRewards,
		),
	}
}

func plotActionKindForCommand(command uint32) (farm.PlotActionKind, bool) {
	switch command {
	case 206:
		return farm.Till, true
	case 208:
		return farm.Clear, true
	case 210:
		return farm.Plant, true
	case 212:
		return farm.Water, true
	case 214:
		return farm.Weed, true
	case 216:
		return farm.Pest, true
	case 218:
		return farm.Fertilize, true
	case 220:
		return farm.Harvest, true
	default:
		return 0, false
	}
}

func (h *Handler) syncFarm(command CommandRequest) CommandResponse {
	if command.SyncRequest == nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	return h.syncFarmFromSeq(command, command.SyncRequest.FromSeq, false)
}

// syncFarmPreparedSelf is used only by the trusted flat Gateway/Farm stream
// for a self SyncFarm. It preserves the general command path as a fallback but
// lets a caught-up response remain scalar until it reaches Gateway.
func (h *Handler) syncFarmPreparedSelf(
	ctx context.Context,
	farmUID uint64,
	originator presence.ConnRef,
	fromSeq uint64,
) CommandResponse {
	return h.syncFarmFromSeq(CommandRequest{
		Context:   ctx,
		Operation: OperationSyncFarm, FarmUID: farmUID,
		Originator: originator, PreferPrepared: true,
	}, fromSeq, true)
}

func (h *Handler) syncFarmFromSeq(
	command CommandRequest,
	fromSeq uint64,
	deferCaughtUpPayload bool,
) CommandResponse {
	execution := syncFarmExecutionPool.Get().(*syncFarmExecution)
	*execution = syncFarmExecution{
		farmUID: command.FarmUID, fromSeq: fromSeq,
		preferPrepared: command.PreferPrepared,
	}
	defer func() {
		*execution = syncFarmExecution{}
		syncFarmExecutionPool.Put(execution)
	}()

	if err := h.runtime.Do(execution.farmUID, func(farmActor *room.FarmActor) error {
		return h.executeSyncFarmActor(execution, farmActor)
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	h.publishDelta(execution.delta, command.Originator)
	if execution.refreshStealHint {
		h.writeStealHint(execution.farmUID, execution.stealable)
	}
	if command.PreferPrepared {
		// A caught-up SyncFarm is the dominant hot path. Do not allocate and then
		// discard a complete typed response merely to discover that it has neither
		// a snapshot nor deltas.
		if len(execution.snapshotPayload) == 0 && len(execution.deltas) == 0 {
			if deferCaughtUpPayload {
				return CommandResponse{
					Err: errcode.OK, FarmSeq: execution.farmSeq,
					PreparedField:   clientwire.PreparedSyncFarmResponse,
					SyncCaughtUp:    true,
					SyncServerTime:  execution.serverTime,
					SyncTimeProfile: execution.timeProfile,
				}
			}
			return CommandResponse{
				Err: errcode.OK, FarmSeq: execution.farmSeq,
				PreparedPayload: clientwire.MarshalSyncFarmCaughtUpPayload(
					execution.farmSeq, execution.serverTime, execution.timeProfile, false,
				),
				PreparedField: clientwire.PreparedSyncFarmResponse,
			}
		}
	}
	typed := &publicv3.SyncFarmResponse{
		Snapshot: execution.snapshotProto, FarmSeq: execution.farmSeq,
		ServerTime: execution.serverTime, TimeProfile: execution.timeProfile,
	}
	for _, item := range execution.deltas {
		typed.Deltas = append(typed.Deltas, clientwire.FarmDeltaToProto(item))
	}
	if command.PreferPrepared {
		var prepared []byte
		var err error
		switch {
		case len(execution.snapshotPayload) != 0:
			prepared, err = clientwire.MarshalSyncFarmSnapshotPayload(
				execution.snapshotPayload, execution.farmSeq,
				execution.serverTime, execution.timeProfile, false,
			)
		default:
			prepared, err = clientwire.MarshalSyncFarmResponsePayload(typed)
		}
		if err != nil {
			return CommandResponse{Err: errcode.Internal}
		}
		return CommandResponse{
			Err: errcode.OK, FarmSeq: execution.farmSeq,
			PreparedPayload: prepared, PreparedField: clientwire.PreparedSyncFarmResponse,
		}
	}
	return CommandResponse{Err: errcode.OK, FarmSeq: execution.farmSeq, SyncFarmResponse: typed}
}

func (h *Handler) executeSyncFarmActor(
	execution *syncFarmExecution,
	farmActor *room.FarmActor,
) error {
	if farmActor == nil || farmActor.Aggregate == nil {
		return errors.New("farmrpc: actor aggregate is nil")
	}
	now := h.now()
	profile := h.timeProfiles.Get()
	changes := farmActor.Aggregate.AdvanceAllWithProfile(now, profile)
	if len(changes) > 0 {
		// SyncFarm 是客户端在风险窗口和成熟点触发的惰性推进屏障。
		farmActor.RequireFlush()
		emitted := farm.FarmDelta{
			OwnerUID: execution.farmUID,
			FarmSeq:  farmActor.Aggregate.FarmSeq,
			Plots:    changes,
		}
		farmActor.Deltas.Append(emitted)
		execution.delta = &emitted
		execution.stealable = farmActor.Aggregate.HasStealable()
		execution.refreshStealHint = true
	}
	execution.farmSeq = farmActor.Aggregate.FarmSeq
	execution.serverTime = now
	execution.timeProfile = profile
	if execution.fromSeq == execution.farmSeq {
		h.scheduleAdvance(execution.farmUID, farmActor.Aggregate)
		return nil
	}
	if execution.fromSeq > execution.farmSeq {
		if err := execution.captureSyncSnapshot(farmActor); err != nil {
			return err
		}
		h.scheduleAdvance(execution.farmUID, farmActor.Aggregate)
		return nil
	}
	var ok bool
	execution.deltas, ok = farmActor.Deltas.Since(execution.fromSeq + 1)
	if !ok || len(execution.deltas) == 0 {
		if err := execution.captureSyncSnapshot(farmActor); err != nil {
			return err
		}
	}
	h.scheduleAdvance(execution.farmUID, farmActor.Aggregate)
	return nil
}

func (execution *syncFarmExecution) captureSyncSnapshot(farmActor *room.FarmActor) error {
	var err error
	if execution.preferPrepared {
		execution.snapshotPayload, err = farmActor.EncodedSnapshotProto()
	} else {
		execution.snapshotProto, err = farmActor.SnapshotProto()
	}
	return err
}

func (h *Handler) pet(command CommandRequest) CommandResponse {
	if command.ClientRequest == nil {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var kind PetOperation
	var dogType farm.DogType
	var grams uint32
	switch command.ClientCommand {
	case 500:
		kind = PetStatus
	case 502:
		kind, dogType = PetActivate, farm.DogType(command.ClientRequest.DogType)
	case 504:
		kind, grams = PetFeed, command.ClientRequest.Grams
	default:
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
		switch kind {
		case PetStatus:
			result.Err = errcode.OK
		case PetActivate:
			result = farmActor.Aggregate.PetActivateWithProfile(dogType, now, h.timeProfiles.Get())
		case PetFeed:
			result = farmActor.Aggregate.PetFeedWithProfile(farm.PetFeedReq{Grams: grams}, now, h.timeProfiles.Get())
		default:
			result.Err = errcode.BadRequest
		}
		if farmActor.Aggregate.FarmSeq != beforeFarmSeq {
			farmActor.RecordItemCounts(result.Patch.Items)
			farmActor.RequireEconomyFlush()
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
	if kind == PetFeed {
		if err := h.advancePetFeedTask(command.FarmUID); err != nil {
			telemetry.L().Error("farmrpc advance pet feed task failed",
				"component", "farmrpc",
				"op", "advance_task",
				"err", err.Error(),
			)
		}
	}
	h.publishDelta(delta, command.Originator)
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewPetCommandResponse(status)}
}

func (h *Handler) taskList(command CommandRequest) CommandResponse {
	if h.taskMail == nil || command.ClientRequest == nil || command.ClientCommand != 600 {
		return CommandResponse{Err: errcode.Internal}
	}
	now := h.now()
	dayKey := gameconfig.LocalDayKey(now)
	var tasks []store.Task
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if err := h.ensureActorTasks(commandContext(command), command.FarmUID, dayKey, farmActor); err != nil {
			return err
		}
		tasks = farmActor.TaskSnapshot(dayKey)
		return nil
	}); err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{
		Err:            errcode.OK,
		ClientResponse: clientwire.NewTaskListCommandResponse(tasks, gameconfig.NextLocalDayResetMs(now)),
	}
}

func (h *Handler) mailList(command CommandRequest) CommandResponse {
	if h.taskMail == nil || command.ClientRequest == nil || command.ClientCommand != 604 {
		return CommandResponse{Err: errcode.Internal}
	}
	var mails []store.Mail
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if err := h.refreshActorMails(commandContext(command), command.FarmUID, farmActor); err != nil {
			return err
		}
		mails = farmActor.MailSnapshot()
		return nil
	}); err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewMailListCommandResponse(mails)}
}

func (h *Handler) mailRead(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if command.ClientRequest == nil || command.ClientCommand != 606 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	mailID, all := command.ClientRequest.MailId, command.ClientRequest.All
	if (!all && mailID == 0) || (all && mailID != 0) {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var affected int64
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if err := h.ensureActorMails(commandContext(command), command.FarmUID, farmActor); err != nil {
			return err
		}
		affected = farmActor.MarkMailsReadState(mailID, h.now())
		if affected > 0 {
			farmActor.RequireSideEffectFlush()
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewMailMutationCommandResponse(affected)}
}

func (h *Handler) mailDelete(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if command.ClientRequest == nil || command.ClientCommand != 610 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	mailID, all := command.ClientRequest.MailId, command.ClientRequest.All
	if (!all && mailID == 0) || (all && mailID != 0) {
		return CommandResponse{Err: errcode.BadRequest}
	}
	var affected int64
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if err := h.ensureActorMails(commandContext(command), command.FarmUID, farmActor); err != nil {
			return err
		}
		affected = farmActor.DeleteMailsState(mailID, h.now())
		if affected > 0 {
			farmActor.RequireSideEffectFlush()
		}
		return nil
	}); err != nil {
		return CommandResponse{Err: taskListErrorCode(err)}
	}
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewMailMutationCommandResponse(affected)}
}

func (h *Handler) taskClaim(command CommandRequest) CommandResponse {
	if command.ClientRequest == nil || command.ClientCommand != 602 || command.ClientRequest.TaskId == 0 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	return h.claimTask(command, command.ClientRequest.TaskId, false)
}

func (h *Handler) claimTask(command CommandRequest, taskID uint32, dailyLoginCompatibility bool) CommandResponse {
	if h.taskMail == nil {
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
		if err := h.ensureActorTasks(commandContext(command), command.FarmUID, dayKey, farmActor); err != nil {
			return err
		}
		reward, claimErr = farmActor.ClaimTaskState(dayKey, taskID, h.now())
		if claimErr != nil {
			return nil
		}
		if !farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp) {
			return errors.New("farmrpc: task reward did not change aggregate")
		}
		farmActor.RequireEconomyFlush()
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
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewTaskRewardCommandResponse(reward)}
}

func (h *Handler) dailyLoginClaim(command CommandRequest) CommandResponse {
	if command.ClientRequest == nil || command.ClientCommand != 614 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	if h.taskMail != nil {
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
		reward, claimErr = h.dailyLoginClaimer.ClaimDailyLogin(commandContext(command), command.FarmUID, dayKey)
		if claimErr != nil {
			return nil
		}
		farmActor.Aggregate.CreditReward(reward.Coin, reward.Exp)
		farmActor.RequireFlush()
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if claimErr != nil {
		return CommandResponse{Err: dailyLoginErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewTaskRewardCommandResponse(reward)}
}

func (h *Handler) mailClaim(command CommandRequest) CommandResponse {
	if h.taskMail == nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if command.ClientRequest == nil || command.ClientCommand != 608 || command.ClientRequest.MailId == 0 {
		return CommandResponse{Err: errcode.BadRequest}
	}
	mailID := command.ClientRequest.MailId
	var mail store.Mail
	var playerDelta farm.PlayerDelta
	var claimErr error
	if err := h.runtime.Do(command.FarmUID, func(farmActor *room.FarmActor) error {
		if farmActor == nil || farmActor.Aggregate == nil {
			return errors.New("farmrpc: actor aggregate is nil")
		}
		if err := h.ensureActorMailForClaim(commandContext(command), command.FarmUID, mailID, farmActor); err != nil {
			return err
		}
		mail, claimErr = farmActor.ClaimMailState(mailID, h.now())
		if claimErr != nil {
			return nil
		}
		if !farmActor.Aggregate.CreditMailReward(mail.AttachmentCoin) {
			return errors.New("farmrpc: mail reward did not change aggregate")
		}
		farmActor.RequireEconomyFlush()
		playerDelta = farmActor.Aggregate.PlayerDelta()
		return nil
	}); err != nil {
		return CommandResponse{Err: errcode.Internal}
	}
	if claimErr != nil {
		return CommandResponse{Err: mailClaimErrorCode(claimErr)}
	}
	h.publishPlayerDelta(command.FarmUID, playerDelta)
	return CommandResponse{Err: errcode.OK, ClientResponse: clientwire.NewMailClaimCommandResponse(mail)}
}

type mailByIDReader interface {
	GetMail(ctx context.Context, uid, mailID uint64) (store.Mail, error)
}

func (h *Handler) ensureActorMailForClaim(
	ctx context.Context,
	uid, mailID uint64,
	farmActor *room.FarmActor,
) error {
	if farmActor == nil || farmActor.Aggregate == nil || h.taskMail == nil {
		return errors.New("farmrpc: mail Actor state is unavailable")
	}
	if farmActor.MailsReady() || farmActor.HasMail(mailID) {
		return nil
	}
	reader, ok := h.taskMail.(mailByIDReader)
	if !ok {
		return h.ensureActorMails(ctx, uid, farmActor)
	}
	mail, err := reader.GetMail(ctx, uid, mailID)
	if err != nil {
		return err
	}
	farmActor.LoadMailForClaim(mail)
	return nil
}

func (h *Handler) ensureActorTasks(
	ctx context.Context,
	uid uint64,
	dayKey int64,
	farmActor *room.FarmActor,
) error {
	if farmActor == nil || farmActor.Aggregate == nil || h.taskMail == nil {
		return errors.New("farmrpc: task Actor state is unavailable")
	}
	if farmActor.TasksReady(dayKey) {
		return nil
	}
	tasks, err := h.taskMail.ListTasks(ctx, uid, dayKey)
	if err != nil {
		return err
	}
	farmActor.LoadTasks(dayKey, tasks)
	return nil
}

func (h *Handler) ensureActorMails(ctx context.Context, uid uint64, farmActor *room.FarmActor) error {
	if farmActor == nil || farmActor.Aggregate == nil || h.taskMail == nil {
		return errors.New("farmrpc: mail Actor state is unavailable")
	}
	if farmActor.MailsReady() {
		return nil
	}
	mails, err := h.taskMail.ListMails(ctx, uid)
	if err != nil {
		return err
	}
	farmActor.LoadMails(mails)
	return nil
}

func (h *Handler) refreshActorMails(ctx context.Context, uid uint64, farmActor *room.FarmActor) error {
	if farmActor == nil || farmActor.Aggregate == nil || h.taskMail == nil {
		return errors.New("farmrpc: mail Actor state is unavailable")
	}
	mails, err := h.taskMail.ListMails(ctx, uid)
	if err != nil {
		return err
	}
	farmActor.MergeMails(mails)
	return nil
}

func nextDirectClaimState(aggregate *farm.Aggregate) (store.DirectClaimState, error) {
	if aggregate == nil || aggregate.FarmSeq == ^uint64(0) {
		return store.DirectClaimState{}, errors.New("farmrpc: invalid direct claim aggregate sequence")
	}
	return store.DirectClaimState{Coin: aggregate.Coin, NextFarmSeq: aggregate.FarmSeq + 1}, nil
}

func commandContext(command CommandRequest) context.Context {
	if command.Context != nil {
		return command.Context
	}
	return context.Background()
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
	taskID := gameplayTaskID(kind)
	if taskID == 0 {
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

func gameplayTaskID(kind farm.PlotActionKind) uint32 {
	switch kind {
	case farm.Plant:
		return store.TaskPlantID
	case farm.Harvest:
		return store.TaskHarvestID
	case farm.Water:
		return store.TaskWaterID
	case farm.Fertilize:
		return store.TaskFertilizeID
	case farm.Till:
		return store.TaskTillID
	case farm.Weed:
		return store.TaskWeedID
	case farm.Pest:
		return store.TaskPestID
	default:
		return 0
	}
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
	if h.bundleJournalEffects {
		dayKey := gameconfig.LocalDayKey(h.now())
		var result store.TaskAdvanceResult
		if err := h.runtime.Do(uid, func(farmActor *room.FarmActor) error {
			if err := h.ensureActorTasks(context.Background(), uid, dayKey, farmActor); err != nil {
				return err
			}
			result, _ = farmActor.AdvanceTaskState(dayKey, taskID, amount)
			if result.Changed {
				farmActor.RecordTaskAdvance(outbox.TaskAdvance{DayKey: dayKey, TaskID: taskID, Amount: amount})
				farmActor.RequireSideEffectFlush()
			}
			return nil
		}); err != nil {
			return err
		}
		if result.Changed {
			h.publishTaskNotify(uid, result.Task)
		}
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
	publish := func() {
		if err := h.deltaPublisher.Publish(context.Background(), *delta, originator); err != nil {
			telemetry.L().Error("farmrpc delta publish failed",
				"component", "farmrpc",
				"op", "publish_delta",
				"err", err.Error(),
			)
		}
	}
	// Production installs AsyncDeltaPublisher, so the direct call only queues
	// a detached value. Compatibility publishers may perform network I/O and
	// retain the old fire-and-forget behavior.
	if _, ok := h.deltaPublisher.(interface{ publishesAsynchronously() }); ok {
		publish()
		return
	}
	go publish()
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
