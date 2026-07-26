package pkgerr_test

import (
	"testing"

	"farm/server/internal/pkgerr"
)

func TestProtocolCodes(t *testing.T) {
	if pkgerr.UsernameTaken != 1103 {
		t.Fatalf("ERR_USERNAME_TAKEN want 1103 got %d", pkgerr.UsernameTaken)
	}
	if pkgerr.BadCredential != 1104 {
		t.Fatalf("want 1104")
	}
	if pkgerr.Unauthorized != 1101 {
		t.Fatalf("want 1101")
	}
	if pkgerr.NotFriend != 1401 {
		t.Fatalf("want 1401")
	}
	if pkgerr.RateLimited != 1003 {
		t.Fatalf("want 1003")
	}
	if pkgerr.ConfigStale != 1007 {
		t.Fatalf("want 1007")
	}
}

// TestFriendCodes 校验 protocol 4.5 期 3 新增的好友错误码 1402–1407。
func TestFriendCodes(t *testing.T) {
	cases := map[pkgerr.Code]int{
		pkgerr.AlreadyFriend:    1402,
		pkgerr.CannotFriendSelf: 1403,
		pkgerr.FriendLimitSelf:  1404,
		pkgerr.FriendLimitPeer:  1405,
		pkgerr.InviteInvalid:    1406,
		pkgerr.InviteExpired:    1407,
	}
	for code, want := range cases {
		if int(code) != want {
			t.Errorf("want %d got %d", want, code)
		}
	}
}
