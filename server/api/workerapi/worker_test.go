package workerapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"farm/server/api/rpc"
	"farm/server/platform/farm"
	"farm/server/platform/store"
)

type workerStub struct{ mailID uint64 }

func (stub workerStub) ListTasks(context.Context, uint64, int64) ([]store.Task, error) {
	return nil, nil
}
func (stub workerStub) AdvanceTask(context.Context, uint64, int64, uint32, uint32) (store.TaskAdvanceResult, error) {
	return store.TaskAdvanceResult{}, nil
}
func (stub workerStub) ClaimTask(context.Context, uint64, int64, uint32) (store.TaskReward, error) {
	return store.TaskReward{}, nil
}
func (stub workerStub) ListMails(context.Context, uint64) ([]store.Mail, error) {
	return []store.Mail{{ID: stub.mailID}}, nil
}
func (stub workerStub) MarkMailsRead(context.Context, uint64, uint64) (int64, error) { return 0, nil }
func (stub workerStub) DeleteMails(context.Context, uint64, uint64) (int64, error)   { return 0, nil }
func (stub workerStub) ClaimMail(context.Context, uint64, uint64) (store.Mail, error) {
	return store.Mail{}, nil
}
func (stub workerStub) ClaimDailyLogin(context.Context, uint64, int64) (store.TaskReward, error) {
	return store.TaskReward{}, nil
}
func (stub workerStub) IssueCodexRewards(context.Context, uint64, farm.CodexProgress) ([]farm.CodexRewardNotice, error) {
	return nil, nil
}

func TestMailIDDoesNotLoseJSONPrecision(t *testing.T) {
	want := uint64(9007199254740993)
	server := httptest.NewServer(rpc.NewHandler("secret", NewDispatcher(workerStub{mailID: want})))
	defer server.Close()

	mails, err := NewClient(server.URL, "secret").ListMails(context.Background(), 9007199254740992)
	if err != nil {
		t.Fatalf("ListMails() error = %v", err)
	}
	if len(mails) != 1 || mails[0].ID != want {
		t.Fatalf("ListMails() = %#v", mails)
	}
}
