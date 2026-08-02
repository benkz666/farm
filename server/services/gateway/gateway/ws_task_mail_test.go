package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"farm/server/platform/farm"
	"farm/server/platform/gameconf"
	"farm/server/platform/pkgerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/store"
)

func TestCodexListReturnsAuthoritativePerCropPlaques(t *testing.T) {
	t.Parallel()

	aggregate := farm.NewAggregate(42, "alice")
	aggregate.CodexHarvests[4] = 50
	aggregate.CodexHarvests[1] = 10
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
	)
	response := gateway.handleWSRequest(&wsConnection{uid: 42, authed: true}, Envelope{
		Cmd:       CommandCodexList,
		ClientSeq: 7,
		Payload:   emptyPayload,
	})
	if response.Err != pkgerr.OK {
		t.Fatalf("CodexList err = %d, want OK", response.Err)
	}
	var payload struct {
		Entries []farm.CodexProgress `json:"entries"`
		Total   int                  `json:"total"`
	}
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode CodexList: %v", err)
	}
	if payload.Total != gameconf.CropCount || len(payload.Entries) != 2 {
		t.Fatalf("CodexList payload = %#v", payload)
	}
	if payload.Entries[0].CropID != 1 || payload.Entries[0].Tier != "bronze" ||
		payload.Entries[0].NextTarget != 20 {
		t.Fatalf("first plaque = %#v", payload.Entries[0])
	}
	if payload.Entries[1].CropID != 4 || payload.Entries[1].Tier != "gold" ||
		payload.Entries[1].NextTarget != 0 {
		t.Fatalf("second plaque = %#v", payload.Entries[1])
	}
}

