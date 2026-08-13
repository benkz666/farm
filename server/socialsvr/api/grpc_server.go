package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	publicv3 "farm/server/gen/farm/public/v3"
	farmv1 "farm/server/gen/farm/v1"
	"farm/server/shared/errcode"
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
	store        store.FriendStore
	stealHints   store.StealHintStore
	inviteSecret []byte
	now          func() int64
	notifier     SocialNotifier
	revoker      FarmAccessRevoker
	hub          *invalidationHub
}

// SocialNotifier emits advisory refresh hints after social mutations.
type SocialNotifier interface {
	PublishMailNotify(context.Context, uint64, string) error
}

// FarmAccessRevoker removes stale friend-room subscriptions after an unfriend.
type FarmAccessRevoker interface {
	RevokeFarmAccess(context.Context, uint64, uint64) error
}

type ServerOption func(*GRPCServer)

func WithStealHints(hints store.StealHintStore) ServerOption {
	return func(server *GRPCServer) { server.stealHints = hints }
}

func WithInviteSecret(secret []byte) ServerOption {
	copyOfSecret := append([]byte(nil), secret...)
	return func(server *GRPCServer) { server.inviteSecret = copyOfSecret }
}

func WithSocialNotifier(notifier SocialNotifier) ServerOption {
	return func(server *GRPCServer) { server.notifier = notifier }
}

func WithFarmAccessRevoker(revoker FarmAccessRevoker) ServerOption {
	return func(server *GRPCServer) { server.revoker = revoker }
}

func WithClock(now func() int64) ServerOption {
	return func(server *GRPCServer) {
		if now != nil {
			server.now = now
		}
	}
}

// NewGRPCServer constructs Social's typed application boundary.
func NewGRPCServer(friendStore store.FriendStore, options ...ServerOption) *GRPCServer {
	server := &GRPCServer{
		store: friendStore, hub: newInvalidationHub(),
		now: func() int64 { return time.Now().UnixMilli() },
	}
	for _, option := range options {
		if option != nil {
			option(server)
		}
	}
	return server
}

// RegisterGRPC registers SocialService on a gRPC server and returns the adapter
// so the process can attach its distributed invalidation subscriber.
func RegisterGRPC(server *grpc.Server, friendStore store.FriendStore, options ...ServerOption) *GRPCServer {
	adapter := NewGRPCServer(friendStore, options...)
	farmv1.RegisterSocialServiceServer(server, adapter)
	return adapter
}

