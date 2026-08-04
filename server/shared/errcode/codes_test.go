package errcode_test

import (
	"testing"

	"farm/server/shared/errcode"
)

func TestProtocolCodes(t *testing.T) {
	if errcode.UsernameTaken != 1103 {
		t.Fatalf("ERR_USERNAME_TAKEN want 1103 got %d", errcode.UsernameTaken)
	}
	if errcode.BadCredential != 1104 {
		t.Fatalf("want 1104")
	}
	if errcode.Unauthorized != 1101 {
		t.Fatalf("want 1101")
	}
	if errcode.NotFriend != 1401 {
		t.Fatalf("want 1401")
	}
	if errcode.RateLimited != 1003 {
		t.Fatalf("want 1003")
	}
	if errcode.ConfigStale != 1007 {
		t.Fatalf("want 1007")
	}
}

// TestFriendCodes 校验 protocol 4.5 期 3 新增的好友错误码 1402–1407。
func TestFriendCodes(t *testing.T) {
	cases := map[errcode.Code]int{
		errcode.AlreadyFriend:    1402,
		errcode.CannotFriendSelf: 1403,
		errcode.FriendLimitSelf:  1404,
		errcode.FriendLimitPeer:  1405,
		errcode.InviteInvalid:    1406,
		errcode.InviteExpired:    1407,
	}
	for code, want := range cases {
		if int(code) != want {
			t.Errorf("want %d got %d", want, code)
		}
	}
}