func TestDailyLoginAndTaskRewardsAreDirectAndIdempotent(t *testing.T) {
	t.Parallel()

	storage := newTaskMailStoreStub()
	aggregate := farm.NewAggregate(42, "alice")
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTaskMailStore(storage),
	)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	gateway.SetClock(func() int64 {
		return now
	})
	connection := &wsConnection{uid: 42, authed: true}

	firstDaily := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandClaimDailyLogin,
		Payload: emptyPayload,
	})
	if firstDaily.Err != pkgerr.OK {
		t.Fatalf("first ClaimDailyLogin err = %d, want OK", firstDaily.Err)
	}
	var dailyReward store.TaskReward
	if err := json.Unmarshal(firstDaily.Payload, &dailyReward); err != nil {
		t.Fatalf("decode daily reward: %v", err)
	}
	if dailyReward.Coin != 100 {
		t.Fatalf("daily reward = %#v, want 100 coins", dailyReward)
	}

	secondDaily := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandClaimDailyLogin,
		Payload: emptyPayload,
	})
	if secondDaily.Err != pkgerr.DuplicateOK {
		t.Fatalf("repeated ClaimDailyLogin err = %d, want %d", secondDaily.Err, pkgerr.DuplicateOK)
	}

	tasks := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandTaskList,
		Payload: emptyPayload,
	})
	var taskList struct {
		Tasks   []store.Task `json:"tasks"`
		ResetAt int64        `json:"reset_at"`
	}
	if err := json.Unmarshal(tasks.Payload, &taskList); err != nil {
		t.Fatalf("decode TaskList: %v", err)
	}
	if tasks.Err != pkgerr.OK || len(taskList.Tasks) != 4 {
		t.Fatalf("TaskList = %#v, payload = %#v", tasks, taskList)
	}
	if taskList.ResetAt != gameconf.NextLocalDayResetMs(now) {
		t.Fatalf("TaskList reset_at = %d, want %d", taskList.ResetAt, gameconf.NextLocalDayResetMs(now))
	}
	dailyTask := taskList.Tasks[store.TaskDailyLoginID-1]
	if dailyTask.Progress != 1 || dailyTask.Target != 1 || !dailyTask.Claimed {
		t.Fatalf("daily login task = %#v, want completed and claimed", dailyTask)
	}
	duplicateDailyTask := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandTaskClaim,
		Payload: json.RawMessage(`{"task_id":4}`),
	})
	if duplicateDailyTask.Err != pkgerr.TaskAlreadyClaimed {
		t.Fatalf("TaskClaim daily after ClaimDailyLogin err = %d, want %d", duplicateDailyTask.Err, pkgerr.TaskAlreadyClaimed)
	}

	taskClaim := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandTaskClaim,
		Payload: json.RawMessage(`{"task_id":1}`),
	})
	if taskClaim.Err != pkgerr.OK {
		t.Fatalf("TaskClaim err = %d, want OK", taskClaim.Err)
	}
	var taskReward store.TaskReward
	if err := json.Unmarshal(taskClaim.Payload, &taskReward); err != nil {
		t.Fatalf("decode task reward: %v", err)
	}
	if taskReward.Coin != 20 {
		t.Fatalf("task reward = %#v, want 20 coins", taskReward)
	}

	mails := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailList,
		Payload: emptyPayload,
	})
	var mailList struct {
		Mails []store.Mail `json:"mails"`
	}
	if err := json.Unmarshal(mails.Payload, &mailList); err != nil {
		t.Fatalf("decode MailList: %v", err)
	}
	if mails.Err != pkgerr.OK || len(mailList.Mails) != 0 {
		t.Fatalf("MailList = %#v, payload = %#v, want no reward mail", mails, mailList)
	}

	duplicateTaskClaim := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandTaskClaim,
		Payload: json.RawMessage(`{"task_id":1}`),
	})
	if duplicateTaskClaim.Err != pkgerr.TaskAlreadyClaimed {
		t.Fatalf("repeated TaskClaim err = %d, want %d", duplicateTaskClaim.Err, pkgerr.TaskAlreadyClaimed)
	}

	if aggregate.Coin != gameconf.InitialCoin+dailyReward.Coin+taskReward.Coin {
		t.Fatalf("online actor coin = %d, want %d", aggregate.Coin, gameconf.InitialCoin+dailyReward.Coin+taskReward.Coin)
	}

	missingMail := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailClaim,
		Payload: json.RawMessage(`{"mail_id":999}`),
	})
	if missingMail.Err != pkgerr.MailNotFound {
		t.Fatalf("missing MailClaim err = %d, want %d", missingMail.Err, pkgerr.MailNotFound)
	}
	notice := storage.addMail("system notice", 0)
	noAttachment := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailClaim,
		Payload: marshalPayload(mailClaimRequest{MailID: pkgjson.Uint64(notice.ID)}),
	})
	if noAttachment.Err != pkgerr.MailNoAttachment {
		t.Fatalf("empty MailClaim err = %d, want %d", noAttachment.Err, pkgerr.MailNoAttachment)
	}
}

func TestMailReadAndDeleteAllAreScopedToAuthenticatedPlayer(t *testing.T) {
	storage := newTaskMailStoreStub()
	first := storage.addMail("new notice", 0)
	second := storage.addMail("reward", 50)
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithTaskMailStore(storage),
	)
	connection := &wsConnection{uid: 42, authed: true}

	read := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailRead,
		Payload: json.RawMessage(`{"all":true}`),
	})
	if read.Err != pkgerr.OK {
		t.Fatalf("MailRead err = %d, want OK", read.Err)
	}
	if !storage.mails[first.ID].Read || !storage.mails[second.ID].Read {
		t.Fatalf("mails after MailRead = %#v, want all read", storage.mails)
	}
	if storage.lastMailUID != 42 {
		t.Fatalf("MailRead uid = %d, want authenticated uid 42", storage.lastMailUID)
	}

	deleted := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailDelete,
		Payload: json.RawMessage(`{"all":true}`),
	})
	if deleted.Err != pkgerr.OK {
		t.Fatalf("MailDelete err = %d, want OK", deleted.Err)
	}
	if len(storage.mails) != 0 {
		t.Fatalf("mails after MailDelete = %#v, want empty", storage.mails)
	}
	if storage.lastMailUID != 42 {
		t.Fatalf("MailDelete uid = %d, want authenticated uid 42", storage.lastMailUID)
	}
}

