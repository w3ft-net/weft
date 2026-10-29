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

// InternalMethods are the RPCs served on the VPC-internal port: shards
// reporting to the coordinator. Not reachable from the internet.
var InternalMethods = auth.SkipMethods{
	"/weft.WeftCoordinator/Heartbeat": true,
}

// OnlyUnaryInterceptor refuses every unary RPC not in allowed. Deny by
// default: a method added to the service is not served on a port until listed.
func OnlyUnaryInterceptor(allowed auth.SkipMethods) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !allowed[info.FullMethod] {
			return nil, status.Error(codes.Unimplemented, "method not served on this port")
		}
		return handler(ctx, req)
	}
}

// OnlyStreamInterceptor is the streaming counterpart.
func OnlyStreamInterceptor(allowed auth.SkipMethods) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !allowed[info.FullMethod] {
			return status.Error(codes.Unimplemented, "method not served on this port")
		}
		return handler(srv, ss)
	}
}
