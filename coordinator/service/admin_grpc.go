package service

import (
	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// NewAdminGRPCServer builds a gRPC server that hosts WeftAdmin and ONLY
// WeftAdmin, gated by auth.SharedTokenUnaryServerInterceptor. The
// coordinator boots it on a 127.0.0.1:<admin-port> listener so it's
// unreachable from anywhere outside the coordinator instance; the shared
// token (WEFT_ADMIN_TOKEN env) is the second wall.
//
// The token is required: SharedTokenUnaryServerInterceptor panics on an
// empty string so a misconfigured deploy can't silently accept
// unauthenticated admin calls.
//
// Tests use this same constructor against an ephemeral 127.0.0.1 port —
// see admin_grpc_test.go.
func NewAdminGRPCServer(server *Server, token string) *grpc.Server {
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(auth.SharedTokenUnaryServerInterceptor(token)),
		grpc.StreamInterceptor(auth.SharedTokenStreamServerInterceptor(token)),
	)
	pb.RegisterWeftAdminServer(srv, server)
	return srv
}
