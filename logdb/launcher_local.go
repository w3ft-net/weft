package logdb

import (
	"context"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"net"
	"sync"

	"google.golang.org/grpc"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	shardsvc "github.com/w3ft-net/weft/shard/service"
)

// LocalLauncher starts in-process gRPC shard servers on random ports.
// Lightweight stand-in used by unit tests in this package — exercises
// the launcher interface without needing a real shard binary or a
// docker socket. NOT used by the production-aligned dev path; see
// DockerLauncher for that.
//
// Renamed from LocalLauncher (exported) when dev mode moved to
// DockerLauncher; kept around so unit tests like coordinator_restart_
// test, archiver_test, etc. don't have to be rewritten against a
// fake. Anything that needs persistence across coordinator process
// restarts must use DockerLauncher — these in-process servers die
// with the process.
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

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(weftsdk.GRPC.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(weftsdk.GRPC.StreamServerInterceptor()),
	)
	pb.RegisterShardQueryServer(srv, shardsvc.NewServer(nil))
	go srv.Serve(lis)

	addr := lis.Addr().String()

	l.mu.Lock()
	l.servers[addr] = &localShard{srv: srv, lis: lis}
	l.mu.Unlock()

	return addr, nil
}

func (l *LocalLauncher) TerminateInstance(_ context.Context, _, addr string) error {
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
