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
