package api

import (
	"context"

	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/grpcx"
	"farm/server/shared/rpcerr"
	"farm/server/shared/store"
)

// GRPCClient implements FriendStore over SocialService.
type GRPCClient struct {
	target string
	pool   *grpcx.Pool
}

// NewGRPCClient constructs a Social gRPC client.
func NewGRPCClient(pool *grpcx.Pool, target string) *GRPCClient {
	return &GRPCClient{pool: pool, target: target}
}

func (client *GRPCClient) service(ctx context.Context) (farmv1.SocialServiceClient, error) {
	conn, err := client.pool.Conn(ctx, client.target)
	if err != nil {
		return nil, err
	}
	return farmv1.NewSocialServiceClient(conn), nil
}

func (client *GRPCClient) AreFriends(ctx context.Context, uid, peerUID uint64) (bool, error) {
	service, err := client.service(ctx)
	if err != nil {
		return false, err
	}
	response, err := service.AreFriends(ctx, &farmv1.AreFriendsRequest{Uid: uid, PeerUid: peerUID})
	if err != nil {
		return false, rpcerr.FromGRPC(err)
	}
	return response.Value, nil
}

func (client *GRPCClient) AddFriends(ctx context.Context, uid, peerUID uint64) error {
	return client.pair(ctx, uid, peerUID, func(ctx context.Context, service farmv1.SocialServiceClient) error {
		_, err := service.AddFriends(ctx, &farmv1.PairRequest{Uid: uid, PeerUid: peerUID})
		return err
	})
}

func (client *GRPCClient) RemoveFriends(ctx context.Context, uid, peerUID uint64) error {
	return client.pair(ctx, uid, peerUID, func(ctx context.Context, service farmv1.SocialServiceClient) error {
		_, err := service.RemoveFriends(ctx, &farmv1.PairRequest{Uid: uid, PeerUid: peerUID})
		return err
	})
}

func (client *GRPCClient) ListFriends(ctx context.Context, uid uint64) ([]store.FriendRow, error) {
	service, err := client.service(ctx)
	if err != nil {
		return nil, err
	}
	response, err := service.ListFriends(ctx, &farmv1.UidRequest{Uid: uid})
	if err != nil {
		return nil, rpcerr.FromGRPC(err)
	}
	rows := make([]store.FriendRow, 0, len(response.Friends))
	for _, friend := range response.Friends {
		rows = append(rows, store.FriendRow{UID: friend.Uid, Nickname: friend.Nickname})
	}
	return rows, nil
}

func (client *GRPCClient) CountFriends(ctx context.Context, uid uint64) (int, error) {
	service, err := client.service(ctx)
	if err != nil {
		return 0, err
	}
	response, err := service.CountFriends(ctx, &farmv1.UidRequest{Uid: uid})
	if err != nil {
		return 0, rpcerr.FromGRPC(err)
	}
	return int(response.Count), nil
}

func (client *GRPCClient) FindUserByUsername(ctx context.Context, username string) (store.UserSearchRow, error) {
	service, err := client.service(ctx)
	if err != nil {
		return store.UserSearchRow{}, err
	}
	response, err := service.FindUser(ctx, &farmv1.FindUserRequest{Username: username})
	if err != nil {
		return store.UserSearchRow{}, rpcerr.FromGRPC(err)
	}
	return store.UserSearchRow{UID: response.Uid, Nickname: response.Nickname}, nil
}

func (client *GRPCClient) CreateFriendRequest(ctx context.Context, fromUID, toUID uint64) error {
	return client.pair(ctx, fromUID, toUID, func(ctx context.Context, service farmv1.SocialServiceClient) error {
		_, err := service.CreateFriendRequest(ctx, &farmv1.PairRequest{Uid: fromUID, PeerUid: toUID})
		return err
	})
}

func (client *GRPCClient) ListIncomingFriendRequests(ctx context.Context, uid uint64) ([]store.FriendRequestRow, error) {
	service, err := client.service(ctx)
	if err != nil {
		return nil, err
	}
	response, err := service.ListIncomingFriendRequests(ctx, &farmv1.UidRequest{Uid: uid})
	if err != nil {
		return nil, rpcerr.FromGRPC(err)
	}
	rows := make([]store.FriendRequestRow, 0, len(response.Requests))
	for _, request := range response.Requests {
		rows = append(rows, store.FriendRequestRow{
			FromUID: request.FromUid, Nickname: request.Nickname, CreatedAt: request.CreatedAt,
		})
	}
	return rows, nil
}

func (client *GRPCClient) AcceptFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	return client.pair(ctx, toUID, fromUID, func(ctx context.Context, service farmv1.SocialServiceClient) error {
		_, err := service.AcceptFriendRequest(ctx, &farmv1.PairRequest{Uid: toUID, PeerUid: fromUID})
		return err
	})
}

func (client *GRPCClient) RejectFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	return client.pair(ctx, toUID, fromUID, func(ctx context.Context, service farmv1.SocialServiceClient) error {
		_, err := service.RejectFriendRequest(ctx, &farmv1.PairRequest{Uid: toUID, PeerUid: fromUID})
		return err
	})
}

type pairCall func(context.Context, farmv1.SocialServiceClient) error

func (client *GRPCClient) SocialService(ctx context.Context) (farmv1.SocialServiceClient, error) {
	return client.service(ctx)
}

func (client *GRPCClient) pair(ctx context.Context, uid, peerUID uint64, call pairCall) error {
	if uid == 0 || peerUID == 0 {
		return store.ErrAccountNotFound
	}
	service, err := client.service(ctx)
	if err != nil {
		return err
	}
	if err := call(ctx, service); err != nil {
		return rpcerr.FromGRPC(err)
	}
	return nil
}

var _ store.FriendStore = (*GRPCClient)(nil)
