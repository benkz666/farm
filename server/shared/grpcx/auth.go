package grpcx

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const authorizationMetadataKey = "authorization"

// BearerTokenUnaryInterceptor validates Authorization: Bearer <token> on unary RPCs.
func BearerTokenUnaryInterceptor(token []byte) grpc.UnaryServerInterceptor {
	want := bearerValue(token)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !authorized(ctx, want) {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}
		return handler(ctx, req)
	}
}

// BearerTokenStreamInterceptor validates Authorization: Bearer <token> on stream RPCs.
func BearerTokenStreamInterceptor(token []byte) grpc.StreamServerInterceptor {
	want := bearerValue(token)
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !authorized(stream.Context(), want) {
			return status.Error(codes.Unauthenticated, "unauthorized")
		}
		return handler(srv, stream)
	}
}

func bearerValue(token []byte) []byte {
	return []byte("Bearer " + strings.TrimSpace(string(token)))
}

func authorized(ctx context.Context, want []byte) bool {
	if len(want) <= len("Bearer ") {
		return false
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	values := md.Get(authorizationMetadataKey)
	if len(values) == 0 {
		return false
	}
	got := []byte(strings.TrimSpace(values[0]))
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

// WithBearerToken attaches Authorization metadata to outgoing RPCs.
func WithBearerToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, authorizationMetadataKey, "Bearer "+strings.TrimSpace(token))
}
