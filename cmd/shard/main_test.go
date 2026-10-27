package main

import (
	"context"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	shardsvc "github.com/graybeardcorp/weft/shard/service"
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
	req := buildHeartbeatRequest("shard-x", "10.0.0.1:9000", "i-abc123", "INGESTING", 0, 4096)

	if req.InstanceId != "i-abc123" {
		t.Errorf("InstanceId: got %q want i-abc123", req.InstanceId)
	}
	if req.ShardId != "shard-x" {
		t.Errorf("ShardId: got %q want shard-x", req.ShardId)
	}
	if req.DiskBytes != 4096 {
		t.Errorf("DiskBytes: got %d want 4096", req.DiskBytes)
	}
}

// Unclaimed shards (no shard.json yet) still heartbeat — they carry
// empty ShardId but MUST carry their instance id so the coordinator
// can find the pre-attached volume at christen time.
func TestHeartbeat_UnclaimedStillCarriesInstanceID(t *testing.T) {
	req := buildHeartbeatRequest("", "10.0.0.1:9000", "i-fresh", "ADOPTED", 0, 0)

	if req.InstanceId != "i-fresh" {
		t.Errorf("InstanceId: got %q want i-fresh", req.InstanceId)
	}
	if req.ShardId != "" {
		t.Errorf("ShardId: got %q want empty", req.ShardId)
	}
}

// noVolume is a Volume that is always attached: activation mounts nothing.
type noVolume struct{}

func (noVolume) Mount(context.Context) error   { return nil }
func (noVolume) Unmount(context.Context) error { return nil }

// A warm-pool spare heartbeats as one, with no shard id, so the
// coordinator refreshes the pool's record of it and never correlates it to
// a LAUNCHING shard.
func TestHeartbeat_SpareAnnouncesItself(t *testing.T) {
	srv := shardsvc.NewSpareServer(t.TempDir(), "", noVolume{})
	req, send := heartbeatFor(srv, "10.0.0.2:9000", "i-spare")
	if !send {
		t.Fatal("a spare must heartbeat")
	}
	if !req.Spare || req.ShardId != "" || req.InstanceId != "i-spare" {
		t.Fatalf("spare heartbeat: spare=%v shard=%q instance=%q", req.Spare, req.ShardId, req.InstanceId)
	}
}

// A paused shard has handed its volume to a successor; a beat from the old
// address would pull callers back to it, so it stays silent.
func TestHeartbeat_PausedShardIsSilent(t *testing.T) {
	srv := shardsvc.NewBlankServer(t.TempDir(), "")
	if _, err := srv.Pause(context.Background(), &pb.PauseRequest{SuccessorAddr: "10.0.0.3:9000"}); err != nil {
		t.Fatal(err)
	}
	if _, send := heartbeatFor(srv, "10.0.0.2:9000", "i-old"); send {
		t.Fatal("a paused shard must not heartbeat")
	}
}

// A warm-pool spare that the leader claims while it is still booting is
// retagged before the shard binary reads its tag, though startup.sh saw the
// spare tag and left the data directory unmounted. The binary then took the
// blank-server path and the shard ran on the root disk, with the attached data
// volume never mounted (seen in prod on 2026-10-27, S3). Where the host
// manages a volume, a data directory that is not a mount means the shard has
// no volume yet, whatever the tag says now.
func TestStartsAsSpare_AnUnmountedDataDirWithAManagedVolumeWaitsForTheVolume(t *testing.T) {
	notAMount := t.TempDir()
	if !startsAsSpare(false, notAMount, true) {
		t.Fatal("tag already retagged, volume helper present, data dir not mounted: want spare mode (wait for the attach), got a blank server on the root disk")
	}
}

func TestStartsAsSpare_AMountedDataDirIsAShard(t *testing.T) {
	// /dev is its own mount on Linux (devtmpfs) and macOS (devfs).
	if startsAsSpare(false, "/dev", true) {
		t.Fatal("data dir is a mount point: want a shard, got spare mode")
	}
}

func TestStartsAsSpare_TheSpareTagStillWins(t *testing.T) {
	if !startsAsSpare(true, "/dev", true) {
		t.Fatal("spare tag set: want spare mode")
	}
}

// Dev and container mode have no volume helper and no mount to wait for.
func TestStartsAsSpare_WithoutAVolumeHelperTheDataDirIsJustADirectory(t *testing.T) {
	if startsAsSpare(false, t.TempDir(), false) {
		t.Fatal("no volume helper: want a normal shard on the plain directory")
	}
}
