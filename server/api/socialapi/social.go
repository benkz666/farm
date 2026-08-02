// Package socialapi 提供 Social 服务的内部协议适配器。
package socialapi

import (
	"context"
	"encoding/json"

	"farm/server/api/rpc"
	"farm/server/api/rpcerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/store"
)

const (
	methodAreFriends    = "social.are_friends"
	methodAddFriends    = "social.add_friends"
	methodRemoveFriends = "social.remove_friends"
	methodListFriends   = "social.list_friends"
	methodCountFriends  = "social.count_friends"
	methodFindUser      = "social.find_user"
	methodCreateRequest = "social.create_request"
	methodListRequests  = "social.list_requests"
	methodAcceptRequest = "social.accept_request"
	methodRejectRequest = "social.reject_request"
)

type pairRequest struct {
	UID     pkgjson.UID `json:"uid"`
	PeerUID pkgjson.UID `json:"peer_uid"`
}

type uidRequest struct {
	UID pkgjson.UID `json:"uid"`
}

type usernameRequest struct {
	Username string `json:"username"`
}

type friendDTO struct {
	UID      pkgjson.UID `json:"uid"`
	Nickname string      `json:"nickname"`
}

type friendRequestDTO struct {
	FromUID   pkgjson.UID `json:"from_uid"`
	Nickname  string      `json:"nickname"`
	CreatedAt int64       `json:"created_at"`
}

// Dispatcher 把内部 RPC 请求转给好友关系仓储。
type Dispatcher struct {
	store store.FriendStore
}

func NewDispatcher(friendStore store.FriendStore) *Dispatcher {
	return &Dispatcher{store: friendStore}
}

func (dispatcher *Dispatcher) Dispatch(ctx context.Context, method string, payload json.RawMessage) (any, string) {
	if dispatcher == nil || dispatcher.store == nil {
		return nil, "internal"
	}
	switch method {
	case methodAreFriends:
		request, ok := decodePair(payload)
		if !ok {
			return nil, "bad_request"
		}
		value, err := dispatcher.store.AreFriends(ctx, uint64(request.UID), uint64(request.PeerUID))
		return responseOrError(struct {
			Value bool `json:"value"`
		}{value}, err)
	case methodAddFriends:
		request, ok := decodePair(payload)
		if !ok {
			return nil, "bad_request"
		}
		return responseOrError(struct{}{}, dispatcher.store.AddFriends(ctx, uint64(request.UID), uint64(request.PeerUID)))
	case methodRemoveFriends:
		request, ok := decodePair(payload)
		if !ok {
			return nil, "bad_request"
		}
		return responseOrError(struct{}{}, dispatcher.store.RemoveFriends(ctx, uint64(request.UID), uint64(request.PeerUID)))
	case methodListFriends:
		request, ok := decodeUID(payload)
		if !ok {
			return nil, "bad_request"
		}
		rows, err := dispatcher.store.ListFriends(ctx, uint64(request.UID))
		friends := make([]friendDTO, 0, len(rows))
		for _, row := range rows {
			friends = append(friends, friendDTO{UID: pkgjson.UID(row.UID), Nickname: row.Nickname})
		}
		return responseOrError(struct {
			Friends []friendDTO `json:"friends"`
		}{friends}, err)
	case methodCountFriends:
		request, ok := decodeUID(payload)
		if !ok {
			return nil, "bad_request"
		}
		count, err := dispatcher.store.CountFriends(ctx, uint64(request.UID))
		return responseOrError(struct {
			Count int `json:"count"`
		}{count}, err)
	case methodFindUser:
		var request usernameRequest
		if json.Unmarshal(payload, &request) != nil || request.Username == "" {
			return nil, "bad_request"
		}
		row, err := dispatcher.store.FindUserByUsername(ctx, request.Username)
		return responseOrError(friendDTO{UID: pkgjson.UID(row.UID), Nickname: row.Nickname}, err)
	case methodCreateRequest, methodAcceptRequest, methodRejectRequest:
		request, ok := decodePair(payload)
		if !ok {
			return nil, "bad_request"
		}
		uid, peerUID := uint64(request.UID), uint64(request.PeerUID)
		var err error
		switch method {
		case methodCreateRequest:
			err = dispatcher.store.CreateFriendRequest(ctx, uid, peerUID)
		case methodAcceptRequest:
			err = dispatcher.store.AcceptFriendRequest(ctx, uid, peerUID)
		case methodRejectRequest:
			err = dispatcher.store.RejectFriendRequest(ctx, uid, peerUID)
		}
		return responseOrError(struct{}{}, err)
	case methodListRequests:
		request, ok := decodeUID(payload)
		if !ok {
			return nil, "bad_request"
		}
		rows, err := dispatcher.store.ListIncomingFriendRequests(ctx, uint64(request.UID))
		requests := make([]friendRequestDTO, 0, len(rows))
		for _, row := range rows {
			requests = append(requests, friendRequestDTO{
				FromUID: pkgjson.UID(row.FromUID), Nickname: row.Nickname, CreatedAt: row.CreatedAt,
			})
		}
		return responseOrError(struct {
			Requests []friendRequestDTO `json:"requests"`
		}{requests}, err)
	default:
		return nil, "unknown_method"
	}
}

