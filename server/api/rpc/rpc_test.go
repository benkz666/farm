package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

type echoDispatcher struct{}

func (echoDispatcher) Dispatch(_ context.Context, method string, payload json.RawMessage) (any, string) {
	if method != "echo" {
		return nil, "unknown_method"
	}
	var value map[string]string
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, "bad_request"
	}
	return value, ""
}

func TestClientAndHandlerRoundTrip(t *testing.T) {
	server := httptest.NewServer(NewHandler("internal-secret", echoDispatcher{}))
	defer server.Close()

	client := NewClient(server.URL, "internal-secret", server.Client())
	var response map[string]string
	if err := client.Call(context.Background(), "echo", map[string]string{"message": "你好"}, &response); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if response["message"] != "你好" {
		t.Fatalf("response = %#v", response)
	}
}

func TestClientPreservesRemoteErrorKind(t *testing.T) {
	server := httptest.NewServer(NewHandler("internal-secret", echoDispatcher{}))
	defer server.Close()

	err := NewClient(server.URL, "internal-secret", server.Client()).Call(
		context.Background(), "missing", struct{}{}, nil,
	)
	var remoteError *RemoteError
	if !errors.As(err, &remoteError) || remoteError.Kind != "unknown_method" {
		t.Fatalf("Call() error = %v", err)
	}
}
