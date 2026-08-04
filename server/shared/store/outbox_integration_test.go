//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/outbox"
)

func TestCommitFarmsAndOutboxShareTransaction(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	uid := testUID(t)
	if err := s.SaveAccount(ctx, uid, "outbox_tx_"+time.Now().Format("150405.000"), "hash"); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	agg, err := s.LoadFarm(ctx, uid)
	if err != nil {
		t.Fatalf("LoadFarm: %v", err)
	}
	agg.Coin = 2_000
	event, err := outbox.NewCrossResultEvent(uid, &farmv1.CrossResult{
		ReqId: 99, VisitorUid: uid + 1, OwnerUid: uid, Code: 0,
	})
	if err != nil {
		t.Fatalf("NewCrossResultEvent: %v", err)
	}
	t.Cleanup(func() {
		_ = s.MarkOutboxPublished(context.Background(), event.EventID)
	})
	if err := s.CommitFarms(ctx, []outbox.FarmCommit{{Snapshot: agg, Outbox: []outbox.Event{event}}}); err != nil {
		t.Fatalf("CommitFarms: %v", err)
	}
	reloaded, err := s.LoadFarm(ctx, uid)
	if err != nil || reloaded.Coin != 2_000 {
		t.Fatalf("reload coin = %d err=%v", reloaded.Coin, err)
	}
	if immediate, err := s.ClaimDueOutbox(ctx, 4, time.Now().UnixMilli()); err != nil || len(immediate) != 0 {
		t.Fatalf("immediate claim = %#v err=%v, want delayed fallback", immediate, err)
	}
	rows, err := s.ClaimDueOutbox(ctx, 4, time.Now().Add(time.Second).UnixMilli())
	if err != nil || len(rows) != 1 || rows[0].EventID != event.EventID {
		t.Fatalf("claimed = %#v err=%v", rows, err)
	}
}

func TestOutboxDuplicateInsertIgnored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	event := outbox.Event{
		EventID: "cross_result:1:2:5", ProducerUID: 1, TargetUID: 2,
		Kind: outbox.KindCrossResult, Payload: []byte("payload"),
	}
	t.Cleanup(func() {
		_ = s.MarkOutboxPublished(context.Background(), event.EventID)
	})
	if err := s.InsertOutboxEvents(ctx, []outbox.Event{event}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := s.InsertOutboxEvents(ctx, []outbox.Event{event}); err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
}

func TestOutboxDeadLetterIsNotClaimedAgain(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	event := outbox.Event{
		EventID:     "cross_result:dead:letter:" + time.Now().Format("150405.000000"),
		ProducerUID: 1, TargetUID: 2,
		Kind: outbox.KindCrossResult, Payload: []byte("payload"),
	}
	t.Cleanup(func() {
		_ = s.MarkOutboxPublished(context.Background(), event.EventID)
	})
	if err := s.InsertOutboxEvents(ctx, []outbox.Event{event}); err != nil {
		t.Fatalf("InsertOutboxEvents: %v", err)
	}
	if err := s.MarkOutboxDeadLetter(ctx, event.EventID, dispatcherMaxAttemptsForTest); err != nil {
		t.Fatalf("MarkOutboxDeadLetter: %v", err)
	}
	rows, err := s.ClaimDueOutbox(ctx, 4, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("ClaimDueOutbox: %v", err)
	}
	for _, row := range rows {
		if row.EventID == event.EventID {
			t.Fatalf("dead-letter event %q was claimed again", event.EventID)
		}
	}
}

const dispatcherMaxAttemptsForTest = 100
