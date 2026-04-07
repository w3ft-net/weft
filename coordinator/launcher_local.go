package coordinator

import (
	"context"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	shardsvc "github.com/kevlarcon/weft/shard/service"
)

// LocalLauncher starts in-process DuckDB shards on random ports.
// Used for unit tests — no Docker, no network, fast and deterministic.
type LocalLauncher struct {
	mu      sync.Mutex
	servers map[string]*localShard
}

type localShard struct {
	srv *grpc.Server
	lis net.Listener
}

func NewLocalLauncher() *LocalLauncher {
	return &LocalLauncher{
		servers: make(map[string]*localShard),
	}
}

func (l *LocalLauncher) Launch(ctx context.Context, tier string, cfg *pb.ConfigureRequest) (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	// Start a blank shard server (nil DuckDBShard — waits for Configure).
	srv := grpc.NewServer()
	pb.RegisterShardQueryServer(srv, shardsvc.NewServer(nil))
	go srv.Serve(lis)

	addr := lis.Addr().String()

	// Call Configure RPC — same path as production.
	if cfg != nil {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			srv.GracefulStop()
			return "", fmt.Errorf("connect for configure: %w", err)
		}
		client := pb.NewShardQueryClient(conn)
		if _, err := client.Configure(ctx, cfg); err != nil {
			conn.Close()
			srv.GracefulStop()
			return "", fmt.Errorf("configure shard: %w", err)
		}
		conn.Close()
	}

	l.mu.Lock()
	l.servers[addr] = &localShard{srv: srv, lis: lis}
	l.mu.Unlock()

	return addr, nil
}

func (l *LocalLauncher) Terminate(_ context.Context, addr string) error {
	l.mu.Lock()
	ls, ok := l.servers[addr]
	delete(l.servers, addr)
	l.mu.Unlock()

	if ok {
		ls.srv.GracefulStop()
	}
	return nil
}

// Close terminates all running shards.
func (l *LocalLauncher) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ls := range l.servers {
		ls.srv.GracefulStop()
	}
	l.servers = make(map[string]*localShard)
}
