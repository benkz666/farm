package gateway

import (
	"context"
	"errors"

	social "farm/server/api/socialapi"
	"farm/server/platform/pkgerr"
	"farm/server/platform/pkgjson"
	"farm/server/platform/store"
)

type acceptInviteRequest struct {
	Token string `json:"token"`
}

type friendPeerRequest struct {
	PeerUID pkgjson.UID `json:"peer_uid"`
}

type searchUserRequest struct {
	Username string `json:"username"`
}

type friendJSON struct {
	UID          pkgjson.UID `json:"uid"`
	Nickname     string      `json:"nickname"`
	HasStealable bool        `json:"has_stealable"`
}

// writeStealHint 刷新弱一致可偷摘要；失败可忽略（下次成熟/动作会再写）。
func (g *Gateway) writeStealHint(uid uint64, hasStealable bool) {
	if g == nil || g.stealHints == nil || uid == 0 {
		return
	}
	_ = g.stealHints.SetStealHint(context.Background(), uid, hasStealable)
}

type friendListResponse struct {
	Friends []friendJSON `json:"friends"`
}

type searchUserResponse struct {
	Users []searchUserResponseItem `json:"users"`
}

type searchUserResponseItem struct {
	UID      pkgjson.UID `json:"uid"`
	Nickname string      `json:"nickname"`
}

type friendRequestJSON struct {
	FromUID   pkgjson.UID `json:"from_uid"`
	Nickname  string      `json:"nickname"`
	CreatedAt int64       `json:"created_at"`
}

type listFriendRequestsResponse struct {
	Requests []friendRequestJSON `json:"requests"`
}

type genShareLinkResponse struct {
	Path string `json:"path"`
}

func (g *Gateway) handleFriendRequest(connection *wsConnection, request Envelope) Envelope {
	response := Envelope{
		Cmd:       request.Cmd,
		ClientSeq: request.ClientSeq,
		Payload:   emptyPayload,
	}
	if g.friends == nil {
		response.Err = pkgerr.Internal
		return response
	}

	switch request.Cmd {
	case CommandFriendList:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		friends, err := g.friends.ListFriends(context.Background(), connection.uid)
		if err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		uids := make([]uint64, 0, len(friends))
		for _, friend := range friends {
			uids = append(uids, friend.UID)
		}
		hints := map[uint64]bool{}
		if g.stealHints != nil && len(uids) > 0 {
			hints, err = g.stealHints.GetStealHints(context.Background(), uids)
			if err != nil {
				response.Err = pkgerr.Internal
				return response
			}
		}
		list := make([]friendJSON, 0, len(friends))
		for _, friend := range friends {
			list = append(list, friendJSON{
				UID:          pkgjson.UID(friend.UID),
				Nickname:     friend.Nickname,
				HasStealable: hints[friend.UID],
			})
		}
		response.Payload = marshalPayload(friendListResponse{Friends: list})
		return response

	case CommandSearchUser:
		var payload searchUserRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.Username == "" {
			response.Err = pkgerr.BadRequest
			return response
		}
		user, err := g.friends.FindUserByUsername(context.Background(), payload.Username)
		if err != nil {
			if errors.Is(err, store.ErrAccountNotFound) {
				response.Err = pkgerr.UserNotFound
				return response
			}
			response.Err = pkgerr.Internal
			return response
		}
		response.Payload = marshalPayload(searchUserResponse{
			Users: []searchUserResponseItem{{
				UID:      pkgjson.UID(user.UID),
				Nickname: user.Nickname,
			}},
		})
		return response

	case CommandRequestFriend:
		var payload friendPeerRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PeerUID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		peerUID := uint64(payload.PeerUID)
		response.Err = g.createFriendRequest(connection.uid, peerUID)
		if response.Err == pkgerr.OK {
			// 对方在线则推 MailNotify，客户端拉申请列表点亮红点（无轮询）
			g.pushMailNotify(peerUID, "friend_request")
		}
		return response

	case CommandListFriendRequests:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		rows, err := g.friends.ListIncomingFriendRequests(context.Background(), connection.uid)
		if err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		list := make([]friendRequestJSON, 0, len(rows))
		for _, row := range rows {
			list = append(list, friendRequestJSON{
				FromUID:   pkgjson.UID(row.FromUID),
				Nickname:  row.Nickname,
				CreatedAt: row.CreatedAt,
			})
		}
		response.Payload = marshalPayload(listFriendRequestsResponse{Requests: list})
		return response

	case CommandAcceptFriendRequest:
		var payload struct {
			FromUID pkgjson.UID `json:"from_uid"`
		}
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.FromUID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		fromUID := uint64(payload.FromUID)
		response.Err = g.acceptFriendRequest(connection.uid, fromUID)
		if response.Err == pkgerr.OK {
			g.pushMailNotify(fromUID, "friend_accept")
		}
		return response

	case CommandRejectFriendRequest:
		var payload struct {
			FromUID pkgjson.UID `json:"from_uid"`
		}
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.FromUID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		fromUID := uint64(payload.FromUID)
		if err := g.friends.RejectFriendRequest(context.Background(), connection.uid, fromUID); err != nil {
			if errors.Is(err, store.ErrFriendRequestNotFound) {
				response.Err = pkgerr.FriendRequestNotFound
			} else {
				response.Err = pkgerr.Internal
			}
			return response
		}
		g.pushMailNotify(fromUID, "friend_reject")
		return response

	case CommandGenShareLink:
		if err := unmarshalPayload(request.Payload, &struct{}{}); err != nil {
			response.Err = pkgerr.BadRequest
			return response
		}
		if len(g.inviteSecret) == 0 {
			response.Err = pkgerr.Internal
			return response
		}
		token, err := social.IssueInvite(connection.uid, g.Now(), g.inviteSecret)
		if err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		response.Payload = marshalPayload(genShareLinkResponse{Path: "/i/" + token})
		return response

	case CommandAcceptInvite:
		var payload acceptInviteRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.Token == "" {
			response.Err = pkgerr.BadRequest
			return response
		}
		if len(g.inviteSecret) == 0 {
			response.Err = pkgerr.Internal
			return response
		}
		inviterUID, code := social.ParseInvite(payload.Token, g.inviteSecret, g.Now())
		if code != pkgerr.OK {
			response.Err = code
			return response
		}
		if inviterUID == 0 {
			response.Err = pkgerr.InviteInvalid
			return response
		}
		response.Err = g.addFriends(connection.uid, inviterUID)
		return response

	case CommandRemoveFriend:
		var payload friendPeerRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PeerUID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		peerUID := uint64(payload.PeerUID)
		if peerUID == connection.uid {
			response.Err = pkgerr.CannotFriendSelf
			return response
		}
		if err := g.friends.RemoveFriends(context.Background(), connection.uid, peerUID); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		g.rooms.RevokeViewer(peerUID, connection.uid)
		g.rooms.RevokeViewer(connection.uid, peerUID)
		return response

	case CommandAddFriendByUID:
		var payload friendPeerRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PeerUID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		response.Err = g.addFriends(connection.uid, uint64(payload.PeerUID))
		return response

	default:
		response.Err = pkgerr.BadRequest
		return response
	}
}

