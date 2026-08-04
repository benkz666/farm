package api

import (
	"context"
	"testing"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"
	"farm/server/shared/store"

	"google.golang.org/grpc"
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
	pair := grpcx.NewBufconnPair(t, "secret", func(server *grpc.Server) {
		RegisterGRPC(server, backend)
	})

	rows, err := NewGRPCClient(pair.Pool, "bufconn").ListFriends(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListFriends() error = %v", err)
	}
	if len(rows) != 2 || rows[0].UID == rows[1].UID {
		t.Fatalf("ListFriends() = %#v", rows)
	}
}

func TestWatchFriendInvalidationsUnimplemented(t *testing.T) {
	pair := grpcx.NewBufconnPair(t, "secret", func(server *grpc.Server) {
		RegisterGRPC(server, &friendStoreStub{})
	})
	conn, err := pair.Pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	stream, err := farmv1.NewSocialServiceClient(conn).WatchFriendInvalidations(context.Background(), &farmv1.UidRequest{Uid: 1})
	if err == nil {
		_, recvErr := stream.Recv()
		if recvErr == nil {
			t.Fatal("expected unimplemented stream error")
		}
	}
}
