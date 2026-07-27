package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"farm/server/internal/farm"
	"farm/server/internal/gameconf"
	"farm/server/internal/pkgerr"
	"farm/server/internal/store"
)

func TestDailyLoginAndTaskRewardsUseMailAndStayIdempotent(t *testing.T) {
	t.Parallel()

	storage := newTaskMailStoreStub()
	gateway := New(
		authStub{},
		sessionStub{uid: 42},
		runtimeStub{aggregate: farm.NewAggregate(42, "alice")},
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

type taskMailStoreStub struct {
	daily map[int64]bool
	mails map[uint64]store.Mail
	tasks map[uint32]bool
	next  uint64
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

func (s *taskMailStoreStub) addMail(title string, coin int64) store.Mail {
	s.next++
	mail := store.Mail{ID: s.next, Title: title, AttachmentCoin: coin}
	s.mails[mail.ID] = mail
	return mail
}

var _ store.TaskMailStore = (*taskMailStoreStub)(nil)
