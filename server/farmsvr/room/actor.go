// Package room 提供按玩家 uid 串行执行农场操作的进程内运行时。
package room

import (
	"errors"
	"sort"

	"farm/server/domain/farm"
	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/clientwire"
	"farm/server/shared/outbox"
	"farm/server/shared/store"

	"google.golang.org/protobuf/proto"
)

// FarmActor 持有单个玩家在内存中的农场聚合。
//
// Aggregate 只能在 Runtime.Do 的回调中访问；Runtime 保证同一 uid 的回调不会并发执行。
type FarmActor struct {
	Aggregate *farm.Aggregate
	Deltas    farm.DeltaRing

	// results 是跨农场回执的进程内热缓存；跨 Actor 卸载的幂等性由 Aggregate
	// 持久化的 CrossReceipts 保证，这里仅避免短时间重投反复查聚合。
	results resultCache
	// syncFlush 由 RequireFlush 置位，Runtime 在回调返回后据此同步落盘。
	syncFlush bool
	// dirty 表示当前回调是否真实改动了需持久化的聚合状态。
	dirty bool
	// outboxEvents 记录尚未 durable ack 的跨农场结果等事件。
	outboxEvents []stampedOutboxEvent
	outboxTail   int
	taskAdvances []stampedTaskAdvance
	taskTail     int
	codexRewards []stampedCodexReward
	codexTail    int
	persistPlan  outbox.PersistPlan
	planSet      bool
	planGen      uint64
	dirtyPlots   map[uint8]uint64
	dirtyItems   map[farm.ItemKey]stampedItemCount
	dirtyCodex   map[uint16]uint64

	// Task/mail state is owned by the same UID Actor as the farm aggregate.
	// MySQL supplies only the cold baseline; after that, commands and reads use
	// these in-memory values while transitions are projected asynchronously.
	taskDay     int64
	tasks       map[uint32]store.Task
	tasksReady  bool
	mails       map[uint64]store.Mail
	mailsReady  bool
	mailDeleted map[uint64]struct{}

	taskClaims       []stampedTaskClaim
	taskClaimTail    int
	mailMutations    []stampedMailMutation
	mailMutationTail int

	snapshotProtoSeq uint64
	snapshotMessage  *publicv3.FarmSnapshot
	snapshotProto    []byte
}

// MarkDirty 标记当前回调已真实改动聚合；纯读路径不得调用。
func (a *FarmActor) MarkDirty() {
	if a == nil {
		return
	}
	a.dirty = true
	a.mergePersistPlan(outbox.PersistPlan{Mode: outbox.PersistFull})
	a.InvalidateSnapshot()
}

// InvalidateSnapshot drops transport encodings after state was persisted by a
// specialized store path. Unlike MarkDirty it must not enqueue another full
// aggregate write.
func (a *FarmActor) InvalidateSnapshot() {
	if a == nil {
		return
	}
	a.snapshotMessage = nil
	a.snapshotProto = nil
}

// SnapshotProto returns the immutable typed snapshot for the current
// aggregate version. The Actor invalidates this message on every mutation, so
// concurrent gRPC responses may safely marshal an older immutable value while
// a newer Actor version is being created.
func (a *FarmActor) SnapshotProto() (*publicv3.FarmSnapshot, error) {
	if a == nil || a.Aggregate == nil {
		return nil, errors.New("room: actor aggregate is nil")
	}
	if a.snapshotMessage != nil && a.snapshotProtoSeq == a.Aggregate.FarmSeq {
		return a.snapshotMessage, nil
	}
	a.snapshotProtoSeq = a.Aggregate.FarmSeq
	a.snapshotMessage = clientwire.FarmSnapshotToProto(a.Aggregate.Snapshot())
	return a.snapshotMessage, nil
}

// EncodedSnapshotProto returns the immutable serialized snapshot for the
// current aggregate version. Enter/Sync can embed these bytes directly in the
// public response instead of making gRPC and Gateway traverse the full farm.
func (a *FarmActor) EncodedSnapshotProto() ([]byte, error) {
	if a == nil || a.Aggregate == nil {
		return nil, errors.New("room: actor aggregate is nil")
	}
	if len(a.snapshotProto) != 0 && a.snapshotProtoSeq == a.Aggregate.FarmSeq {
		return a.snapshotProto, nil
	}
	message, err := a.SnapshotProto()
	if err != nil {
		return nil, err
	}
	encoded, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}
	a.snapshotProto = encoded
	return a.snapshotProto, nil
}

