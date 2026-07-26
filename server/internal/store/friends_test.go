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

func TestFriendStoreAddListAndRemove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ownerUID := createFriendTestAccount(t, s)
	friendUID := createFriendTestAccount(t, s)

	if err := s.AddFriends(ctx, ownerUID, friendUID); err != nil {
		t.Fatalf("AddFriends: %v", err)
	}
	if err := s.AddFriends(ctx, friendUID, ownerUID); !errors.Is(err, store.ErrAlreadyFriend) {
		t.Fatalf("duplicate AddFriends error = %v, want ErrAlreadyFriend", err)
	}

	for _, pair := range [][2]uint64{{ownerUID, friendUID}, {friendUID, ownerUID}} {
		got, err := s.AreFriends(ctx, pair[0], pair[1])
		if err != nil {
			t.Fatalf("AreFriends(%d, %d): %v", pair[0], pair[1], err)
		}
		if !got {
			t.Fatalf("AreFriends(%d, %d) = false, want true", pair[0], pair[1])
		}
	}

	count, err := s.CountFriends(ctx, ownerUID)
	if err != nil {
		t.Fatalf("CountFriends: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountFriends = %d, want 1", count)
	}

	friends, err := s.ListFriends(ctx, ownerUID)
	if err != nil {
		t.Fatalf("ListFriends: %v", err)
	}
	wantNickname := "it_friend_" + strconv.FormatUint(friendUID, 10)
	if len(friends) != 1 || friends[0].UID != friendUID || friends[0].Nickname != wantNickname {
		t.Fatalf("ListFriends = %#v, want uid=%d nickname=%q", friends, friendUID, wantNickname)
	}

	if err := s.RemoveFriends(ctx, ownerUID, friendUID); err != nil {
		t.Fatalf("RemoveFriends: %v", err)
	}
	got, err := s.AreFriends(ctx, ownerUID, friendUID)
	if err != nil {
		t.Fatalf("AreFriends after RemoveFriends: %v", err)
	}
	if got {
		t.Fatal("AreFriends after RemoveFriends = true, want false")
	}
}

func TestFriendStoreEnforcesFriendLimit(t *testing.T) {
	originalLimit := gameconf.FriendLimit
	gameconf.FriendLimit = 1
	t.Cleanup(func() {
		gameconf.FriendLimit = originalLimit
	})

	s := newTestStore(t)
	ctx := context.Background()
	ownerUID := createFriendTestAccount(t, s)
	limitedFriendUID := createFriendTestAccount(t, s)
	anotherUID := createFriendTestAccount(t, s)

	if err := s.AddFriends(ctx, ownerUID, limitedFriendUID); err != nil {
		t.Fatalf("first AddFriends: %v", err)
	}
	if err := s.AddFriends(ctx, ownerUID, anotherUID); !errors.Is(err, store.ErrFriendLimitSelf) {
		t.Fatalf("AddFriends past owner limit error = %v, want ErrFriendLimitSelf", err)
	}
	if err := s.AddFriends(ctx, anotherUID, limitedFriendUID); !errors.Is(err, store.ErrFriendLimitPeer) {
		t.Fatalf("AddFriends past peer limit error = %v, want ErrFriendLimitPeer", err)
	}
}

func createFriendTestAccount(t *testing.T, s *store.Store) uint64 {
	t.Helper()

	uid := testUID(t)
	if err := s.SaveAccount(context.Background(), uid, "it_friend_"+strconv.FormatUint(uid, 10), "hash"); err != nil {
		t.Fatalf("SaveAccount(%d): %v", uid, err)
	}
	return uid
}
