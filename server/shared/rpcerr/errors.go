// Package rpcerr maps business errors across gRPC process boundaries.
package rpcerr

import (
	"errors"
	"strconv"
	"strings"

	"farm/server/shared/errcode"
	"farm/server/shared/store"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const internalKind = "internal"

var storeErrors = []struct {
	kind string
	err  error
}{
	{"username_taken", store.ErrUsernameTaken},
	{"account_not_found", store.ErrAccountNotFound},
	{"session_not_found", store.ErrSessionNotFound},
	{"session_replaced", store.ErrSessionReplaced},
	{"farm_not_found", store.ErrFarmNotFound},
	{"player_not_found", store.ErrPlayerNotFound},
	{"already_friend", store.ErrAlreadyFriend},
	{"friend_limit_self", store.ErrFriendLimitSelf},
	{"friend_limit_peer", store.ErrFriendLimitPeer},
	{"cannot_friend_self", store.ErrCannotFriendSelf},
	{"friend_request_pending", store.ErrFriendRequestPending},
	{"friend_request_not_found", store.ErrFriendRequestNotFound},
	{"task_not_complete", store.ErrTaskNotComplete},
	{"task_already_claimed", store.ErrTaskAlreadyClaimed},
	{"mail_not_found", store.ErrMailNotFound},
	{"mail_no_attachment", store.ErrMailNoAttachment},
	{"mail_already_claimed", store.ErrMailAlreadyClaimed},
	{"daily_login_already_claimed", store.ErrDailyLoginAlreadyClaimed},
}

// Kind encodes a registered business error as a stable cross-process name.
func Kind(err error) string {
	if err == nil {
		return ""
	}
	var code errcode.Code
	if errors.As(err, &code) {
		return "protocol:" + strconv.Itoa(int(code))
	}
	for _, candidate := range storeErrors {
		if errors.Is(err, candidate.err) {
			return candidate.kind
		}
	}
	return internalKind
}

// GRPCStatus maps a business error to a gRPC status for typed Social RPCs.
func GRPCStatus(err error) error {
	if err == nil {
		return nil
	}
	kind := Kind(err)
	if kind == "" {
		return nil
	}
	if kind == internalKind {
		return status.Error(codes.Internal, internalKind)
	}
	return status.Error(codes.FailedPrecondition, kind)
}

// FromGRPC restores store/protocol errors from a gRPC client error.
func FromGRPC(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if st.Code() == codes.OK {
		return nil
	}
	return DecodeKind(st.Message())
}

// DecodeKind maps a stable error kind back to a caller-side error value.
func DecodeKind(kind string) error {
	if strings.HasPrefix(kind, "protocol:") {
		value, parseErr := strconv.Atoi(strings.TrimPrefix(kind, "protocol:"))
		if parseErr == nil {
			return errcode.Code(value)
		}
		return errcode.Internal
	}
	for _, candidate := range storeErrors {
		if kind == candidate.kind {
			return candidate.err
		}
	}
	return errcode.Internal
}
