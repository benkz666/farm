//go:build integration

package store_test

import (
	"context"
	"testing"

	"farm/server/internal/store"
)

func TestStealHintSetAndReadRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ownerUID := testUID(t)
	otherUID := testUID(t)

	hints, err := s.GetStealHints(ctx, []uint64{ownerUID, otherUID})
	if err != nil {
		t.Fatalf("GetStealHints empty: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("GetStealHints on empty = %#v, want no entries", hints)
	}

	if err := s.SetStealHint(ctx, ownerUID, true); err != nil {
		t.Fatalf("SetStealHint true: %v", err)
	}

	hints, err = s.GetStealHints(ctx, []uint64{ownerUID, otherUID})
	if err != nil {
		t.Fatalf("GetStealHints after set: %v", err)
	}
	if !hints[ownerUID] {
		t.Fatalf("GetStealHints = %#v, want ownerUID=true", hints)
	}
	if hints[otherUID] {
		t.Fatalf("GetStealHints = %#v, want otherUID absent/false", hints)
	}

	if err := s.SetStealHint(ctx, ownerUID, false); err != nil {
		t.Fatalf("SetStealHint false: %v", err)
	}
	hints, err = s.GetStealHints(ctx, []uint64{ownerUID})
	if err != nil {
		t.Fatalf("GetStealHints after clear: %v", err)
	}
	if hints[ownerUID] {
		t.Fatalf("GetStealHints after clear = %#v, want ownerUID=false", hints)
	}
}

func TestStealHintStoreImplementsInterface(t *testing.T) {
	var _ store.StealHintStore = (*store.Store)(nil)
}
