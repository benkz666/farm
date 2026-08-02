package socialapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"farm/server/api/rpc"
	"farm/server/platform/store"
)

type friendStoreStub struct{ rows []store.FriendRow }

func (stub *friendStoreStub) AreFriends(context.Context, uint64, uint64) (bool, error) {
	return true, nil
}
func (stub *friendStoreStub) AddFriends(context.Context, uint64, uint64) error    { return nil }
func (stub *friendStoreStub) RemoveFriends(context.Context, uint64, uint64) error { return nil }
func (stub *friendStoreStub) ListFriends(context.Context, uint64) ([]store.FriendRow, error) {
	return stub.rows, nil
}
func (stub *friendStoreStub) CountFriends(context.Context, uint64) (int, error) {
	return len(stub.rows), nil
}
func (stub *friendStoreStub) FindUserByUsername(context.Context, string) (store.UserSearchRow, error) {
	return store.UserSearchRow{}, nil
}
func (stub *friendStoreStub) CreateFriendRequest(context.Context, uint64, uint64) error { return nil }
func (stub *friendStoreStub) ListIncomingFriendRequests(context.Context, uint64) ([]store.FriendRequestRow, error) {
	return nil, nil
}
func (stub *friendStoreStub) AcceptFriendRequest(context.Context, uint64, uint64) error { return nil }
func (stub *friendStoreStub) RejectFriendRequest(context.Context, uint64, uint64) error { return nil }

func TestLargeFriendUIDsRemainDistinct(t *testing.T) {
	backend := &friendStoreStub{rows: []store.FriendRow{
		{UID: 9007199254740992, Nickname: "A"},
		{UID: 9007199254740993, Nickname: "B"},
	}}
	server := httptest.NewServer(rpc.NewHandler("secret", NewDispatcher(backend)))
	defer server.Close()

	rows, err := NewClient(server.URL, "secret").ListFriends(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListFriends() error = %v", err)
	}
	if len(rows) != 2 || rows[0].UID == rows[1].UID {
		t.Fatalf("ListFriends() = %#v", rows)
	}
}