// ExecuteClientCommand owns every public social command. Gateway contributes
// only authenticated transport metadata and never reads or mutates social data.
func (server *GRPCServer) ExecuteClientCommand(ctx context.Context, request *farmv1.ClientCommandRequest) (*farmv1.ClientCommandResponse, error) {
	if server == nil || server.store == nil || request == nil || request.Uid == 0 || request.Envelope == nil {
		return socialError(request, errcode.BadRequest), nil
	}
	envelope := request.Envelope
	payload := envelope.GetCommandRequest()
	if payload == nil {
		return socialError(request, errcode.BadRequest), nil
	}
	response := &publicv3.CommandResponse{}
	code := errcode.OK

	switch envelope.Cmd {
	case 400: // FriendList
		rows, err := server.store.ListFriends(ctx, request.Uid)
		if err != nil {
			code = errcode.Internal
			break
		}
		uids := make([]uint64, 0, len(rows))
		for _, row := range rows {
			uids = append(uids, row.UID)
		}
		hints := map[uint64]bool{}
		if server.stealHints != nil && len(uids) != 0 {
			hints, err = server.stealHints.GetStealHints(ctx, uids)
			if err != nil {
				code = errcode.Internal
				break
			}
		}
		response.Friends = make([]*publicv3.Friend, 0, len(rows))
		for _, row := range rows {
			response.Friends = append(response.Friends, &publicv3.Friend{
				Uid: row.UID, Nickname: row.Nickname, HasStealable: hints[row.UID],
			})
		}
	case 402: // GenShareLink
		if len(server.inviteSecret) == 0 {
			code = errcode.Internal
			break
		}
		token, err := IssueInvite(request.Uid, server.now(), server.inviteSecret)
		if err != nil {
			code = errcode.Internal
			break
		}
		response.Path = "/i/" + token
	case 404: // AcceptInvite
		if payload.InviteToken == "" || len(server.inviteSecret) == 0 {
			code = errcode.BadRequest
			break
		}
		peerUID, parsed := ParseInvite(payload.InviteToken, server.inviteSecret, server.now())
		if parsed != errcode.OK {
			code = parsed
			break
		}
		code = server.addFriends(ctx, request.Uid, peerUID)
		if code == errcode.OK {
			server.notify(peerUID, "friend_accept")
		}
	case 406: // RemoveFriend
		peerUID := payload.PeerUid
		if peerUID == 0 {
			code = errcode.BadRequest
			break
		}
		if peerUID == request.Uid {
			code = errcode.CannotFriendSelf
			break
		}
		if err := server.store.RemoveFriends(ctx, request.Uid, peerUID); err != nil {
			code = errcode.Internal
			break
		}
		server.hub.broadcast(request.Uid, peerUID)
		if server.revoker != nil {
			_ = server.revoker.RevokeFarmAccess(context.Background(), request.Uid, peerUID)
			_ = server.revoker.RevokeFarmAccess(context.Background(), peerUID, request.Uid)
		}
	case 408: // AddFriendByUID
		code = server.addFriends(ctx, request.Uid, payload.PeerUid)
		if code == errcode.OK {
			server.notify(payload.PeerUid, "friend_accept")
		}
	case 410: // SearchUser
		username := strings.TrimSpace(payload.Username)
		if username == "" {
			code = errcode.BadRequest
			break
		}
		row, err := server.store.FindUserByUsername(ctx, username)
		if err != nil {
			if errors.Is(err, store.ErrAccountNotFound) {
				code = errcode.UserNotFound
			} else {
				code = errcode.Internal
			}
			break
		}
		response.Users = []*publicv3.User{{Uid: row.UID, Nickname: row.Nickname}}
	case 412: // RequestFriend
		code = server.createFriendRequest(ctx, request.Uid, payload.PeerUid)
		if code == errcode.OK {
			server.notify(payload.PeerUid, "friend_request")
		}
	case 414: // ListFriendRequests
		rows, err := server.store.ListIncomingFriendRequests(ctx, request.Uid)
		if err != nil {
			code = errcode.Internal
			break
		}
		response.FriendRequests = make([]*publicv3.FriendRequest, 0, len(rows))
		for _, row := range rows {
			response.FriendRequests = append(response.FriendRequests, &publicv3.FriendRequest{
				FromUid: row.FromUID, Nickname: row.Nickname, CreatedAt: row.CreatedAt,
			})
		}
	case 416: // AcceptFriendRequest
		code = server.acceptFriendRequest(ctx, request.Uid, payload.FromUid)
		if code == errcode.OK {
			server.notify(payload.FromUid, "friend_accept")
		}
	case 418: // RejectFriendRequest
		if payload.FromUid == 0 {
			code = errcode.BadRequest
			break
		}
		if err := server.store.RejectFriendRequest(ctx, request.Uid, payload.FromUid); err != nil {
			if errors.Is(err, store.ErrFriendRequestNotFound) {
				code = errcode.FriendRequestNotFound
			} else {
				code = errcode.Internal
			}
			break
		}
		server.notify(payload.FromUid, "friend_reject")
	default:
		code = errcode.BadRequest
	}

	return &farmv1.ClientCommandResponse{Envelope: socialResponse(envelope, code, response)}, nil
}

