package rpcerr

import (
	"errors"
	"testing"

	"farm/server/api/rpc"
	"farm/server/platform/pkgerr"
	"farm/server/platform/store"
)

func TestStoreErrorRoundTrip(t *testing.T) {
	kind := Kind(store.ErrFriendRequestNotFound)
	decoded := Decode(&rpc.RemoteError{Kind: kind})
	if !errors.Is(decoded, store.ErrFriendRequestNotFound) {
		t.Fatalf("Decode(%q) = %v", kind, decoded)
	}
}

func TestProtocolErrorRoundTrip(t *testing.T) {
	kind := Kind(pkgerr.BadCredential)
	decoded := Decode(&rpc.RemoteError{Kind: kind})
	var code pkgerr.Code
	if !errors.As(decoded, &code) || code != pkgerr.BadCredential {
		t.Fatalf("Decode(%q) = %v", kind, decoded)
	}
}