func (a *FarmActor) consumeDirty() bool {
	if a == nil {
		return false
	}
	dirty := a.dirty
	a.dirty = false
	return dirty
}

// RequireFlush 要求 Runtime 在本次回调返回前把聚合同步写入存储，写失败则整次
// 调用报错。
func (a *FarmActor) RequireFlush() {
	if a == nil {
		return
	}
	a.syncFlush = true
	a.MarkDirty()
}

// RequireEconomyFlush requests an ordered durable shop commit that only
// rewrites player economy and inventory rows.
func (a *FarmActor) RequireEconomyFlush() {
	a.requirePlannedFlush(outbox.PersistPlan{Mode: outbox.PersistEconomy})
}

// RequireSideEffectFlush durably appends a task/mail-only transition without
// pretending that the farm aggregate itself changed.
func (a *FarmActor) RequireSideEffectFlush() {
	if a == nil {
		return
	}
	a.syncFlush = true
	a.dirty = true
	a.mergePersistPlan(outbox.PersistPlan{Mode: outbox.PersistSideEffects})
}

// MarkPlotDirty records the smallest ordered write-behind plan for a local
// plot mutation. Inventory and codex rows are included only for actions that
// can actually change them.
func (a *FarmActor) MarkPlotDirty(plotIndex uint8, includeItems, includeCodex bool) {
	if a == nil {
		return
	}
	// The request must not acknowledge a state mutation until its immutable
	// snapshot is present in the reliable write journal. With the journaled
	// store this waits only for Redis Streams, while MySQL materialization stays
	// asynchronous. Tests and non-journal stores retain the original durable
	// behavior because the same Runtime boundary still applies.
	a.syncFlush = true
	a.dirty = true
	a.mergePersistPlan(outbox.PersistPlan{
		Mode:         outbox.PersistPlot,
		PlotIndex:    plotIndex,
		IncludeItems: includeItems,
		IncludeCodex: includeCodex,
	})
	if a.dirtyPlots == nil {
		a.dirtyPlots = make(map[uint8]uint64, 1)
	}
	a.dirtyPlots[plotIndex] = 0
	a.InvalidateSnapshot()
}

type stampedItemCount struct {
	generation uint64
	count      uint32
}

// SnapshotItems is a cheap inventory-only before-image. Callers use it around
// operations that may change inventory so the journal records exact keys,
// never a complete bag replacement.
func (a *FarmActor) SnapshotItems() map[farm.ItemKey]uint32 {
	if a == nil || a.Aggregate == nil || len(a.Aggregate.Items) == 0 {
		return nil
	}
	before := make(map[farm.ItemKey]uint32, len(a.Aggregate.Items))
	for key, count := range a.Aggregate.Items {
		before[key] = count
	}
	return before
}

// RecordItemChanges compares an inventory before-image with current Actor
// state and retains only final counts for changed keys. A zero count is an
// explicit row tombstone.
func (a *FarmActor) RecordItemChanges(before map[farm.ItemKey]uint32) {
	if a == nil || a.Aggregate == nil {
		return
	}
	if a.dirtyItems == nil {
		a.dirtyItems = make(map[farm.ItemKey]stampedItemCount)
	}
	for key, previous := range before {
		current := a.Aggregate.Items[key]
		if current != previous {
			a.dirtyItems[key] = stampedItemCount{count: current}
		}
	}
	for key, current := range a.Aggregate.Items {
		if previous, existed := before[key]; !existed && current != 0 || existed && previous != current {
			a.dirtyItems[key] = stampedItemCount{count: current}
		}
	}
}

// RecordItemCounts retains authoritative final counts already produced by the
// domain action patch. This avoids cloning and comparing the complete inventory
// on ordinary one-key mutations such as Plant, Harvest, Buy and Sell.
func (a *FarmActor) RecordItemCounts(changes map[farm.ItemKey]uint32) {
	if a == nil || len(changes) == 0 {
		return
	}
	if a.dirtyItems == nil {
		a.dirtyItems = make(map[farm.ItemKey]stampedItemCount, len(changes))
	}
	for key, count := range changes {
		if key != "" {
			a.dirtyItems[key] = stampedItemCount{count: count}
		}
	}
}

