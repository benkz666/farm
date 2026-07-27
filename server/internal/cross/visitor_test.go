package cross

import (
	"testing"

	"farm/server/internal/pkgerr"
)

func TestVisitorExpiresReservedActionAfterFiveSeconds(t *testing.T) {
	visitor := NewVisitor()
	action := CrossAction{
		ReqID:      101,
		Kind:       Water,
		VisitorUID: 7,
		OwnerUID:   9,
		PlotIndex:  2,
	}

	pending, err := visitor.Reserve(action)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	expired := visitor.Expire(pending.ReservedAt.Add(PendingTimeout))
	if len(expired) != 1 {
		t.Fatalf("Expire count = %d, want 1", len(expired))
	}
	if expired[0].State != RolledBack {
		t.Fatalf("expired state = %q, want %q", expired[0].State, RolledBack)
	}
	if expired[0].Result.Code != pkgerr.Timeout {
		t.Fatalf("expired code = %d, want %d", expired[0].Result.Code, pkgerr.Timeout)
	}
	if _, ok := visitor.Pending(action.ReqID); ok {
		t.Fatal("timed-out reservation must be removed from pending")
	}
}

func TestVisitorIgnoresDuplicateResult(t *testing.T) {
	visitor := NewVisitor()
	action := CrossAction{
		ReqID:      202,
		Kind:       RemoveWeed,
		VisitorUID: 7,
		OwnerUID:   9,
		PlotIndex:  3,
	}
	if _, err := visitor.Reserve(action); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	result := CrossResult{
		ReqID:      action.ReqID,
		VisitorUID: action.VisitorUID,
		OwnerUID:   action.OwnerUID,
		Code:       pkgerr.OK,
	}

	settled, applied, err := visitor.Settle(result)
	if err != nil {
		t.Fatalf("first Settle: %v", err)
	}
	if !applied {
		t.Fatal("first result must be applied")
	}
	if settled.State != Settled {
		t.Fatalf("first result state = %q, want %q", settled.State, Settled)
	}

	duplicate, applied, err := visitor.Settle(result)
	if err != nil {
		t.Fatalf("duplicate Settle: %v", err)
	}
	if applied {
		t.Fatal("duplicate result must not be applied twice")
	}
	if duplicate.Action.ReqID != 0 {
		t.Fatalf("duplicate result returned pending = %#v, want zero value", duplicate)
	}
}