func TestMailMutationRejectsAmbiguousTarget(t *testing.T) {
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
		WithTaskMailStore(newTaskMailStoreStub()),
	)
	connection := &wsConnection{uid: 42, authed: true}

	for _, payload := range []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"all":true,"mail_id":1}`),
	} {
		response := gateway.handleWSRequest(connection, Envelope{
			Cmd:     CommandMailRead,
			Payload: payload,
		})
		if response.Err != pkgerr.BadRequest {
			t.Fatalf("MailRead payload %s err = %d, want BadRequest", payload, response.Err)
		}
	}
}

func TestSuccessfulPlantAdvancesDailyTaskProgress(t *testing.T) {
	storage := newTaskMailStoreStub()
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0].State = farm.StateTilled
	aggregate.Items[farm.SeedItem(1)] = 1
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTaskMailStore(storage),
	)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	gateway.SetClock(func() int64 {
		return now
	})

	response := gateway.handleWSRequest(&wsConnection{uid: 42, authed: true}, Envelope{
		Cmd:     CommandPlant,
		Payload: json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("Plant err = %d, want OK", response.Err)
	}
	want := taskProgressCall{uid: 42, dayKey: gameconf.LocalDayKey(now), taskID: store.TaskPlantID, amount: 1}
	if len(storage.progressCalls) != 1 || storage.progressCalls[0] != want {
		t.Fatalf("progress calls = %#v, want %#v", storage.progressCalls, want)
	}
}

func TestTaskNotifyDoesNotBlockPlantOnSlowConnection(t *testing.T) {
	storage := newTaskMailStoreStub()
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0].State = farm.StateTilled
	aggregate.Items[farm.SeedItem(1)] = 1
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTaskMailStore(storage),
	)
	started := make(chan struct{})
	release := make(chan struct{})
	firstDelivery := true
	gateway.taskNotifyDelivery = func(_ *wsConnection, _ store.Task) error {
		if !firstDelivery {
			return nil
		}
		firstDelivery = false
		close(started)
		<-release
		return nil
	}
	slow := &wsConnection{id: 1, uid: 42, authed: true}
	slow.enableTaskNotify(gateway)
	defer slow.closeTaskNotify()
	gateway.connections.Store(uint64(1), slow)
	gateway.PublishTaskNotify(t.Context(), 42, store.Task{ID: store.TaskPlantID})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow TaskNotify delivery did not start")
	}
	defer close(release)

	responses := make(chan Envelope, 1)
	go func() {
		responses <- gateway.handleWSRequest(&wsConnection{uid: 42, authed: true}, Envelope{
			Cmd:     CommandPlant,
			Payload: json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
		})
	}()
	select {
	case response := <-responses:
		if response.Err != pkgerr.OK {
			t.Fatalf("Plant err = %d, want OK", response.Err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Plant waited for slow TaskNotify connection")
	}
}

func TestSuccessfulHarvestAdvancesTaskAndPublishesTaskNotify(t *testing.T) {
	storage := newTaskMailStoreStub()
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0] = farm.Plot{
		State:          farm.StateMature,
		CropID:         1,
		SeasonDuration: gameconf.HourMs(gameconf.TimeProfileDemo),
		MatureAt:       1,
		FinalYield:     16,
	}
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTaskMailStore(storage),
	)
	gateway.SetClock(func() int64 { return 1 })
	notified := make(chan store.Task, 1)
	gateway.taskNotifyDelivery = func(_ *wsConnection, task store.Task) error {
		notified <- task
		return nil
	}
	connection := &wsConnection{id: 1, uid: 42, authed: true}
	connection.enableTaskNotify(gateway)
	defer connection.closeTaskNotify()
	gateway.connections.Store(uint64(1), connection)

	response := gateway.handleWSRequest(&wsConnection{uid: 42, authed: true}, Envelope{
		Cmd:     CommandHarvest,
		Payload: json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":0}`),
	})
	if response.Err != pkgerr.OK {
		t.Fatalf("Harvest err = %d, want OK", response.Err)
	}
	want := taskProgressCall{uid: 42, dayKey: gameconf.LocalDayKey(gateway.Now()), taskID: store.TaskHarvestID, amount: 1}
	if len(storage.progressCalls) != 1 || storage.progressCalls[0] != want {
		t.Fatalf("progress calls = %#v, want %#v", storage.progressCalls, want)
	}
	select {
	case task := <-notified:
		if task.ID != store.TaskHarvestID || task.Progress != task.Target {
			t.Fatalf("Harvest TaskNotify = %#v", task)
		}
	case <-time.After(time.Second):
		t.Fatal("Harvest did not publish TaskNotify")
	}
}

