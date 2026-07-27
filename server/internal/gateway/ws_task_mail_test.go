package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

func TestDailyLoginAndTaskRewardsUseMailAndStayIdempotent(t *testing.T) {
	t.Parallel()

	storage := newTaskMailStoreStub()
	aggregate := farm.NewAggregate(42, "alice")
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: aggregate},
		WithTaskMailStore(storage),
	)
	gateway.SetClock(func() int64 {
		return 7 * gameconf.LogicDayMs(gameconf.TimeProfileDemo)
	})
	connection := &wsConnection{uid: 42, authed: true}

	firstDaily := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandClaimDailyLogin,
		Payload: emptyPayload,
	})
	if firstDaily.Err != pkgerr.OK {
		t.Fatalf("first ClaimDailyLogin err = %d, want OK", firstDaily.Err)
	}
	var dailyMail store.Mail
	if err := json.Unmarshal(firstDaily.Payload, &dailyMail); err != nil {
		t.Fatalf("decode daily mail: %v", err)
	}
	if dailyMail.ID == 0 || dailyMail.AttachmentCoin == 0 {
		t.Fatalf("daily mail = %#v, want persisted attachment", dailyMail)
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
		Tasks []store.Task `json:"tasks"`
	}
	if err := json.Unmarshal(tasks.Payload, &taskList); err != nil {
		t.Fatalf("decode TaskList: %v", err)
	}
	if tasks.Err != pkgerr.OK || len(taskList.Tasks) != 3 {
		t.Fatalf("TaskList = %#v, payload = %#v", tasks, taskList)
	}

	taskClaim := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandTaskClaim,
		Payload: json.RawMessage(`{"task_id":1}`),
	})
	if taskClaim.Err != pkgerr.OK {
		t.Fatalf("TaskClaim err = %d, want OK", taskClaim.Err)
	}
	var taskMail store.Mail
	if err := json.Unmarshal(taskClaim.Payload, &taskMail); err != nil {
		t.Fatalf("decode task mail: %v", err)
	}
	if taskMail.ID == 0 {
		t.Fatal("TaskClaim did not deliver a mail")
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
	if mails.Err != pkgerr.OK || len(mailList.Mails) != 2 {
		t.Fatalf("MailList = %#v, payload = %#v", mails, mailList)
	}

	duplicateTaskClaim := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandTaskClaim,
		Payload: json.RawMessage(`{"task_id":1}`),
	})
	if duplicateTaskClaim.Err != pkgerr.TaskAlreadyClaimed {
		t.Fatalf("repeated TaskClaim err = %d, want %d", duplicateTaskClaim.Err, pkgerr.TaskAlreadyClaimed)
	}

	firstMailClaim := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailClaim,
		Payload: marshalPayload(mailClaimRequest{MailID: taskMail.ID}),
	})
	if firstMailClaim.Err != pkgerr.OK {
		t.Fatalf("first MailClaim err = %d, want OK", firstMailClaim.Err)
	}
	if aggregate.Coin != gameconf.InitialCoin+taskMail.AttachmentCoin {
		t.Fatalf("online actor coin = %d, want %d", aggregate.Coin, gameconf.InitialCoin+taskMail.AttachmentCoin)
	}
	secondMailClaim := gateway.handleWSRequest(connection, Envelope{
		Cmd:     CommandMailClaim,
		Payload: marshalPayload(mailClaimRequest{MailID: taskMail.ID}),
	})
	if secondMailClaim.Err != pkgerr.MailAlreadyClaimed {
		t.Fatalf("repeated MailClaim err = %d, want %d", secondMailClaim.Err, pkgerr.MailAlreadyClaimed)
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
		Payload: marshalPayload(mailClaimRequest{MailID: notice.ID}),
	})
	if noAttachment.Err != pkgerr.MailNoAttachment {
		t.Fatalf("empty MailClaim err = %d, want %d", noAttachment.Err, pkgerr.MailNoAttachment)
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
	gateway.SetClock(func() int64 {
		return 7 * gameconf.LogicDayMs(gameconf.TimeProfileDemo)
	})

	response := gateway.handleWSRequest(&wsConnection{uid: 42, authed: true}, Envelope{
		Cmd:     CommandPlant,
		Payload: json.RawMessage(`{"owner_uid":0,"plot_index":0,"arg":1}`),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("Plant err = %d, want OK", response.Err)
	}
	want := taskProgressCall{uid: 42, logicDay: 7, taskID: store.TaskPlantID, amount: 1}
	if len(storage.progressCalls) != 1 || storage.progressCalls[0] != want {
		t.Fatalf("progress calls = %#v, want %#v", storage.progressCalls, want)
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
	gateway.SetClock(func() int64 {
		return 7 * gameconf.LogicDayMs(gameconf.TimeProfileDemo)
	})

	response := gateway.handleWSRequest(&wsConnection{uid: visitorUID, authed: true}, Envelope{
		Cmd:     CommandEnterFarm,
		Payload: marshalPayload(enterFarmRequest{OwnerUID: ownerUID}),
	})

	if response.Err != pkgerr.OK {
		t.Fatalf("EnterFarm err = %d, want OK", response.Err)
	}
	want := taskProgressCall{uid: visitorUID, logicDay: 7, taskID: store.TaskVisitID, amount: 1}
	if len(storage.progressCalls) != 1 || storage.progressCalls[0] != want {
		t.Fatalf("progress calls = %#v, want %#v", storage.progressCalls, want)
	}
}

func TestSuccessfulPlantReportsTaskProgressFailure(t *testing.T) {
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

	if response.Err != pkgerr.Internal {
		t.Fatalf("Plant err = %d, want %d when task progress fails", response.Err, pkgerr.Internal)
	}
}

type taskMailStoreStub struct {
	daily         map[int64]bool
	mails         map[uint64]store.Mail
	tasks         map[uint32]bool
	progressCalls []taskProgressCall
	advanceErr    error
	next          uint64
}

type taskProgressCall struct {
	uid      uint64
	logicDay int64
	taskID   uint32
	amount   uint32
}

func newTaskMailStoreStub() *taskMailStoreStub {
	return &taskMailStoreStub{
		daily: make(map[int64]bool),
		mails: make(map[uint64]store.Mail),
		tasks: make(map[uint32]bool),
	}
}

func (s *taskMailStoreStub) ListTasks(_ context.Context, _ uint64, _ int64) ([]store.Task, error) {
	return []store.Task{
		{ID: 1, Title: "stub plant", Progress: 1, Target: 1, RewardCoin: 20},
		{ID: 2, Title: "stub harvest", Progress: 1, Target: 1, RewardCoin: 30},
		{ID: 3, Title: "stub visit", Progress: 1, Target: 1, RewardCoin: 40},
	}, nil
}

func (s *taskMailStoreStub) ClaimTask(_ context.Context, _ uint64, _ int64, taskID uint32) (store.Mail, error) {
	if s.tasks[taskID] {
		return store.Mail{}, store.ErrTaskAlreadyClaimed
	}
	s.tasks[taskID] = true
	return s.addMail("task reward", 20), nil
}

func (s *taskMailStoreStub) ListMails(_ context.Context, _ uint64) ([]store.Mail, error) {
	out := make([]store.Mail, 0, len(s.mails))
	for _, mail := range s.mails {
		out = append(out, mail)
	}
	return out, nil
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
	s.mails[mailID] = mail
	return mail, nil
}

func (s *taskMailStoreStub) ClaimDailyLogin(_ context.Context, _ uint64, logicDay int64) (store.Mail, error) {
	if s.daily[logicDay] {
		return store.Mail{}, store.ErrDailyLoginAlreadyClaimed
	}
	s.daily[logicDay] = true
	return s.addMail("daily login", 100), nil
}

func (s *taskMailStoreStub) AdvanceTask(_ context.Context, uid uint64, logicDay int64, taskID, amount uint32) error {
	s.progressCalls = append(s.progressCalls, taskProgressCall{
		uid: uid, logicDay: logicDay, taskID: taskID, amount: amount,
	})
	return s.advanceErr
}

func (s *taskMailStoreStub) addMail(title string, coin int64) store.Mail {
	s.next++
	mail := store.Mail{ID: s.next, Title: title, AttachmentCoin: coin}
	s.mails[mail.ID] = mail
	return mail
}

var _ store.TaskMailStore = (*taskMailStoreStub)(nil)