// RecordCodexChange marks one exact plaque row after Harvest.
func (a *FarmActor) RecordCodexChange(cropID uint16) {
	if a == nil || cropID == 0 {
		return
	}
	if a.dirtyCodex == nil {
		a.dirtyCodex = make(map[uint16]uint64, 1)
	}
	a.dirtyCodex[cropID] = 0
}

// RequireCrossVisitorFlush requests an ordered visitor reservation/settlement
// commit. Settlement includes inventory while reservation does not.
func (a *FarmActor) RequireCrossVisitorFlush(includeItems bool) {
	a.requirePlannedFlush(outbox.PersistPlan{
		Mode: outbox.PersistCrossVisitor, IncludeItems: includeItems,
	})
}

// RequireCrossOwnerFlush keeps the owner mutation and result outbox atomic in
// one reduced commit.
func (a *FarmActor) RequireCrossOwnerFlush(plotIndex uint8, event outbox.Event) {
	if a == nil || event.EventID == "" {
		return
	}
	a.outboxEvents = append(a.outboxEvents, stampedOutboxEvent{event: event})
	a.requireCrossOwnerFlush(plotIndex)
}

// RequireCrossOwnerCompositeFlush persists the owner-side final state without
// creating a result outbox. The colocated pair commit writes visitor and owner
// mutations atomically to the reliable journal, so there is no remote Saga to
// acknowledge or repair.
func (a *FarmActor) RequireCrossOwnerCompositeFlush(plotIndex uint8) {
	if a == nil {
		return
	}
	a.requireCrossOwnerFlush(plotIndex)
}

func (a *FarmActor) requireCrossOwnerFlush(plotIndex uint8) {
	if a.dirtyPlots == nil {
		a.dirtyPlots = make(map[uint8]uint64, 1)
	}
	if a.Aggregate != nil && int(plotIndex) < len(a.Aggregate.Plots) {
		a.dirtyPlots[plotIndex] = 0
	}
	a.requirePlannedFlush(outbox.PersistPlan{Mode: outbox.PersistCrossOwner, PlotIndex: plotIndex})
}

func (a *FarmActor) requirePlannedFlush(plan outbox.PersistPlan) {
	if a == nil {
		return
	}
	a.syncFlush = true
	a.dirty = true
	a.mergePersistPlan(plan)
	a.InvalidateSnapshot()
}

func (a *FarmActor) mergePersistPlan(plan outbox.PersistPlan) {
	if plan.Modes == 0 {
		plan.Modes = outbox.PersistModeMask(plan.Mode)
	}
	if !a.planSet {
		a.persistPlan = plan
		a.planSet = true
		return
	}
	if a.persistPlan.Mode != plan.Mode ||
		(plan.Mode == outbox.PersistCrossOwner || plan.Mode == outbox.PersistPlot) &&
			a.persistPlan.PlotIndex != plan.PlotIndex {
		a.persistPlan = outbox.PersistPlan{
			Mode:         outbox.PersistFull,
			Modes:        a.persistPlan.Modes | plan.Modes,
			IncludeItems: a.persistPlan.IncludeItems || plan.IncludeItems,
			IncludeCodex: a.persistPlan.IncludeCodex || plan.IncludeCodex,
		}
		return
	}
	a.persistPlan.Modes |= plan.Modes
	a.persistPlan.IncludeItems = a.persistPlan.IncludeItems || plan.IncludeItems
	a.persistPlan.IncludeCodex = a.persistPlan.IncludeCodex || plan.IncludeCodex
}

func (a *FarmActor) stampPersistGeneration(generation uint64) {
	if a != nil && a.planSet {
		a.planGen = generation
	}
	if a == nil {
		return
	}
	for index, stamped := range a.dirtyPlots {
		if stamped == 0 {
			a.dirtyPlots[index] = generation
		}
	}
	for key, stamped := range a.dirtyItems {
		if stamped.generation == 0 {
			stamped.generation = generation
			a.dirtyItems[key] = stamped
		}
	}
	for cropID, stamped := range a.dirtyCodex {
		if stamped == 0 {
			a.dirtyCodex[cropID] = generation
		}
	}
}

func (a *FarmActor) pendingPersistPlan() outbox.PersistPlan {
	if a == nil || !a.planSet {
		return outbox.PersistPlan{Mode: outbox.PersistFull}
	}
	return a.persistPlan
}

