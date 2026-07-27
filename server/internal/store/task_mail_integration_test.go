//go:build integration

package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"farm/server/internal/gameconf"
	"farm/server/internal/store"
)

func TestTaskMailStoreClaimsRewardsExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "it_task_mail_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	before, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm before claim: %v", err)
	}

	const logicDay = int64(9)
	tasks, err := s.ListTasks(ctx, uid, logicDay)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("ListTasks returned %d tasks, want 3", len(tasks))
	}
	if _, err := s.ClaimTask(ctx, uid, logicDay, store.TaskPlantID); !errors.Is(err, store.ErrTaskNotComplete) {
		t.Fatalf("ClaimTask before gameplay error = %v, want ErrTaskNotComplete", err)
	}
	if err := s.AdvanceTask(ctx, uid, logicDay, store.TaskPlantID, 1); err != nil {
		t.Fatalf("AdvanceTask: %v", err)
	}

	taskMail, err := s.ClaimTask(ctx, uid, logicDay, store.TaskPlantID)
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if taskMail.ID == 0 || taskMail.AttachmentCoin == 0 {
		t.Fatalf("task reward mail = %#v", taskMail)
	}
	if _, err := s.ClaimTask(ctx, uid, logicDay, store.TaskPlantID); !errors.Is(err, store.ErrTaskAlreadyClaimed) {
		t.Fatalf("second ClaimTask error = %v, want ErrTaskAlreadyClaimed", err)
	}

	dailyMail, err := s.ClaimDailyLogin(ctx, uid, logicDay)
	if err != nil {
		t.Fatalf("ClaimDailyLogin: %v", err)
	}
	if _, err := s.ClaimDailyLogin(ctx, uid, logicDay); !errors.Is(err, store.ErrDailyLoginAlreadyClaimed) {
		t.Fatalf("second ClaimDailyLogin error = %v, want ErrDailyLoginAlreadyClaimed", err)
	}

	mails, err := s.ListMails(ctx, uid)
	if err != nil {
		t.Fatalf("ListMails: %v", err)
	}
	if len(mails) != 2 {
		t.Fatalf("ListMails returned %d mails, want 2", len(mails))
	}
	if _, err := s.ClaimMail(ctx, uid, taskMail.ID); err != nil {
		t.Fatalf("ClaimMail: %v", err)
	}
	if _, err := s.ClaimMail(ctx, uid, taskMail.ID); !errors.Is(err, store.ErrMailAlreadyClaimed) {
		t.Fatalf("second ClaimMail error = %v, want ErrMailAlreadyClaimed", err)
	}

	after, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm after claim: %v", err)
	}
	if after.Coin != before.Coin+taskMail.AttachmentCoin {
		t.Fatalf("coin after mail claim = %d, want %d", after.Coin, before.Coin+taskMail.AttachmentCoin)
	}
	if dailyMail.AttachmentCoin != 100 || before.Coin != gameconf.InitialCoin {
		t.Fatalf("daily mail = %#v, initial coin = %d", dailyMail, before.Coin)
	}
}