func decodePair(payload json.RawMessage) (pairRequest, bool) {
	var request pairRequest
	err := json.Unmarshal(payload, &request)
	return request, err == nil && request.UID != 0 && request.PeerUID != 0
}

func decodeUID(payload json.RawMessage) (uidRequest, bool) {
	var request uidRequest
	err := json.Unmarshal(payload, &request)
	return request, err == nil && request.UID != 0
}

func responseOrError(response any, err error) (any, string) {
	if err != nil {
		return nil, rpcerr.Kind(err)
	}
	return response, ""
}

// Client 实现 Gateway 所需的 FriendStore 边界。
type Client struct{ rpc *rpc.Client }

func NewClient(endpoint, internalToken string) *Client {
	return &Client{rpc: rpc.NewClient(endpoint, internalToken, nil)}
}

func (client *Client) call(ctx context.Context, method string, request, response any) error {
	return rpcerr.Decode(client.rpc.Call(ctx, method, request, response))
}

func (client *Client) AreFriends(ctx context.Context, uid, peerUID uint64) (bool, error) {
	var response struct {
		Value bool `json:"value"`
	}
	err := client.call(ctx, methodAreFriends, newPair(uid, peerUID), &response)
	return response.Value, err
}

func (client *Client) AddFriends(ctx context.Context, uid, peerUID uint64) error {
	return client.call(ctx, methodAddFriends, newPair(uid, peerUID), nil)
}

func (client *Client) RemoveFriends(ctx context.Context, uid, peerUID uint64) error {
	return client.call(ctx, methodRemoveFriends, newPair(uid, peerUID), nil)
}

func (client *Client) ListFriends(ctx context.Context, uid uint64) ([]store.FriendRow, error) {
	var response struct {
		Friends []friendDTO `json:"friends"`
	}
	if err := client.call(ctx, methodListFriends, uidRequest{UID: pkgjson.UID(uid)}, &response); err != nil {
		return nil, err
	}
	rows := make([]store.FriendRow, 0, len(response.Friends))
	for _, friend := range response.Friends {
		rows = append(rows, store.FriendRow{UID: uint64(friend.UID), Nickname: friend.Nickname})
	}
	return rows, nil
}

func (client *Client) CountFriends(ctx context.Context, uid uint64) (int, error) {
	var response struct {
		Count int `json:"count"`
	}
	err := client.call(ctx, methodCountFriends, uidRequest{UID: pkgjson.UID(uid)}, &response)
	return response.Count, err
}

func (client *Client) FindUserByUsername(ctx context.Context, username string) (store.UserSearchRow, error) {
	var response friendDTO
	if err := client.call(ctx, methodFindUser, usernameRequest{Username: username}, &response); err != nil {
		return store.UserSearchRow{}, err
	}
	return store.UserSearchRow{UID: uint64(response.UID), Nickname: response.Nickname}, nil
}

func (client *Client) CreateFriendRequest(ctx context.Context, fromUID, toUID uint64) error {
	return client.call(ctx, methodCreateRequest, newPair(fromUID, toUID), nil)
}

func (client *Client) ListIncomingFriendRequests(ctx context.Context, uid uint64) ([]store.FriendRequestRow, error) {
	var response struct {
		Requests []friendRequestDTO `json:"requests"`
	}
	if err := client.call(ctx, methodListRequests, uidRequest{UID: pkgjson.UID(uid)}, &response); err != nil {
		return nil, err
	}
	rows := make([]store.FriendRequestRow, 0, len(response.Requests))
	for _, request := range response.Requests {
		rows = append(rows, store.FriendRequestRow{
			FromUID: uint64(request.FromUID), Nickname: request.Nickname, CreatedAt: request.CreatedAt,
		})
	}
	return rows, nil
}

func (client *Client) AcceptFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	return client.call(ctx, methodAcceptRequest, newPair(toUID, fromUID), nil)
}

func (client *Client) RejectFriendRequest(ctx context.Context, toUID, fromUID uint64) error {
	return client.call(ctx, methodRejectRequest, newPair(toUID, fromUID), nil)
}

func newPair(uid, peerUID uint64) pairRequest {
	return pairRequest{UID: pkgjson.UID(uid), PeerUID: pkgjson.UID(peerUID)}
}

var _ store.FriendStore = (*Client)(nil)
