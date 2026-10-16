package logdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/graybeardcorp/weft/internal/weftlog"
)

// DockerLauncher is the dev-mode shard launcher: it spawns sibling
// containers running ROLE=shard against the same image the coordinator
// itself was built from, with a host-bind-mounted dataDir so each
// shard's DuckDB + shard.json survive across coordinator process
// boundaries (the production-alignment goal — see
// docs/plans/multi-customer-demo-seed.md and the restart_test.go
// integration suite).
//
// Implements ShardLauncher: synchronous LaunchInstance returns the
// host:port a coordinator can immediately Configure+heartbeat against.
// The launched shard's entrypoint.sh ROLE=shard branch wires its own
// heartbeat back to the coordinator over the docker network.
//
// Wiring:
//
//   - the coordinator container must have /var/run/docker.sock mounted
//     so we can speak to the host daemon and spawn sibling containers
//   - the host data root (e.g. ./dev/shard-data) must be mounted into
//     BOTH the coordinator (for visibility/cleanup) and each spawned
//     shard (for DuckDB persistence). HostDataRoot is the host-side
//     absolute path; LocalDataRoot is where the same volume is mounted
//     inside the coordinator
//   - the docker network name (from compose) so the shard container
//     joins the same network and can resolve "coordinator" by service
//     name when sending heartbeats
type DockerLauncher struct {
	mu sync.Mutex

	// Image to launch. Should be the same image the coordinator runs
	// from — that image already has weft-shard + entrypoint.sh
	// installed at /usr/local/bin/.
	Image string

	// Network is the docker network name the spawned containers
	// attach to (so they can resolve "coordinator" by service name).
	Network string

	// HostDataRoot is the absolute host-side path of the directory
	// holding per-shard data subdirectories. Each LaunchInstance
	// creates HostDataRoot/<unique-id>/ on the host (visible inside
	// the coordinator at LocalDataRoot/<unique-id>/) and bind-mounts
	// it to /opt/app/data inside the spawned shard container.
	HostDataRoot string

	// LocalDataRoot is the path inside the coordinator container
	// where HostDataRoot is mounted. Used to mkdir per-shard subdirs
	// before the shard container picks them up.
	LocalDataRoot string

	// CoordinatorAddr is the address (within the docker network) the
	// shard sends heartbeats to — typically "coordinator:9020".
	CoordinatorAddr string

	// HeartbeatInterval is the cadence the spawned shards heartbeat at.
	HeartbeatInterval time.Duration

	// ShardSecret matches SHARD_SECRET on the coordinator and the
	// shared-token interceptor on the shard's gRPC server.
	ShardSecret string

	// containers maps addr → container ID for TerminateInstance.
	containers map[string]string
}

// NewDockerLauncher returns a launcher with sensible defaults; callers
// must set HostDataRoot/LocalDataRoot/CoordinatorAddr/Image at minimum.
func NewDockerLauncher() *DockerLauncher {
	return &DockerLauncher{
		HeartbeatInterval: 10 * time.Second,
		containers:        make(map[string]string),
	}
}

// devAZ is the single synthetic availability zone dev mode stripes every
// shard into — there's one docker host, so AZ-spreading is a local no-op.
// Mirrors EC2Provisioner's AZsForTier/StripeAZ so the bucket SM's striper
// lookups resolve identically in dev and prod.
const devAZ = "local"

// LaunchInstance starts a fresh shard container, returns its
// container-internal address. The coordinator will Configure it and
// then receive heartbeats once it boots.
//
// This is the legacy synchronous launcher the old reconciler drives; the
// FSM control plane uses Launch (below) instead. It stays until the Slice
// C switch deletes the reconciler.
func (d *DockerLauncher) LaunchInstance(ctx context.Context, _ string) (string, error) {
	_, addr, containerID, err := d.spawn(ctx, newSubdirSuffix(), false)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	if d.containers == nil {
		d.containers = make(map[string]string)
	}
	d.containers[addr] = containerID
	d.mu.Unlock()
	return addr, nil
}

// Launch is the FSM control plane's launch primitive (ShardLaunchAPI),
// the dev counterpart to EC2Provisioner.Launch. It spawns one shard
// container with a forced identity: the synthetic instance id is the
// container name, which is also its --hostname, so two things line up at
// once — the addr the shard advertises (hostname:9000) is dialable on the
// docker network, and the instance id it heartbeats (SHARD_INSTANCE_ID)
// matches what the SM stamps onto the LAUNCHING entry from this result.
// That instance id is the correlation key heartbeatMatchesLaunching
// checks. No shard id is injected — the shard boots unconfigured and the
// SM sends Configure on the first heartbeat.
//
// Dev has no EBS, so VolumeID stays empty (the host bind-mount holds the
// data dir); recovery's volume-reattach path is a prod-only concern.
func (d *DockerLauncher) Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error) {
	cname, _, _, err := d.spawn(ctx, newSubdirSuffix(), true)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("launch %s: %w", spec.ShardID, err)
	}
	az := spec.AZ
	if az == "" {
		az = devAZ
	}
	weftlog.Infof(ctx, "docker-launcher: launched %s as container %s (az=%s)", spec.ShardID, cname, az)
	return LaunchResult{InstanceID: cname, AZ: az}, nil
}

