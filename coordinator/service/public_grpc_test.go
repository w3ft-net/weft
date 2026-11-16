package service

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/identity"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// The public port serves only the ingest/infra RPCs; every other
// WeftCoordinator method (shard admin, the api's JWT-forwarded surface) is
// reachable only on the loopback listener. Deny by default, so an RPC added to
// the service later is not public until someone lists it.
func TestOnly_DeniesEverythingButTheAllowList(t *testing.T) {
	for name, allowed := range map[string]identity.SkipMethods{"public": PublicMethods, "internal": InternalMethods} {
		t.Run(name, func(t *testing.T) { checkOnlyAllowed(t, allowed) })
	}
}

// Heartbeat moves shards off the internet-facing port: it names an instance and
// supplies an address the coordinator then dials, so it is served only inside
// the VPC.
func TestPublicPort_DoesNotServeHeartbeat(t *testing.T) {
	if PublicMethods["/weft.WeftCoordinator/Heartbeat"] {
		t.Error("Heartbeat must not be served on the public port")
	}
}

func checkOnlyAllowed(t *testing.T, allowed identity.SkipMethods) {
	t.Helper()
	unary := OnlyUnaryInterceptor(allowed)
	reached := false
	handler := func(context.Context, any) (any, error) { reached = true; return nil, nil }

	for _, m := range pb.WeftCoordinator_ServiceDesc.Methods {
		full := "/weft.WeftCoordinator/" + m.MethodName
		reached = false
		_, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: full}, handler)
		if allowed[full] {
			if err != nil || !reached {
				t.Errorf("%s is allowed: err=%v reached=%v", full, err, reached)
			}
			continue
		}
		if status.Code(err) != codes.Unimplemented || reached {
			t.Errorf("%s must be denied on this port: err=%v reached=%v", full, err, reached)
		}
	}

	stream := OnlyStreamInterceptor(allowed)
	for _, s := range pb.WeftCoordinator_ServiceDesc.Streams {
		full := "/weft.WeftCoordinator/" + s.StreamName
		reached = false
		err := stream(nil, nil, &grpc.StreamServerInfo{FullMethod: full}, func(any, grpc.ServerStream) error { reached = true; return nil })
		if allowed[full] {
			if err != nil || !reached {
				t.Errorf("%s is allowed: err=%v reached=%v", full, err, reached)
			}
			continue
		}
		if status.Code(err) != codes.Unimplemented || reached {
			t.Errorf("%s must be denied on this port: err=%v reached=%v", full, err, reached)
		}
	}
}

// A typo in an allow-list would silently drop an RPC.
func TestAllowLists_AllExist(t *testing.T) {
	have := map[string]bool{}
	for _, m := range pb.WeftCoordinator_ServiceDesc.Methods {
		have["/weft.WeftCoordinator/"+m.MethodName] = true
	}
	for _, s := range pb.WeftCoordinator_ServiceDesc.Streams {
		have["/weft.WeftCoordinator/"+s.StreamName] = true
	}
	for _, allowed := range []identity.SkipMethods{PublicMethods, InternalMethods} {
		for m := range allowed {
			if !have[m] {
				t.Errorf("an allow-list names %s, which WeftCoordinator does not define", m)
			}
		}
	}
}