func (a *FarmActor) ackPersistPlan(generation uint64) {
	if a == nil {
		return
	}
	if a.planSet && generation >= a.planGen {
		a.persistPlan = outbox.PersistPlan{}
		a.planSet = false
		a.planGen = 0
	}
	for index, stamped := range a.dirtyPlots {
		if stamped != 0 && stamped <= generation {
			delete(a.dirtyPlots, index)
		}
	}
	for key, stamped := range a.dirtyItems {
		if stamped.generation != 0 && stamped.generation <= generation {
			delete(a.dirtyItems, key)
		}
	}
	for cropID, stamped := range a.dirtyCodex {
		if stamped != 0 && stamped <= generation {
			delete(a.dirtyCodex, cropID)
		}
	}
}

func (a *FarmActor) pendingWriteMutation() (*farmv1.FarmWriteMutation, error) {
	if a == nil || a.Aggregate == nil {
		return nil, errors.New("room: actor aggregate is nil")
	}
	plots := make([]uint8, 0, len(a.dirtyPlots))
	for index, generation := range a.dirtyPlots {
		if generation != 0 {
			plots = append(plots, index)
		}
	}
	items := make(map[farm.ItemKey]uint32, len(a.dirtyItems))
	for key, stamped := range a.dirtyItems {
		if stamped.generation != 0 {
			items[key] = stamped.count
		}
	}
	codex := make([]uint16, 0, len(a.dirtyCodex))
	for cropID, generation := range a.dirtyCodex {
		if generation != 0 {
			codex = append(codex, cropID)
		}
	}
	return outbox.NewFarmWriteMutation(
		a.Aggregate, a.pendingPersistPlan(), plots, items, codex,
		a.pendingOutboxEvents(), a.pendingTaskAdvances(), a.pendingCodexRewards(),
		a.pendingTaskClaims(), a.pendingMailMutations(),
	)
}

type stampedOutboxEvent struct {
	generation uint64
	event      outbox.Event
}

type stampedTaskAdvance struct {
	generation uint64
	advance    outbox.TaskAdvance
}

type stampedCodexReward struct {
	generation uint64
	reward     outbox.CodexReward
}

type stampedTaskClaim struct {
	generation uint64
	claim      outbox.TaskClaim
}

type stampedMailMutation struct {
	generation uint64
	mutation   outbox.MailMutation
}

func (a *FarmActor) TasksReady(dayKey int64) bool {
	return a != nil && a.tasksReady && a.taskDay == dayKey
}

func (a *FarmActor) LoadTasks(dayKey int64, tasks []store.Task) {
	if a == nil {
		return
	}
	a.taskDay, a.tasksReady = dayKey, true
	a.tasks = make(map[uint32]store.Task, len(tasks))
	for _, task := range tasks {
		a.tasks[task.ID] = task
	}
}

func (a *FarmActor) TaskSnapshot(dayKey int64) []store.Task {
	if !a.TasksReady(dayKey) {
		return nil
	}
	tasks := make([]store.Task, 0, len(a.tasks))
	for _, task := range a.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks
}

func (a *FarmActor) AdvanceTaskState(dayKey int64, taskID, amount uint32) (store.TaskAdvanceResult, bool) {
	if !a.TasksReady(dayKey) || amount == 0 {
		return store.TaskAdvanceResult{}, false
	}
	task, ok := a.tasks[taskID]
	if !ok || task.Claimed || task.Progress >= task.Target {
		return store.TaskAdvanceResult{}, true
	}
	previous := task.Progress
	task.Progress = uint32(min(uint64(task.Target), uint64(task.Progress)+uint64(amount)))
	a.tasks[taskID] = task
	return store.TaskAdvanceResult{
		Task: task, Changed: task.Progress != previous,
		JustCompleted: previous < task.Target && task.Progress == task.Target,
	}, true
}

func (a *FarmActor) ClaimTaskState(dayKey int64, taskID uint32, claimedAt int64) (store.TaskReward, error) {
	if !a.TasksReady(dayKey) {
		return store.TaskReward{}, errors.New("room: task state is not loaded")
	}
	task, ok := a.tasks[taskID]
	if !ok || task.Progress < task.Target {
		return store.TaskReward{}, store.ErrTaskNotComplete
	}
	if task.Claimed {
		return store.TaskReward{}, store.ErrTaskAlreadyClaimed
	}
	task.Claimed = true
	task.Progress = task.Target
	a.tasks[taskID] = task
	a.taskClaims = append(a.taskClaims, stampedTaskClaim{claim: outbox.TaskClaim{
		DayKey: dayKey, TaskID: taskID, ClaimedAt: claimedAt,
	}})
	return store.TaskReward{Coin: task.RewardCoin}, nil
}