func (g *Gateway) addFriends(uid, peerUID uint64) pkgerr.Code {
	if peerUID == uid {
		return pkgerr.CannotFriendSelf
	}
	if err := g.friends.AddFriends(context.Background(), uid, peerUID); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyFriend):
			return pkgerr.AlreadyFriend
		case errors.Is(err, store.ErrFriendLimitSelf):
			return pkgerr.FriendLimitSelf
		case errors.Is(err, store.ErrFriendLimitPeer):
			return pkgerr.FriendLimitPeer
		case errors.Is(err, store.ErrPlayerNotFound):
			return pkgerr.BadRequest
		default:
			return pkgerr.Internal
		}
	}
	return pkgerr.OK
}

func (g *Gateway) createFriendRequest(uid, peerUID uint64) pkgerr.Code {
	if peerUID == uid {
		return pkgerr.CannotFriendSelf
	}
	if err := g.friends.CreateFriendRequest(context.Background(), uid, peerUID); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyFriend):
			return pkgerr.AlreadyFriend
		case errors.Is(err, store.ErrFriendRequestPending):
			return pkgerr.FriendRequestPending
		case errors.Is(err, store.ErrCannotFriendSelf):
			return pkgerr.CannotFriendSelf
		case errors.Is(err, store.ErrFriendLimitSelf):
			return pkgerr.FriendLimitSelf
		case errors.Is(err, store.ErrFriendLimitPeer):
			return pkgerr.FriendLimitPeer
		case errors.Is(err, store.ErrPlayerNotFound):
			return pkgerr.UserNotFound
		default:
			return pkgerr.Internal
		}
	}
	return pkgerr.OK
}

func (g *Gateway) acceptFriendRequest(uid, fromUID uint64) pkgerr.Code {
	if err := g.friends.AcceptFriendRequest(context.Background(), uid, fromUID); err != nil {
		switch {
		case errors.Is(err, store.ErrFriendRequestNotFound):
			return pkgerr.FriendRequestNotFound
		case errors.Is(err, store.ErrAlreadyFriend):
			return pkgerr.AlreadyFriend
		case errors.Is(err, store.ErrFriendLimitSelf):
			return pkgerr.FriendLimitSelf
		case errors.Is(err, store.ErrFriendLimitPeer):
			return pkgerr.FriendLimitPeer
		case errors.Is(err, store.ErrCannotFriendSelf):
			return pkgerr.CannotFriendSelf
		default:
			return pkgerr.Internal
		}
	}
	return pkgerr.OK
}
