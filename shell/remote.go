package shell

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardservice "github.com/kevlarcon/weft/shard/service"
)

// RemoteCoordinator connects to a coordinator gRPC service and provides
// the Query interface the shell needs.
type RemoteCoordinator struct {
	conn   *grpc.ClientConn
	client pb.WeftCoordinatorClient
}

// RemoteQueryResult mirrors coordinator.QueryResult for the shell.
type RemoteQueryResult struct {
	Rows []shard.LogRow
	Text string
}

// NewRemoteCoordinator connects to a coordinator at the given address.
func NewRemoteCoordinator(addr string) (*RemoteCoordinator, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to coordinator at %s: %w", addr, err)
	}
	return &RemoteCoordinator{
		conn:   conn,
		client: pb.NewWeftCoordinatorClient(conn),
	}, nil
}

func (r *RemoteCoordinator) Close() error {
	return r.conn.Close()
}

// Query executes a pipeline against the remote coordinator.
func (r *RemoteCoordinator) Query(pipeline string, start, end time.Time) (*RemoteQueryResult, error) {
	resp, err := r.client.Query(context.Background(), &pb.CoordinatorQueryRequest{
		Pipeline:    pipeline,
		TimeStartNs: start.UnixNano(),
		TimeEndNs:   end.UnixNano(),
	})
	if err != nil {
		return nil, err
	}

	result := &RemoteQueryResult{Text: resp.Text}
	for _, r := range resp.Rows {
		result.Rows = append(result.Rows, shardservice.ProtoToLogRow(r))
	}
	return result, nil
}

// ListApps returns the available app names from the coordinator.
func (r *RemoteCoordinator) ListApps() ([]string, error) {
	resp, err := r.client.ListApps(context.Background(), &pb.ListAppsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Apps, nil
}
