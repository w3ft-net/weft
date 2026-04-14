package coordinator

import (
	"context"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	shardsvc "github.com/kevlarcon/weft/shard/service"
)

// LocalLauncher starts in-process DuckDB shards on random ports.
// Used for dev/docker and tests.
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

func (l *LocalLauncher) LaunchInstance(_ context.Context, _ string) (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	srv := grpc.NewServer()
	pb.RegisterShardQueryServer(srv, shardsvc.NewServer(nil))
	go srv.Serve(lis)

	addr := lis.Addr().String()

	l.mu.Lock()
	l.servers[addr] = &localShard{srv: srv, lis: lis}
	l.mu.Unlock()

	return addr, nil
}

func (l *LocalLauncher) TerminateInstance(_ context.Context, addr string) error {
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
