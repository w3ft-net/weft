package service

import (
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// These RPCs once let any logged-in user edit /bin and /cron.d over gRPC,
// bypassing the group checks the shell enforces. Nothing called them (the CLI,
// MCP and web all use REST, which writes through the shell's mounts), so they
// were removed rather than guarded. Keep them gone: a second implementation of
// a write the first one already does is how the checks drifted apart.
func TestWeftCoordinator_DoesNotExposeRetiredRPCs(t *testing.T) {
	retired := map[string]bool{
		"SaveFunction": true, "GetFunction": true, "ListFunctions": true, "DeleteFunction": true,
		"SaveWatch": true, "GetWatch": true, "ListWatches": true, "DeleteWatch": true, "GetWatchResults": true,
		"ListSources": true, "ListTokens": true, "RevokeToken": true,
	}
	for _, m := range pb.WeftCoordinator_ServiceDesc.Methods {
		if retired[m.MethodName] {
			t.Errorf("WeftCoordinator still exposes %s", m.MethodName)
		}
	}
}
