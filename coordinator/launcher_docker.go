package coordinator

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// DockerLauncher starts shard containers for integration tests.
// Requires the weft shard image to be built (docker compose build).
type DockerLauncher struct {
	mu          sync.Mutex
	image       string
	network     string
	shardSecret string
	containers  map[string]string // addr → container ID
	nextPort    int
}

// NewDockerLauncher creates a launcher that starts containers from the given
// image on the given Docker network. shardSecret is attached as Bearer on
// the Configure probe the launcher runs; must match the SHARD_SECRET the
// container itself picks up.
func NewDockerLauncher(image, network, shardSecret string) *DockerLauncher {
	return &DockerLauncher{
		image:       image,
		network:     network,
		shardSecret: shardSecret,
		containers:  make(map[string]string),
		nextPort:    19000, // start from a high port range
	}
}

func (d *DockerLauncher) Launch(ctx context.Context, tier string, cfg *pb.ConfigureRequest) (string, error) {
	d.mu.Lock()
	port := d.nextPort
	d.nextPort++
	d.mu.Unlock()

	// Start a blank shard container (no --window-start/--window-end).
	name := fmt.Sprintf("weft-shard-bench-%d", port)
	args := []string{
		"run", "-d", "--rm",
		"--name", name,
		"--network", d.network,
		"-p", fmt.Sprintf("%d:9000", port),
		d.image,
		"weft-shard",
		"--grpc-port=9000",
		"--http-port=9001",
	}

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run: %w\n%s", err, out)
	}

	containerID := strings.TrimSpace(string(out))
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	d.mu.Lock()
	d.containers[addr] = containerID
	d.mu.Unlock()

	// Wait for the shard gRPC to be reachable, then call Configure.
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		conn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithUnaryInterceptor(auth.ClientTokenUnaryInterceptor(d.shardSecret)),
			grpc.WithStreamInterceptor(auth.ClientTokenStreamInterceptor(d.shardSecret)),
		)
		if err != nil {
			continue
		}
		if cfg != nil {
			client := pb.NewShardQueryClient(conn)
			if _, err := client.Configure(ctx, cfg); err != nil {
				conn.Close()
				continue
			}
		}
		conn.Close()
		return addr, nil
	}

	return addr, nil // optimistic — gRPC client will retry
}

func (d *DockerLauncher) Terminate(_ context.Context, addr string) error {
	d.mu.Lock()
	containerID, ok := d.containers[addr]
	delete(d.containers, addr)
	d.mu.Unlock()

	if ok {
		exec.Command("docker", "rm", "-f", containerID).Run()
	}
	return nil
}

// Close terminates all containers.
func (d *DockerLauncher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.containers {
		exec.Command("docker", "rm", "-f", id).Run()
	}
	d.containers = make(map[string]string)
}