func (a *FarmActor) MailsReady() bool { return a != nil && a.mailsReady }

func (a *FarmActor) HasMail(mailID uint64) bool {
	if a == nil || a.mails == nil {
		return false
	}
	_, exists := a.mails[mailID]
	return exists
}

func (a *FarmActor) LoadMails(mails []store.Mail) {
	if a == nil {
		return
	}
	// A targeted MailClaim cold load may already have changed one mail while the
	// full mailbox baseline was still absent. Reapply those Actor-owned facts so
	// an asynchronous MySQL projection cannot temporarily roll state backwards.
	pending := append([]stampedMailMutation(nil), a.mailMutations...)
	a.mailsReady = true
	a.mails = make(map[uint64]store.Mail, len(mails))
	a.mailDeleted = nil
	for _, mail := range mails {
		a.mails[mail.ID] = mail
	}
	for _, stamped := range pending {
		mutation := stamped.mutation
		mail, exists := a.mails[mutation.MailID]
		switch mutation.Kind {
		case outbox.MailRead:
			if exists {
				mail.Read = true
				a.mails[mutation.MailID] = mail
			}
		case outbox.MailClaim:
			if exists {
				mail.Read, mail.Claimed = true, true
				a.mails[mutation.MailID] = mail
			}
		case outbox.MailDelete:
			delete(a.mails, mutation.MailID)
			if a.mailDeleted == nil {
				a.mailDeleted = make(map[uint64]struct{})
			}
			a.mailDeleted[mutation.MailID] = struct{}{}
		}
	}
}

// LoadMailForClaim installs one cold row without declaring the whole mailbox
// loaded. This lets MailClaim stay O(1) in mailbox size; a later MailList still
// obtains the complete baseline through LoadMails above.
func (a *FarmActor) LoadMailForClaim(mail store.Mail) {
	if a == nil || mail.ID == 0 {
		return
	}
	if a.mails == nil {
		a.mails = make(map[uint64]store.Mail, 1)
	}
	if _, deleted := a.mailDeleted[mail.ID]; deleted {
		return
	}
	if _, exists := a.mails[mail.ID]; !exists {
		a.mails[mail.ID] = mail
	}
}

// MergeMails adds externally created durable mails without overwriting newer
// Actor-owned read/claim/delete state. Tombstones prevent a not-yet-projected
// local delete from being resurrected by a stale MySQL/cache baseline.
func (a *FarmActor) MergeMails(mails []store.Mail) {
	if a == nil || !a.mailsReady {
		a.LoadMails(mails)
		return
	}
	for _, mail := range mails {
		if _, deleted := a.mailDeleted[mail.ID]; deleted {
			continue
		}
		if _, exists := a.mails[mail.ID]; !exists {
			a.mails[mail.ID] = mail
		}
	}
}

func (a *FarmActor) MailSnapshot() []store.Mail {
	if !a.MailsReady() {
		return nil
	}
	mails := make([]store.Mail, 0, len(a.mails))
	for _, mail := range a.mails {
		mails = append(mails, mail)
	}
	sort.Slice(mails, func(i, j int) bool { return mails[i].ID > mails[j].ID })
	return mails
}

func (a *FarmActor) MarkMailsReadState(mailID uint64, occurredAt int64) int64 {
	if !a.MailsReady() {
		return 0
	}
	var affected int64
	for id, mail := range a.mails {
		if (mailID != 0 && id != mailID) || mail.Read {
			continue
		}
		mail.Read = true
		a.mails[id] = mail
		a.mailMutations = append(a.mailMutations, stampedMailMutation{mutation: outbox.MailMutation{
			MailID: id, Kind: outbox.MailRead, OccurredAt: occurredAt,
		}})
		affected++
	}
	return affected
}

