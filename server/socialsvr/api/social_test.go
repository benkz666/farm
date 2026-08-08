package api

import (
	"context"
	"testing"
	"time"

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

type distributedFriendStoreStub struct {
	*friendStoreStub
	started chan struct{}
}

func (stub *distributedFriendStoreStub) WatchFriendInvalidations(ctx context.Context, notify func(uint64, uint64)) {
	close(stub.started)
	notify(101, 202)
	<-ctx.Done()
}

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

func TestWatchFriendInvalidationsStopsWhenContextIsCancelled(t *testing.T) {
	pair := grpcx.NewBufconnPair(t, "secret", func(server *grpc.Server) {
		RegisterGRPC(server, &friendStoreStub{})
	})
	conn, err := pair.Pool.Conn(context.Background(), "bufconn")
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := farmv1.NewSocialServiceClient(conn).WatchFriendInvalidations(ctx, &farmv1.UidRequest{Uid: 1})
	if err != nil {
		t.Fatalf("WatchFriendInvalidations: %v", err)
	}
	cancel()
	if _, recvErr := stream.Recv(); recvErr == nil {
		t.Fatal("Recv after cancellation unexpectedly succeeded")
	}
}

func TestCreateFriendRequestBroadcastsPossibleMutualAcceptInvalidation(t *testing.T) {
	server := NewGRPCServer(&friendStoreStub{})
	invalidations, unsubscribe := server.hub.subscribe(0)
	defer unsubscribe()

	if _, err := server.CreateFriendRequest(t.Context(), &farmv1.PairRequest{Uid: 11, PeerUid: 22}); err != nil {
		t.Fatalf("CreateFriendRequest: %v", err)
	}
	select {
	case invalidation := <-invalidations:
		if invalidation.Uid != 11 || invalidation.PeerUid != 22 {
			t.Fatalf("invalidation = %#v", invalidation)
		}
	case <-time.After(time.Second):
		t.Fatal("CreateFriendRequest did not broadcast invalidation")
	}
}

func TestDistributedInvalidationIsForwardedToLocalWatchers(t *testing.T) {
	backend := &distributedFriendStoreStub{
		friendStoreStub: &friendStoreStub{},
		started:         make(chan struct{}),
	}
	server := NewGRPCServer(backend)
	invalidations, unsubscribe := server.hub.subscribe(0)
	defer unsubscribe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server.StartDistributedInvalidations(ctx)

	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("distributed invalidation watcher did not start")
	}
	select {
	case invalidation := <-invalidations:
		if invalidation.Uid != 101 || invalidation.PeerUid != 202 {
			t.Fatalf("invalidation = %#v", invalidation)
		}
	case <-time.After(time.Second):
		t.Fatal("distributed invalidation was not forwarded")
	}
}