func TestFriendEnterAdvancesDailyVisitTask(t *testing.T) {
	const (
		ownerUID   = uint64(42)
		visitorUID = uint64(7)
	)
	storage := newTaskMailStoreStub()
	friends := newFriendStoreStub()
	friends.add(ownerUID, visitorUID)
	gateway := New(
		authStub{},
		sessionStub{uid: visitorUID},
		runtimeStub{aggregate: farm.NewAggregate(ownerUID, "owner")},
		WithFriendStore(friends),
		WithTaskMailStore(storage),
	)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.Local).UnixMilli()
	gateway.SetClock(func() int64 {
		return now
	})

	response := gateway.handleWSRequest(&wsConnection{uid: visitorUID, authed: true}, Envelope{
		Cmd:     CommandEnterFarm,
		Payload: marshalPayload(enterFarmRequest{OwnerUID: pkgjson.UID(ownerUID)}),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("EnterFarm err = %d, want OK", response.Err)
	}
	want := taskProgressCall{uid: visitorUID, dayKey: gameconf.LocalDayKey(now), taskID: store.TaskVisitID, amount: 1}
	if len(storage.progressCalls) != 1 || storage.progressCalls[0] != want {
		t.Fatalf("progress calls = %#v, want %#v", storage.progressCalls, want)
	}
}

// 任务计数是旁路副作用：种植已经在 Actor 里提交、Delta 已经广播给房间，此时把响应
// 改成失败会让客户端回滚一次真实发生的变更，违反「失败无副作用」。
func TestTaskProgressFailureDoesNotFailCommittedPlant(t *testing.T) {
	storage := newTaskMailStoreStub()
	storage.advanceErr = errors.New("task storage unavailable")
	aggregate := farm.NewAggregate(42, "alice")
	aggregate.Plots[0].State = farm.StateTilled
	aggregate.Items[farm.SeedItem(1)] = 1
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTaskMailStore(storage),
	)

	response := gateway.handleWSRequest(&wsConnection{uid: 42, authed: true}, Envelope{
		Cmd:     CommandPlant,
		Payload: json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("Plant err = %d, want OK despite task progress failure", response.Err)
	}
	if aggregate.Plots[0].State != farm.StateGrowing {
		t.Fatalf("plot state = %d, want growing", aggregate.Plots[0].State)
	}
	if got := aggregate.Items[farm.SeedItem(1)]; got != 0 {
		t.Fatalf("seed still in bag = %d, want 0", got)
	}
}

type taskMailStoreStub struct {
	mails         map[uint64]store.Mail
	tasks         map[uint32]bool
	progressCalls []taskProgressCall
	advanceErr    error
	next          uint64
	lastMailUID   uint64
}

type taskProgressCall struct {
	uid    uint64
	dayKey int64
	taskID uint32
	amount uint32
}

