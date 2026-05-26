package main

import (
	"context"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// Off-EC2 (dev/docker) the shard has no IMDS, so the DockerLauncher
// injects a synthetic instance id via SHARD_INSTANCE_ID — the same id it
// stamps on the LAUNCHING shard SM. getInstanceID must report that env
// value so the first heartbeat correlates to the right shard. Env wins
// over IMDS so a stray metadata hit can't shadow the synthetic id.
func TestGetInstanceID_PrefersEnv(t *testing.T) {
	t.Setenv("SHARD_INSTANCE_ID", "weft-shard-abc123")
	if got := getInstanceID(); got != "weft-shard-abc123" {
		t.Errorf("getInstanceID: got %q want weft-shard-abc123", got)
	}
}

// recordingCoord captures the heartbeat request for assertion.
type recordingCoord struct {
	pb.UnimplementedWeftCoordinatorServer
	last *pb.ShardHeartbeatRequest
}

func (r *recordingCoord) Heartbeat(_ context.Context, req *pb.ShardHeartbeatRequest) (*pb.HeartbeatResponse, error) {
	r.last = req
	return &pb.HeartbeatResponse{}, nil
}

// The shard must include its EC2 instance id in every heartbeat.
// christen() looks up the pre-attached volume via
// tag:shard_id=pending-<instance_id>; without this field the lookup
// silently fails and the volume's shard_id tag never gets rewritten
// from pending-... to the real shard id. Observed 2026-04-18 in prod
// as a `volume=` empty string on the christening log line.
func TestHeartbeat_IncludesInstanceID(t *testing.T) {
	req := buildHeartbeatRequest("shard-x", "10.0.0.1:9000", "i-abc123", "INGESTING", 0)

	if req.InstanceId != "i-abc123" {
		t.Errorf("InstanceId: got %q want i-abc123", req.InstanceId)
	}
	if req.ShardId != "shard-x" {
		t.Errorf("ShardId: got %q want shard-x", req.ShardId)
	}
}

// Unclaimed shards (no shard.json yet) still heartbeat — they carry
// empty ShardId but MUST carry their instance id so the coordinator
// can find the pre-attached volume at christen time.
func TestHeartbeat_UnclaimedStillCarriesInstanceID(t *testing.T) {
	req := buildHeartbeatRequest("", "10.0.0.1:9000", "i-fresh", "ADOPTED", 0)

	if req.InstanceId != "i-fresh" {
		t.Errorf("InstanceId: got %q want i-fresh", req.InstanceId)
	}
	if req.ShardId != "" {
		t.Errorf("ShardId: got %q want empty", req.ShardId)
	}
}
