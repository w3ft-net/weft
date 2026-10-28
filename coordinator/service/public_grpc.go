package service

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/auth"
)

// PublicMethods are the WeftCoordinator RPCs served on the public gRPC port:
// the ones weftd, shards and producers call, which authenticate by agent
// token or not at all (so they are also the JWT-exempt set). Everything else
// on the service is served only on the loopback listener the api connects to.
var PublicMethods = auth.SkipMethods{
	"/weft.WeftCoordinator/Authenticate":    true,
	"/weft.WeftCoordinator/IngestStream":    true,
	"/weft.WeftCoordinator/SubmitSample":    true,
	"/weft.WeftCoordinator/GetSourceStatus": true,
	"/weft.WeftCoordinator/Heartbeat":       true,
}

// PublicOnlyUnaryInterceptor refuses every unary RPC not in PublicMethods.
// Deny by default: a method added to the service is not public until listed.
func PublicOnlyUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !PublicMethods[info.FullMethod] {
			return nil, status.Error(codes.Unimplemented, "method not served on this port")
		}
		return handler(ctx, req)
	}
}

// PublicOnlyStreamInterceptor is the streaming counterpart.
func PublicOnlyStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !PublicMethods[info.FullMethod] {
			return status.Error(codes.Unimplemented, "method not served on this port")
		}
		return handler(srv, ss)
	}
}
