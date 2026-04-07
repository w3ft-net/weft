package coordinator

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardsvc "github.com/kevlarcon/weft/shard/service"
)

// LocalLauncher starts in-process DuckDB shards on random ports.
// Used for unit tests — no Docker, no network, fast and deterministic.
type LocalLauncher struct {
	mu      sync.Mutex
	servers map[string]*localShard
}

type localShard struct {
	ds     *shard.DuckDBShard
	srv    *grpc.Server
	lis    net.Listener
}

func NewLocalLauncher() *LocalLauncher {
	return &LocalLauncher{
		servers: make(map[string]*localShard),
	}
}

func (l *LocalLauncher) Launch(_ context.Context, tier string) (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen: %w", err)
	}

	farPast := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	ds, err := shard.NewDuckDBShard("local", "", farPast, farFuture)
	if err != nil {
		lis.Close()
		return "", fmt.Errorf("create shard: %w", err)
	}

	srv := grpc.NewServer()
	pb.RegisterShardQueryServer(srv, shardsvc.NewServer(ds))
	go srv.Serve(lis)

	addr := lis.Addr().String()

	l.mu.Lock()
	l.servers[addr] = &localShard{ds: ds, srv: srv, lis: lis}
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
		ls.ds.Close()
	}
	return nil
}

// Close terminates all running shards.
func (l *LocalLauncher) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ls := range l.servers {
		ls.srv.GracefulStop()
		ls.ds.Close()
	}
	l.servers = make(map[string]*localShard)
}
