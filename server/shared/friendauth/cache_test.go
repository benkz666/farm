package friendauth

import (
	"context"
	"errors"
	"testing"
	"time"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/store"
)

type friendStub struct {
	value       bool
	calls       int
	listRows    []store.FriendRow
	listCalls   int
	searchRow   store.UserSearchRow
	searchErr   error
	searchCalls int
}

func (s *friendStub) AreFriends(context.Context, uint64, uint64) (bool, error) {
	s.calls++
	return s.value, nil
}

func (s *friendStub) AddFriends(context.Context, uint64, uint64) error    { return nil }
func (s *friendStub) RemoveFriends(context.Context, uint64, uint64) error { return nil }
func (s *friendStub) ListFriends(context.Context, uint64) ([]store.FriendRow, error) {
	s.listCalls++
	return append([]store.FriendRow(nil), s.listRows...), nil
}
func (s *friendStub) CountFriends(context.Context, uint64) (int, error) { return 0, nil }
func (s *friendStub) FindUserByUsername(context.Context, string) (store.UserSearchRow, error) {
	s.searchCalls++
	return s.searchRow, s.searchErr
}
func (s *friendStub) CreateFriendRequest(context.Context, uint64, uint64) error { return nil }
func (s *friendStub) ListIncomingFriendRequests(context.Context, uint64) ([]store.FriendRequestRow, error) {
	return nil, nil
}
func (s *friendStub) AcceptFriendRequest(context.Context, uint64, uint64) error { return nil }
func (s *friendStub) RejectFriendRequest(context.Context, uint64, uint64) error { return nil }

func TestCacheOnlyStoresTrueResults(t *testing.T) {
	stub := &friendStub{value: true}
	cache := NewCache(stub)
	cache.ttl = time.Second

	ok, err := cache.AreFriends(context.Background(), 1, 2)
	if err != nil || !ok {
		t.Fatalf("first AreFriends = %v err=%v", ok, err)
	}
	ok, err = cache.AreFriends(context.Background(), 1, 2)
	if err != nil || !ok {
		t.Fatalf("cached AreFriends = %v err=%v", ok, err)
	}
	if stub.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", stub.calls)
	}
}

func TestCacheInvalidateDropsTrueEntry(t *testing.T) {
	stub := &friendStub{value: true}
	cache := NewCache(stub)
	if _, err := cache.AreFriends(context.Background(), 3, 4); err != nil {
		t.Fatalf("AreFriends: %v", err)
	}
	cache.Invalidate(3, 4)
	if _, err := cache.AreFriends(context.Background(), 3, 4); err != nil {
		t.Fatalf("second AreFriends: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("inner calls = %d, want 2", stub.calls)
	}
}

func TestCacheDoesNotStoreFalse(t *testing.T) {
	stub := &friendStub{value: false}
	cache := NewCache(stub)
	ok, err := cache.AreFriends(context.Background(), 5, 6)
	if err != nil || ok {
		t.Fatalf("false AreFriends = %v err=%v", ok, err)
	}
	stub.value = true
	ok, err = cache.AreFriends(context.Background(), 5, 6)
	if err != nil || !ok {
		t.Fatalf("second AreFriends = %v err=%v", ok, err)
	}
	if stub.calls != 2 {
		t.Fatalf("inner calls = %d, want 2", stub.calls)
	}
}

func TestWatchInvalidationsAllowsWildcardUID(t *testing.T) {
	stub := &friendStub{value: true}
	cache := NewCache(stub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan uint64, 1)
	invalidations := make(chan *farmv1.FriendInvalidation, 1)
	go cache.WatchInvalidations(ctx, 0, func(_ context.Context, uid uint64) (<-chan *farmv1.FriendInvalidation, error) {
		started <- uid
		return invalidations, nil
	})

	select {
	case uid := <-started:
		if uid != 0 {
			t.Fatalf("watch uid = %d, want 0", uid)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not start for uid=0")
	}

	if _, err := cache.AreFriends(context.Background(), 10, 20); err != nil {
		t.Fatalf("AreFriends: %v", err)
	}
	invalidations <- &farmv1.FriendInvalidation{Uid: 10, PeerUid: 20}
	time.Sleep(20 * time.Millisecond)
	if _, err := cache.AreFriends(context.Background(), 10, 20); err != nil {
		t.Fatalf("second AreFriends: %v", err)
	}
	if stub.calls != 2 {
		t.Fatalf("invalidation did not drop cache; inner calls = %d", stub.calls)
	}
}

func TestCacheEvictsWhenAtCapacity(t *testing.T) {
	stub := &friendStub{value: true}
	cache := NewCache(stub)
	cache.capacity = cacheShardCount
	cache.ttl = time.Hour

	for i := uint64(1); i <= cacheShardCount+1; i++ {
		if _, err := cache.AreFriends(context.Background(), i, i+100); err != nil {
			t.Fatalf("AreFriends pair %d: %v", i, err)
		}
	}
	size := cache.entryCount()
	if size > cacheShardCount {
		t.Fatalf("entries = %d, want <= %d", size, cacheShardCount)
	}
}

func TestListFriendsCachesCopyAndInvalidatesBothUIDs(t *testing.T) {
	stub := &friendStub{listRows: []store.FriendRow{{UID: 2, Nickname: "peer"}}}
	cache := NewCache(stub)

	first, err := cache.ListFriends(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListFriends: %v", err)
	}
	first[0].Nickname = "mutated"
	second, err := cache.ListFriends(context.Background(), 1)
	if err != nil {
		t.Fatalf("cached ListFriends: %v", err)
	}
	if stub.listCalls != 1 || second[0].Nickname != "peer" {
		t.Fatalf("cached rows=%#v calls=%d", second, stub.listCalls)
	}

	cache.Invalidate(1, 2)
	if _, err := cache.ListFriends(context.Background(), 1); err != nil {
		t.Fatalf("ListFriends after invalidation: %v", err)
	}
	if stub.listCalls != 2 {
		t.Fatalf("list calls=%d, want 2", stub.listCalls)
	}
}

func TestFindUserCachesPositiveAndNotFound(t *testing.T) {
	stub := &friendStub{searchRow: store.UserSearchRow{UID: 9, Nickname: "nine"}}
	cache := NewCache(stub)
	for range 2 {
		row, err := cache.FindUserByUsername(context.Background(), "user9")
		if err != nil || row.UID != 9 {
			t.Fatalf("FindUserByUsername = %#v err=%v", row, err)
		}
	}
	if stub.searchCalls != 1 {
		t.Fatalf("positive search calls=%d, want 1", stub.searchCalls)
	}

	stub.searchErr = store.ErrAccountNotFound
	for range 2 {
		_, err := cache.FindUserByUsername(context.Background(), "missing")
		if !errors.Is(err, store.ErrAccountNotFound) {
			t.Fatalf("missing error=%v", err)
		}
	}
	if stub.searchCalls != 2 {
		t.Fatalf("total search calls=%d, want 2", stub.searchCalls)
	}
}
