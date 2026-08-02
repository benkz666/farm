// Package rpcerr 在进程边界两侧稳定地映射业务错误。
//
// 服务间协议不传 Go 错误文本，避免调用方依赖实现细节；只传这里登记的稳定名称。
package rpcerr

import (
	"errors"
	"strconv"
	"strings"

	"farm/server/api/rpc"
	"farm/server/platform/pkgerr"
	"farm/server/platform/store"
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

// Kind 把已登记的业务错误编码为跨进程稳定名称。
func Kind(err error) string {
	if err == nil {
		return ""
	}
	var code pkgerr.Code
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

// Decode 把 RPC 客户端收到的稳定名称恢复为调用方可 errors.Is/As 的错误。
func Decode(err error) error {
	var remoteError *rpc.RemoteError
	if !errors.As(err, &remoteError) {
		return err
	}
	if strings.HasPrefix(remoteError.Kind, "protocol:") {
		value, parseErr := strconv.Atoi(strings.TrimPrefix(remoteError.Kind, "protocol:"))
		if parseErr == nil {
			return pkgerr.Code(value)
		}
		return pkgerr.Internal
	}
	for _, candidate := range storeErrors {
		if remoteError.Kind == candidate.kind {
			return candidate.err
		}
	}
	return pkgerr.Internal
}