// Terminate removes the container behind instanceID (which is its name,
// see Launch) and decrements nothing — there's no ASG in dev. A missing
// container is success, so the ARCHIVED/GONE cleanup paths are idempotent.
func (d *DockerLauncher) Terminate(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("Terminate: instance id required")
	}
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", instanceID).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm -f %s: %w\n%s", instanceID, err, out)
	}
	return nil
}

// AZsForTier / StripeAZ are the dev striper. One synthetic AZ, so every
// stripe index lands in the same place — AZ-spreading is meaningless on a
// single docker host, but the bucket SM consults the striper unaware of
// the environment.
func (d *DockerLauncher) AZsForTier(string) []string  { return []string{devAZ} }
func (d *DockerLauncher) StripeAZ(string, int) string { return devAZ }

// spawn runs one shard container off the given subdir suffix and returns
// it once its gRPC port is reachable. When forceIdentity is set the
// container gets --hostname <name> plus -e SHARD_INSTANCE_ID=<name>, so
// the shard advertises a dialable addr and reports the instance id the SM
// correlates against (see Launch); the legacy LaunchInstance leaves both
// off. Shared by both launch paths so the docker-run plumbing lives once.
func (d *DockerLauncher) spawn(ctx context.Context, suffix string, forceIdentity bool) (containerName, addr, containerID string, err error) {
	d.mu.Lock()
	misconfigured := d.HostDataRoot == "" || d.LocalDataRoot == "" || d.Image == ""
	d.mu.Unlock()
	if misconfigured {
		return "", "", "", fmt.Errorf("docker launcher misconfigured: HostDataRoot/LocalDataRoot/Image required")
	}

	// The per-shard subdir uses a random hex prefix, not the eventual
	// shard id — for the legacy path Configure (which mints the id) hasn't
	// run yet, and for Launch the id lives in shard.json once configured.
	hostSubdir := filepath.Join(d.HostDataRoot, suffix)
	localSubdir := filepath.Join(d.LocalDataRoot, suffix)
	if err := os.MkdirAll(localSubdir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create local subdir %s: %w", localSubdir, err)
	}

	containerName = fmt.Sprintf("weft-shard-%s", suffix)
	args := d.runArgs(containerName, hostSubdir, forceIdentity)

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", "", "", fmt.Errorf("docker run: %w\n%s", err, out)
	}
	containerID = strings.TrimSpace(string(out))

	// The coordinator dials the shard by container DNS name on the shared
	// network (9000 is the shard's gRPC). The Heartbeat RPC carries the
	// same host:port as Addr so the heartbeat path can reconcile.
	addr = containerName + ":9000"
	if err := waitForTCP(ctx, addr, 30*time.Second); err != nil {
		exec.Command("docker", "rm", "-f", containerID).Run()
		return "", "", "", fmt.Errorf("shard %s did not become reachable: %w", containerName, err)
	}
	return containerName, addr, containerID, nil
}

// runArgs builds the `docker run` argv for a shard container. Pulled out
// as a pure function so the forced-identity wiring is unit-testable
// without a docker daemon.
func (d *DockerLauncher) runArgs(containerName, hostSubdir string, forceIdentity bool) []string {
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", d.Network,
		"-v", hostSubdir + ":/opt/app/data",
	}
	if forceIdentity {
		// --hostname makes the shard's os.Hostname()-derived addr equal
		// containerName (dialable on the network); SHARD_INSTANCE_ID is
		// the synthetic id it heartbeats back for SM correlation.
		args = append(args, "--hostname", containerName, "-e", "SHARD_INSTANCE_ID="+containerName)
	}
	args = append(args,
		"-e", "ROLE=shard",
		"-e", "SHARD_GRPC_PORT=9000",
		"-e", "SHARD_HTTP_PORT=9001",
		"-e", "SHARD_DATA_DIR=/opt/app/data",
		"-e", "COORDINATOR_ADDR="+d.CoordinatorAddr,
		"-e", "HEARTBEAT_INTERVAL="+d.HeartbeatInterval.String(),
		"-e", "SHARD_SECRET="+d.ShardSecret,
		"--entrypoint", "/usr/local/bin/entrypoint.sh",
		d.Image,
	)
	return args
}

// TerminateInstance removes the shard container at addr. The data
// subdirectory on the host is intentionally left in place — a future
// pass can adopt it by re-launching with the same dataDir, so an
// accidental shard kill doesn't lose data.
func (d *DockerLauncher) TerminateInstance(_ context.Context, _, addr string) error {
	d.mu.Lock()
	containerID, ok := d.containers[addr]
	delete(d.containers, addr)
	d.mu.Unlock()
	if !ok {
		return nil // already gone or never tracked here
	}
	return exec.Command("docker", "rm", "-f", containerID).Run()
}

// Close terminates every tracked container. Used for test cleanup.
func (d *DockerLauncher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.containers {
		exec.Command("docker", "rm", "-f", id).Run()
	}
	d.containers = make(map[string]string)
}

// newSubdirSuffix returns 16 hex chars of randomness — enough that
// concurrent LaunchInstance calls from the same coordinator can't
// collide. The shard's durable id (minted at Configure) is unrelated
// and lives inside shard.json once the shard is christened.
func newSubdirSuffix() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// waitForTCP polls addr (host:port) until a TCP dial succeeds or the
// timeout elapses. Used right after `docker run -d` so we don't return
// from LaunchInstance before Configure has any chance to land.
func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("tcp dial %s timed out after %s", addr, timeout)
}
