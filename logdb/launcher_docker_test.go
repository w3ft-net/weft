package logdb

import (
	"testing"
	"time"
)

// fixtureDockerLauncher returns a launcher with the fields runArgs reads
// populated, so the argv assertions don't trip over empty values.
func fixtureDockerLauncher() *DockerLauncher {
	return &DockerLauncher{
		Image:             "weft:dev",
		Network:           "weft-net",
		HostDataRoot:      "/host/data",
		LocalDataRoot:     "/local/data",
		CoordinatorAddr:   "coordinator:9020",
		HeartbeatInterval: 10 * time.Second,
		ShardSecret:       "s3cr3t",
		containers:        map[string]string{},
	}
}

// argFollowing returns the token after the first occurrence of flag, or
// "" if flag isn't present (or is last). Lets the assertions check
// flag/value pairs without hard-coding argv positions.
func argFollowing(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasEnv(args []string, kv string) bool {
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && args[i+1] == kv {
			return true
		}
	}
	return false
}

// The forced-identity launch must wire the synthetic instance id so the
// shard reports it back and the coordinator can correlate the first
// heartbeat to the LAUNCHING SM: --hostname == container name, and
// SHARD_INSTANCE_ID == container name (which is also the addr host).
func TestDockerRunArgs_ForcedIdentity(t *testing.T) {
	d := fixtureDockerLauncher()
	const cname = "weft-shard-abc123"
	args := d.runArgs(cname, "/host/data/abc123", true)

	if got := argFollowing(args, "--hostname"); got != cname {
		t.Errorf("--hostname: got %q want %q", got, cname)
	}
	if !hasEnv(args, "SHARD_INSTANCE_ID="+cname) {
		t.Errorf("missing -e SHARD_INSTANCE_ID=%s in args: %v", cname, args)
	}
	if got := argFollowing(args, "--name"); got != cname {
		t.Errorf("--name: got %q want %q", got, cname)
	}
}

// Both launch paths carry the shared shard env regardless of identity
// mode, so a forced-identity container still boots the shard role.
func TestDockerRunArgs_CommonEnv(t *testing.T) {
	d := fixtureDockerLauncher()
	for _, forced := range []bool{false, true} {
		args := d.runArgs("weft-shard-x", "/host/data/x", forced)
		for _, want := range []string{
			"ROLE=shard",
			"COORDINATOR_ADDR=coordinator:9020",
			"SHARD_SECRET=s3cr3t",
		} {
			if !hasEnv(args, want) {
				t.Errorf("forced=%v: missing -e %s in args: %v", forced, want, args)
			}
		}
	}
}

// The dev striper collapses to one synthetic AZ — every stripe index
// lands in "local" — but it answers AZsForTier/StripeAZ so the bucket SM
// can consult it identically to the prod ASGProvisioner.
func TestDockerStriper_SingleAZ(t *testing.T) {
	d := fixtureDockerLauncher()
	if azs := d.AZsForTier("small"); len(azs) != 1 || azs[0] != devAZ {
		t.Errorf("AZsForTier: got %v want [%s]", azs, devAZ)
	}
	for _, idx := range []int{0, 1, 2, 7} {
		if az := d.StripeAZ("small", idx); az != devAZ {
			t.Errorf("StripeAZ(small, %d): got %q want %q", idx, az, devAZ)
		}
	}
}

// DockerLauncher must satisfy ShardLaunchAPI so shardSMDeps wires it as
// the dev launch primitive once the Slice C switch flips the tick to the
// FSMs. A compile-time assertion guards the contract.
func TestDockerLauncher_SatisfiesShardLaunchAPI(t *testing.T) {
	var _ ShardLaunchAPI = (*DockerLauncher)(nil)
}
