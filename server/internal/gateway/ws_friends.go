package gateway

import (
	"context"
	"errors"

	"farm/server/internal/pkgerr"
	"farm/server/internal/social"
	"farm/server/internal/store"
)

type acceptInviteRequest struct {
	Token string `json:"token"`
}

type friendPeerRequest struct {
	PeerUID uint64 `json:"peer_uid"`
}

type friendJSON struct {
	UID          uint64 `json:"uid"`
	Nickname     string `json:"nickname"`
	HasStealable bool   `json:"has_stealable"`
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
				UID:          friend.UID,
				Nickname:     friend.Nickname,
				HasStealable: hints[friend.UID],
			})
		}
		response.Payload = marshalPayload(friendListResponse{Friends: list})
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
		if payload.PeerUID == connection.uid {
			response.Err = pkgerr.CannotFriendSelf
			return response
		}
		if err := g.friends.RemoveFriends(context.Background(), connection.uid, payload.PeerUID); err != nil {
			response.Err = pkgerr.Internal
			return response
		}
		g.rooms.RevokeViewer(payload.PeerUID, connection.uid)
		g.rooms.RevokeViewer(connection.uid, payload.PeerUID)
		return response

	case CommandAddFriendByUID:
		var payload friendPeerRequest
		if err := unmarshalPayload(request.Payload, &payload); err != nil || payload.PeerUID == 0 {
			response.Err = pkgerr.BadRequest
			return response
		}
		response.Err = g.addFriends(connection.uid, payload.PeerUID)
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
