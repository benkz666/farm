package authapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"farm/server/api/rpc"
)

type authStub struct{}

func (authStub) Register(_ context.Context, username, _ string) (uint64, string, error) {
	return 9007199254740993, "register-" + username, nil
}
func (authStub) Login(_ context.Context, username, _ string) (uint64, string, error) {
	return 9007199254740992, "login-" + username, nil
}

func TestUIDDoesNotLoseJSONPrecision(t *testing.T) {
	server := httptest.NewServer(rpc.NewHandler("secret", NewDispatcher(authStub{})))
	defer server.Close()

	uid, token, err := NewClient(server.URL, "secret").Register(context.Background(), "alice", "password")
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if uid != 9007199254740993 || token != "register-alice" {
		t.Fatalf("Register() = (%d, %q)", uid, token)
	}
}
