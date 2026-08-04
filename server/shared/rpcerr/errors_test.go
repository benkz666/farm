package rpcerr

import (
	"errors"
	"testing"

	"farm/server/shared/errcode"
	"farm/server/shared/store"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStoreErrorRoundTrip(t *testing.T) {
	kind := Kind(store.ErrFriendRequestNotFound)
	decoded := FromGRPC(status.Error(codes.FailedPrecondition, kind))
	if !errors.Is(decoded, store.ErrFriendRequestNotFound) {
		t.Fatalf("FromGRPC(%q) = %v", kind, decoded)
	}
}

func TestProtocolErrorRoundTrip(t *testing.T) {
	kind := Kind(errcode.BadCredential)
	decoded := FromGRPC(status.Error(codes.FailedPrecondition, kind))
	var code errcode.Code
	if !errors.As(decoded, &code) || code != errcode.BadCredential {
		t.Fatalf("FromGRPC(%q) = %v", kind, decoded)
	}
}

func TestGRPCStatusInternal(t *testing.T) {
	err := GRPCStatus(errors.New("boom"))
	if status.Code(err) != codes.Internal {
		t.Fatalf("status = %v", err)
	}
}