func newTaskMailStoreStub() *taskMailStoreStub {
	return &taskMailStoreStub{
		mails: make(map[uint64]store.Mail),
		tasks: make(map[uint32]bool),
	}
}

func (s *taskMailStoreStub) ListTasks(_ context.Context, _ uint64, _ int64) ([]store.Task, error) {
	return []store.Task{
		{ID: 1, Title: "stub plant", Progress: 1, Target: 1, RewardCoin: 20},
		{ID: 2, Title: "stub harvest", Progress: 1, Target: 1, RewardCoin: 30},
		{ID: 3, Title: "stub visit", Progress: 1, Target: 1, RewardCoin: 40},
		{ID: store.TaskDailyLoginID, Title: "每日登录", Progress: 1, Target: 1, RewardCoin: 100, Claimed: s.tasks[store.TaskDailyLoginID]},
	}, nil
}

func (s *taskMailStoreStub) ClaimTask(_ context.Context, _ uint64, _ int64, taskID uint32) (store.TaskReward, error) {
	if s.tasks[taskID] {
		return store.TaskReward{}, store.ErrTaskAlreadyClaimed
	}
	s.tasks[taskID] = true
	if taskID == store.TaskDailyLoginID {
		return store.TaskReward{Coin: 100}, nil
	}
	return store.TaskReward{Coin: 20}, nil
}

func (s *taskMailStoreStub) ListMails(_ context.Context, _ uint64) ([]store.Mail, error) {
	out := make([]store.Mail, 0, len(s.mails))
	for _, mail := range s.mails {
		out = append(out, mail)
	}
	return out, nil
}

func (s *taskMailStoreStub) MarkMailsRead(_ context.Context, uid uint64, mailID uint64) (int64, error) {
	s.lastMailUID = uid
	var affected int64
	for id, mail := range s.mails {
		if mailID != 0 && id != mailID {
			continue
		}
		if !mail.Read {
			mail.Read = true
			s.mails[id] = mail
			affected++
		}
	}
	return affected, nil
}

func (s *taskMailStoreStub) DeleteMails(_ context.Context, uid uint64, mailID uint64) (int64, error) {
	s.lastMailUID = uid
	var affected int64
	for id := range s.mails {
		if mailID != 0 && id != mailID {
			continue
		}
		delete(s.mails, id)
		affected++
	}
	return affected, nil
}

func (s *taskMailStoreStub) ClaimMail(_ context.Context, _ uint64, mailID uint64) (store.Mail, error) {
	mail, ok := s.mails[mailID]
	if !ok {
		return store.Mail{}, store.ErrMailNotFound
	}
	if mail.AttachmentCoin == 0 {
		return store.Mail{}, store.ErrMailNoAttachment
	}
	if mail.Claimed {
		return store.Mail{}, store.ErrMailAlreadyClaimed
	}
	mail.Claimed = true
	mail.Read = true
	s.mails[mailID] = mail
	return mail, nil
}

func (s *taskMailStoreStub) ClaimDailyLogin(ctx context.Context, uid uint64, dayKey int64) (store.TaskReward, error) {
	return s.ClaimTask(ctx, uid, dayKey, store.TaskDailyLoginID)
}

func (s *taskMailStoreStub) AdvanceTask(_ context.Context, uid uint64, dayKey int64, taskID, amount uint32) (store.TaskAdvanceResult, error) {
	s.progressCalls = append(s.progressCalls, taskProgressCall{
		uid: uid, dayKey: dayKey, taskID: taskID, amount: amount,
	})
	return store.TaskAdvanceResult{
		Task:    store.Task{ID: taskID, Progress: 1, Target: 1},
		Changed: s.advanceErr == nil,
	}, s.advanceErr
}

func (s *taskMailStoreStub) addMail(title string, coin int64) store.Mail {
	s.next++
	mail := store.Mail{ID: s.next, Title: title, AttachmentCoin: coin}
	s.mails[mail.ID] = mail
	return mail
}

var _ store.TaskMailStore = (*taskMailStoreStub)(nil)
