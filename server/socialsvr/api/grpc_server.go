package api

import (
	"context"
	"sync"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/rpcerr"
	"farm/server/shared/store"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type invalidationHub struct {
	mu       sync.RWMutex
	watchers map[uint64]map[chan *farmv1.FriendInvalidation]struct{}
}

func newInvalidationHub() *invalidationHub {
	return &invalidationHub{watchers: make(map[uint64]map[chan *farmv1.FriendInvalidation]struct{})}
}

func (hub *invalidationHub) subscribe(uid uint64) (chan *farmv1.FriendInvalidation, func()) {
	ch := make(chan *farmv1.FriendInvalidation, 8)
	hub.mu.Lock()
	if hub.watchers[uid] == nil {
		hub.watchers[uid] = make(map[chan *farmv1.FriendInvalidation]struct{})
	}
	hub.watchers[uid][ch] = struct{}{}
	hub.mu.Unlock()
	return ch, func() {
		hub.mu.Lock()
		delete(hub.watchers[uid], ch)
		if len(hub.watchers[uid]) == 0 {
			delete(hub.watchers, uid)
		}
		hub.mu.Unlock()
		close(ch)
	}
}

func (hub *invalidationHub) broadcast(uid, peerUID uint64) {
	msg := &farmv1.FriendInvalidation{Uid: uid, PeerUid: peerUID}
	targets := []uint64{uid, peerUID, 0}
	seen := make(map[uint64]struct{}, len(targets))
	for _, target := range targets {
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		hub.mu.RLock()
		subs := hub.watchers[target]
		channels := make([]chan *farmv1.FriendInvalidation, 0, len(subs))
		for ch := range subs {
			channels = append(channels, ch)
		}
		hub.mu.RUnlock()
		for _, ch := range channels {
			select {
			case ch <- msg:
			default:
			}
		}
	}
}

// GRPCServer implements SocialService over the local FriendStore.
type GRPCServer struct {
	farmv1.UnimplementedSocialServiceServer
	store store.FriendStore
	hub   *invalidationHub
}

// NewGRPCServer constructs the typed Social gRPC adapter.
func NewGRPCServer(friendStore store.FriendStore) *GRPCServer {
	return &GRPCServer{store: friendStore, hub: newInvalidationHub()}
}

// RegisterGRPC registers SocialService on a gRPC server and returns the adapter
// so the process can attach its distributed invalidation subscriber.
func RegisterGRPC(server *grpc.Server, friendStore store.FriendStore) *GRPCServer {
	adapter := NewGRPCServer(friendStore)
	farmv1.RegisterSocialServiceServer(server, adapter)
	return adapter
}

// StartDistributedInvalidations forwards changes received by this Social
// replica to the Gateway/Farm streams connected to the same replica.
func (server *GRPCServer) StartDistributedInvalidations(ctx context.Context) {
	source, ok := server.store.(store.FriendInvalidationSource)
	if !ok {
		return
	}
	go source.WatchFriendInvalidations(ctx, server.hub.broadcast)
}

func (server *GRPCServer) AreFriends(ctx context.Context, request *farmv1.AreFriendsRequest) (*farmv1.AreFriendsResponse, error) {
	if server == nil || server.store == nil || request == nil || request.Uid == 0 || request.PeerUid == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	value, err := server.store.AreFriends(ctx, request.Uid, request.PeerUid)
	if err != nil {
		return nil, rpcerr.GRPCStatus(err)
	}
	return &farmv1.AreFriendsResponse{Value: value}, nil
}

func (server *GRPCServer) AddFriends(ctx context.Context, request *farmv1.PairRequest) (*farmv1.Empty, error) {
	resp, err := server.pairMutation(ctx, request, server.store.AddFriends)
	if err == nil {
		server.hub.broadcast(request.Uid, request.PeerUid)
	}
	return resp, err
}

func (server *GRPCServer) RemoveFriends(ctx context.Context, request *farmv1.PairRequest) (*farmv1.Empty, error) {
	resp, err := server.pairMutation(ctx, request, server.store.RemoveFriends)
	if err == nil {
		server.hub.broadcast(request.Uid, request.PeerUid)
	}
	return resp, err
}

func (server *GRPCServer) ListFriends(ctx context.Context, request *farmv1.UidRequest) (*farmv1.ListFriendsResponse, error) {
	if server == nil || server.store == nil || request == nil || request.Uid == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	rows, err := server.store.ListFriends(ctx, request.Uid)
	if err != nil {
		return nil, rpcerr.GRPCStatus(err)
	}
	friends := make([]*farmv1.Friend, 0, len(rows))
	for _, row := range rows {
		friends = append(friends, &farmv1.Friend{Uid: row.UID, Nickname: row.Nickname})
	}
	return &farmv1.ListFriendsResponse{Friends: friends}, nil
}

func (server *GRPCServer) CountFriends(ctx context.Context, request *farmv1.UidRequest) (*farmv1.CountFriendsResponse, error) {
	if server == nil || server.store == nil || request == nil || request.Uid == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	count, err := server.store.CountFriends(ctx, request.Uid)
	if err != nil {
		return nil, rpcerr.GRPCStatus(err)
	}
	return &farmv1.CountFriendsResponse{Count: int32(count)}, nil
}

func (server *GRPCServer) FindUser(ctx context.Context, request *farmv1.FindUserRequest) (*farmv1.Friend, error) {
	if server == nil || server.store == nil || request == nil || request.Username == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	row, err := server.store.FindUserByUsername(ctx, request.Username)
	if err != nil {
		return nil, rpcerr.GRPCStatus(err)
	}
	return &farmv1.Friend{Uid: row.UID, Nickname: row.Nickname}, nil
}

func (server *GRPCServer) CreateFriendRequest(ctx context.Context, request *farmv1.PairRequest) (*farmv1.Empty, error) {
	resp, err := server.pairMutation(ctx, request, server.store.CreateFriendRequest)
	if err == nil {
		// A reverse pending request implicitly accepts and creates the relation.
		// Broadcasting for the ordinary pending case is a harmless invalidation.
		server.hub.broadcast(request.Uid, request.PeerUid)
	}
	return resp, err
}

func (server *GRPCServer) ListIncomingFriendRequests(ctx context.Context, request *farmv1.UidRequest) (*farmv1.ListFriendRequestsResponse, error) {
	if server == nil || server.store == nil || request == nil || request.Uid == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	rows, err := server.store.ListIncomingFriendRequests(ctx, request.Uid)
	if err != nil {
		return nil, rpcerr.GRPCStatus(err)
	}
	requests := make([]*farmv1.FriendRequest, 0, len(rows))
	for _, row := range rows {
		requests = append(requests, &farmv1.FriendRequest{
			FromUid:   row.FromUID,
			Nickname:  row.Nickname,
			CreatedAt: row.CreatedAt,
		})
	}
	return &farmv1.ListFriendRequestsResponse{Requests: requests}, nil
}

func (server *GRPCServer) AcceptFriendRequest(ctx context.Context, request *farmv1.PairRequest) (*farmv1.Empty, error) {
	resp, err := server.pairMutation(ctx, request, server.store.AcceptFriendRequest)
	if err == nil {
		server.hub.broadcast(request.Uid, request.PeerUid)
	}
	return resp, err
}

func (server *GRPCServer) RejectFriendRequest(ctx context.Context, request *farmv1.PairRequest) (*farmv1.Empty, error) {
	return server.pairMutation(ctx, request, server.store.RejectFriendRequest)
}

func (server *GRPCServer) WatchFriendInvalidations(request *farmv1.UidRequest, stream farmv1.SocialService_WatchFriendInvalidationsServer) error {
	if server == nil || request == nil {
		return status.Error(codes.InvalidArgument, "bad_request")
	}
	ch, unsubscribe := server.hub.subscribe(request.Uid)
	defer unsubscribe()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

type pairFn func(context.Context, uint64, uint64) error

func (server *GRPCServer) pairMutation(ctx context.Context, request *farmv1.PairRequest, mutate pairFn) (*farmv1.Empty, error) {
	if server == nil || server.store == nil || request == nil || request.Uid == 0 || request.PeerUid == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request")
	}
	if err := mutate(ctx, request.Uid, request.PeerUid); err != nil {
		return nil, rpcerr.GRPCStatus(err)
	}
	return &farmv1.Empty{}, nil
}