func (a *FarmActor) DeleteMailsState(mailID uint64, occurredAt int64) int64 {
	if !a.MailsReady() {
		return 0
	}
	var affected int64
	for id, mail := range a.mails {
		if (mailID != 0 && id != mailID) || (mail.AttachmentCoin > 0 && !mail.Claimed) {
			continue
		}
		delete(a.mails, id)
		if a.mailDeleted == nil {
			a.mailDeleted = make(map[uint64]struct{})
		}
		a.mailDeleted[id] = struct{}{}
		a.mailMutations = append(a.mailMutations, stampedMailMutation{mutation: outbox.MailMutation{
			MailID: id, Kind: outbox.MailDelete, OccurredAt: occurredAt,
		}})
		affected++
	}
	return affected
}

func (a *FarmActor) ClaimMailState(mailID uint64, occurredAt int64) (store.Mail, error) {
	if a == nil || a.mails == nil {
		return store.Mail{}, errors.New("room: mail state is not loaded")
	}
	mail, ok := a.mails[mailID]
	if !ok {
		return store.Mail{}, store.ErrMailNotFound
	}
	if mail.AttachmentCoin <= 0 {
		return store.Mail{}, store.ErrMailNoAttachment
	}
	if mail.Claimed {
		return store.Mail{}, store.ErrMailAlreadyClaimed
	}
	mail.Claimed, mail.Read = true, true
	a.mails[mailID] = mail
	a.mailMutations = append(a.mailMutations, stampedMailMutation{mutation: outbox.MailMutation{
		MailID: mailID, Kind: outbox.MailClaim, OccurredAt: occurredAt,
	}})
	return mail, nil
}

// RecordTaskAdvance and RecordCodexReward attach gameplay side effects to the
// same durable Farm commit instead of issuing additional synchronous XADDs.
func (a *FarmActor) RecordTaskAdvance(advance outbox.TaskAdvance) {
	if a == nil || advance.DayKey == 0 || advance.TaskID == 0 || advance.Amount == 0 {
		return
	}
	a.taskAdvances = append(a.taskAdvances, stampedTaskAdvance{advance: advance})
	a.dirty = true
}

func (a *FarmActor) RecordCodexReward(progress farm.CodexProgress) {
	if a == nil || progress.CropID == 0 || progress.HarvestCount == 0 {
		return
	}
	a.codexRewards = append(a.codexRewards, stampedCodexReward{
		reward: outbox.CodexReward{Progress: progress},
	})
	a.dirty = true
}

func (a *FarmActor) stampSideEffectGeneration(generation uint64) {
	for index := a.taskTail; index < len(a.taskAdvances); index++ {
		a.taskAdvances[index].generation = generation
	}
	a.taskTail = len(a.taskAdvances)
	for index := a.codexTail; index < len(a.codexRewards); index++ {
		a.codexRewards[index].generation = generation
	}
	a.codexTail = len(a.codexRewards)
	for index := a.taskClaimTail; index < len(a.taskClaims); index++ {
		a.taskClaims[index].generation = generation
	}
	a.taskClaimTail = len(a.taskClaims)
	for index := a.mailMutationTail; index < len(a.mailMutations); index++ {
		a.mailMutations[index].generation = generation
	}
	a.mailMutationTail = len(a.mailMutations)
}

func (a *FarmActor) pendingTaskAdvances() []outbox.TaskAdvance {
	advances := make([]outbox.TaskAdvance, 0, len(a.taskAdvances))
	for _, stamped := range a.taskAdvances {
		if stamped.generation != 0 {
			advances = append(advances, stamped.advance)
		}
	}
	return advances
}

func (a *FarmActor) pendingCodexRewards() []outbox.CodexReward {
	rewards := make([]outbox.CodexReward, 0, len(a.codexRewards))
	for _, stamped := range a.codexRewards {
		if stamped.generation != 0 {
			rewards = append(rewards, stamped.reward)
		}
	}
	return rewards
}

func (a *FarmActor) pendingTaskClaims() []outbox.TaskClaim {
	claims := make([]outbox.TaskClaim, 0, len(a.taskClaims))
	for _, stamped := range a.taskClaims {
		if stamped.generation != 0 {
			claims = append(claims, stamped.claim)
		}
	}
	return claims
}

func (a *FarmActor) pendingMailMutations() []outbox.MailMutation {
	mutations := make([]outbox.MailMutation, 0, len(a.mailMutations))
	for _, stamped := range a.mailMutations {
		if stamped.generation != 0 {
			mutations = append(mutations, stamped.mutation)
		}
	}
	return mutations
}

