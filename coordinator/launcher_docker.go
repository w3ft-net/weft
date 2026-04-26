package coordinator

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

// LaunchInstance starts a fresh shard container, returns its
// container-internal address. The coordinator will Configure it and
// then receive heartbeats once it boots.
func (d *DockerLauncher) LaunchInstance(ctx context.Context, _ string) (string, error) {
	d.mu.Lock()
	if d.HostDataRoot == "" || d.LocalDataRoot == "" || d.Image == "" {
		d.mu.Unlock()
		return "", fmt.Errorf("docker launcher misconfigured: HostDataRoot/LocalDataRoot/Image required")
	}
	d.mu.Unlock()

	// Mint a per-shard subdirectory name. We use a random hex prefix
	// instead of the eventual shard id because Configure (which mints
	// the durable id) hasn't been called yet — the dataDir name is
	// internal to the launcher.
	suffix := newSubdirSuffix()
	hostSubdir := filepath.Join(d.HostDataRoot, suffix)
	localSubdir := filepath.Join(d.LocalDataRoot, suffix)
	if err := os.MkdirAll(localSubdir, 0o755); err != nil {
		return "", fmt.Errorf("create local subdir %s: %w", localSubdir, err)
	}

	containerName := fmt.Sprintf("weft-shard-%s", suffix)
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", d.Network,
		"-v", hostSubdir + ":/opt/app/data",
		"-e", "ROLE=shard",
		"-e", "SHARD_GRPC_PORT=9000",
		"-e", "SHARD_HTTP_PORT=9001",
		"-e", "SHARD_DATA_DIR=/opt/app/data",
		"-e", "COORDINATOR_ADDR=" + d.CoordinatorAddr,
		"-e", "HEARTBEAT_INTERVAL=" + d.HeartbeatInterval.String(),
		"-e", "SHARD_SECRET=" + d.ShardSecret,
		"--entrypoint", "/usr/local/bin/entrypoint.sh",
		d.Image,
	}

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run: %w\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))

	// The coordinator dials the shard by container DNS name on the
	// shared network (port 9000 is the shard's gRPC). The Heartbeat
	// RPC the shard sends back will carry the same host:port as Addr,
	// so the coordinator's adopt-from-heartbeat path can reconcile.
	addr := containerName + ":9000"

	// Wait for the shard's gRPC port to be reachable from inside the
	// coordinator container. exec.LookPath returns the docker CLI we
	// already verified at startup, so this is purely about giving the
	// inner process time to bind.
	if err := waitForTCP(ctx, addr, 30*time.Second); err != nil {
		exec.Command("docker", "rm", "-f", containerID).Run()
		return "", fmt.Errorf("shard %s did not become reachable: %w", containerName, err)
	}

	d.mu.Lock()
	if d.containers == nil {
		d.containers = make(map[string]string)
	}
	d.containers[addr] = containerID
	d.mu.Unlock()
	return addr, nil
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
