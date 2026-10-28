package service

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// The public port serves only the ingest/infra RPCs; every other
// WeftCoordinator method (shard admin, the api's JWT-forwarded surface) is
// reachable only on the loopback listener. Deny by default, so an RPC added to
// the service later is not public until someone lists it.
func TestPublicOnly_DeniesEverythingButTheAllowList(t *testing.T) {
	unary := PublicOnlyUnaryInterceptor()
	reached := false
	handler := func(context.Context, any) (any, error) { reached = true; return nil, nil }

	for _, m := range pb.WeftCoordinator_ServiceDesc.Methods {
		full := "/weft.WeftCoordinator/" + m.MethodName
		reached = false
		_, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: full}, handler)
		if PublicMethods[full] {
			if err != nil || !reached {
				t.Errorf("%s is public: err=%v reached=%v", full, err, reached)
			}
			continue
		}
		if status.Code(err) != codes.Unimplemented || reached {
			t.Errorf("%s must be denied on the public port: err=%v reached=%v", full, err, reached)
		}
	}

	stream := PublicOnlyStreamInterceptor()
	for _, s := range pb.WeftCoordinator_ServiceDesc.Streams {
		full := "/weft.WeftCoordinator/" + s.StreamName
		reached = false
		err := stream(nil, nil, &grpc.StreamServerInfo{FullMethod: full}, func(any, grpc.ServerStream) error { reached = true; return nil })
		if PublicMethods[full] {
			if err != nil || !reached {
				t.Errorf("%s is public: err=%v reached=%v", full, err, reached)
			}
			continue
		}
		if status.Code(err) != codes.Unimplemented || reached {
			t.Errorf("%s must be denied on the public port: err=%v reached=%v", full, err, reached)
		}
	}
}

// A typo in the allow-list would silently drop a public RPC.
func TestPublicMethods_AllExist(t *testing.T) {
	have := map[string]bool{}
	for _, m := range pb.WeftCoordinator_ServiceDesc.Methods {
		have["/weft.WeftCoordinator/"+m.MethodName] = true
	}
	for _, s := range pb.WeftCoordinator_ServiceDesc.Streams {
		have["/weft.WeftCoordinator/"+s.StreamName] = true
	}
	for m := range PublicMethods {
		if !have[m] {
			t.Errorf("PublicMethods lists %s, which WeftCoordinator does not define", m)
		}
	}
}