func (a *FarmActor) ackSideEffects(committedGen uint64) {
	if a == nil || committedGen == 0 {
		return
	}
	keptTasks := a.taskAdvances[:0]
	for _, stamped := range a.taskAdvances {
		if stamped.generation > committedGen {
			keptTasks = append(keptTasks, stamped)
		}
	}
	a.taskAdvances = keptTasks
	a.taskTail = len(keptTasks)
	keptCodex := a.codexRewards[:0]
	for _, stamped := range a.codexRewards {
		if stamped.generation > committedGen {
			keptCodex = append(keptCodex, stamped)
		}
	}
	a.codexRewards = keptCodex
	a.codexTail = len(keptCodex)
	keptClaims := a.taskClaims[:0]
	for _, stamped := range a.taskClaims {
		if stamped.generation > committedGen {
			keptClaims = append(keptClaims, stamped)
		}
	}
	a.taskClaims = keptClaims
	a.taskClaimTail = len(keptClaims)
	keptMail := a.mailMutations[:0]
	for _, stamped := range a.mailMutations {
		if stamped.generation > committedGen {
			keptMail = append(keptMail, stamped)
		}
	}
	a.mailMutations = keptMail
	a.mailMutationTail = len(keptMail)
}

// RecordOutbox queues a durable outbox event for the current callback generation.
func (a *FarmActor) RecordOutbox(event outbox.Event) {
	if a == nil || event.EventID == "" {
		return
	}
	a.outboxEvents = append(a.outboxEvents, stampedOutboxEvent{event: event})
	a.MarkDirty()
}

func (a *FarmActor) stampOutboxGeneration(generation uint64) {
	for i := a.outboxTail; i < len(a.outboxEvents); i++ {
		a.outboxEvents[i].generation = generation
	}
	a.outboxTail = len(a.outboxEvents)
}

func (a *FarmActor) pendingOutboxEvents() []outbox.Event {
	if a == nil || len(a.outboxEvents) == 0 {
		return nil
	}
	events := make([]outbox.Event, 0, len(a.outboxEvents))
	for _, stamped := range a.outboxEvents {
		if stamped.generation == 0 {
			continue
		}
		events = append(events, stamped.event)
	}
	return events
}

func (a *FarmActor) ackOutbox(committedGen uint64) {
	if a == nil || committedGen == 0 || len(a.outboxEvents) == 0 {
		return
	}
	kept := a.outboxEvents[:0]
	for _, stamped := range a.outboxEvents {
		if stamped.generation > committedGen {
			kept = append(kept, stamped)
		}
	}
	a.outboxEvents = kept
	a.outboxTail = len(a.outboxEvents)
}

// CachedResult 读取先前缓存的跨农场结果，用于快速识别重复投递的同一请求。
func (a *FarmActor) CachedResult(reqID uint64) (any, bool) {
	if a == nil {
		return nil, false
	}
	return a.results.get(reqID)
}

// CacheResult 记录一次跨农场请求的处理结果，供热路径上的重复投递直接复用。
func (a *FarmActor) CacheResult(reqID uint64, result any) {
	if a == nil {
		return
	}
	a.results.put(reqID, result)
}

// resultCacheCapacity 是每个 Actor 保留的幂等结果条数。
//
// 只需覆盖事件总线的重投窗口：同一 (owner, req_id) 在短时间内可能被投递多次，
// 但不会在几十条其他请求之后再回来。
const resultCacheCapacity = 64

// resultCache 是固定容量的先进先出缓存。容量很小，用切片顺序扫描比维护链表更简单。
type resultCache struct {
	order   []uint64
	entries map[uint64]any
}

func (c *resultCache) get(reqID uint64) (any, bool) {
	if c == nil || c.entries == nil {
		return nil, false
	}
	result, ok := c.entries[reqID]
	return result, ok
}

func (c *resultCache) put(reqID uint64, result any) {
	if c == nil {
		return
	}
	if c.entries == nil {
		c.entries = make(map[uint64]any, resultCacheCapacity)
		c.order = make([]uint64, 0, resultCacheCapacity)
	}
	if _, exists := c.entries[reqID]; exists {
		c.entries[reqID] = result
		return
	}
	if len(c.order) >= resultCacheCapacity {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, reqID)
	c.entries[reqID] = result
}
