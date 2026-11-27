package server

import (
	"testing"
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