func socialError(request *farmv1.ClientCommandRequest, code errcode.Code) *farmv1.ClientCommandResponse {
	var envelope *publicv3.WireEnvelope
	if request != nil {
		envelope = request.Envelope
	}
	return &farmv1.ClientCommandResponse{Envelope: socialResponse(envelope, code, &publicv3.CommandResponse{})}
}

func socialResponse(request *publicv3.WireEnvelope, code errcode.Code, response *publicv3.CommandResponse) *publicv3.WireEnvelope {
	var command, sequence uint32
	if request != nil {
		command, sequence = request.Cmd, request.ClientSeq
	}
	return &publicv3.WireEnvelope{
		Cmd: command, ClientSeq: sequence, Err: int32(code),
		Payload: &publicv3.WireEnvelope_CommandResponse{CommandResponse: response},
	}
}

func (server *GRPCServer) notify(uid uint64, kind string) {
	if server.notifier != nil && uid != 0 {
		_ = server.notifier.PublishMailNotify(context.Background(), uid, kind)
	}
}

func (server *GRPCServer) addFriends(ctx context.Context, uid, peerUID uint64) errcode.Code {
	if uid == 0 || peerUID == 0 {
		return errcode.BadRequest
	}
	if uid == peerUID {
		return errcode.CannotFriendSelf
	}
	if err := server.store.AddFriends(ctx, uid, peerUID); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyFriend):
			return errcode.AlreadyFriend
		case errors.Is(err, store.ErrFriendLimitSelf):
			return errcode.FriendLimitSelf
		case errors.Is(err, store.ErrFriendLimitPeer):
			return errcode.FriendLimitPeer
		case errors.Is(err, store.ErrPlayerNotFound), errors.Is(err, store.ErrAccountNotFound):
			return errcode.UserNotFound
		default:
			return errcode.Internal
		}
	}
	server.hub.broadcast(uid, peerUID)
	return errcode.OK
}

func (server *GRPCServer) createFriendRequest(ctx context.Context, uid, peerUID uint64) errcode.Code {
	if uid == 0 || peerUID == 0 {
		return errcode.BadRequest
	}
	if uid == peerUID {
		return errcode.CannotFriendSelf
	}
	if err := server.store.CreateFriendRequest(ctx, uid, peerUID); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyFriend):
			return errcode.AlreadyFriend
		case errors.Is(err, store.ErrFriendRequestPending):
			return errcode.FriendRequestPending
		case errors.Is(err, store.ErrCannotFriendSelf):
			return errcode.CannotFriendSelf
		case errors.Is(err, store.ErrFriendLimitSelf):
			return errcode.FriendLimitSelf
		case errors.Is(err, store.ErrFriendLimitPeer):
			return errcode.FriendLimitPeer
		case errors.Is(err, store.ErrPlayerNotFound):
			return errcode.UserNotFound
		default:
			return errcode.Internal
		}
	}
	server.hub.broadcast(uid, peerUID)
	return errcode.OK
}

func (server *GRPCServer) acceptFriendRequest(ctx context.Context, uid, fromUID uint64) errcode.Code {
	if uid == 0 || fromUID == 0 {
		return errcode.BadRequest
	}
	if err := server.store.AcceptFriendRequest(ctx, uid, fromUID); err != nil {
		switch {
		case errors.Is(err, store.ErrFriendRequestNotFound):
			return errcode.FriendRequestNotFound
		case errors.Is(err, store.ErrAlreadyFriend):
			return errcode.AlreadyFriend
		case errors.Is(err, store.ErrFriendLimitSelf):
			return errcode.FriendLimitSelf
		case errors.Is(err, store.ErrFriendLimitPeer):
			return errcode.FriendLimitPeer
		case errors.Is(err, store.ErrCannotFriendSelf):
			return errcode.CannotFriendSelf
		default:
			return errcode.Internal
		}
	}
	server.hub.broadcast(uid, fromUID)
	return errcode.OK
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
