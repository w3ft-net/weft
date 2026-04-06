package shell

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// RemoteCoordinator connects to a coordinator gRPC service and provides
// the Query interface the shell needs.
type RemoteCoordinator struct {
	conn   *grpc.ClientConn
	client pb.WeftCoordinatorClient
}

// RemoteQueryResult mirrors coordinator.QueryResult for the shell.
type RemoteQueryResult struct {
	Rows []*pb.LogRow
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
	stream, err := r.client.Query(context.Background(), &pb.CoordinatorQueryRequest{
		Pipeline:    pipeline,
		TimeStartNs: start.UnixNano(),
		TimeEndNs:   end.UnixNano(),
	})
	if err != nil {
		return nil, err
	}

	result := &RemoteQueryResult{}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if resp.Text != "" {
			result.Text += resp.Text
		}
		result.Rows = append(result.Rows, resp.Rows...)
	}
	return result, nil
}

// Follow opens a streaming query with follow mode. It calls onRow for each
// row (both historical and live) until the context is cancelled or the stream ends.
func (r *RemoteCoordinator) Follow(ctx context.Context, pipeline string, onRow func(*pb.LogRow)) error {
	stream, err := r.client.Query(ctx, &pb.CoordinatorQueryRequest{
		Pipeline: pipeline,
		Follow:   true,
	})
	if err != nil {
		return err
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		for _, row := range resp.Rows {
			onRow(row)
		}
	}
}

// ListApps returns the available app names from the coordinator.
func (r *RemoteCoordinator) ListApps() ([]string, error) {
	resp, err := r.client.ListApps(context.Background(), &pb.ListAppsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Apps, nil
}

// AddSource registers a log source on the coordinator.
func (r *RemoteCoordinator) AddSource(app, customer, logGroup, region, schemaYAML, pollInterval, awsAccountID, roleARN, externalID string) error {
	_, err := r.client.AddSource(context.Background(), &pb.AddSourceRequest{
		App:          app,
		Customer:     customer,
		LogGroup:     logGroup,
		Region:       region,
		SchemaYaml:   schemaYAML,
		PollInterval: pollInterval,
		AwsAccountId: awsAccountID,
		RoleArn:      roleARN,
		ExternalId:   externalID,
	})
	return err
}

// SourceInfo holds info about a configured log source.
type SourceInfo struct {
	App        string
	Customer   string
	LogGroup   string
	Region     string
	Enabled    bool
	LastPollAt string
}

// ListSources returns all configured log sources.
func (r *RemoteCoordinator) ListSources() ([]SourceInfo, error) {
	resp, err := r.client.ListSources(context.Background(), &pb.ListSourcesRequest{})
	if err != nil {
		return nil, err
	}
	var sources []SourceInfo
	for _, s := range resp.Sources {
		sources = append(sources, SourceInfo{
			App:        s.App,
			Customer:   s.Customer,
			LogGroup:   s.LogGroup,
			Region:     s.Region,
			Enabled:    s.Enabled,
			LastPollAt: s.LastPollAt,
		})
	}
	return sources, nil
}

// RemoveSource deletes a log source.
func (r *RemoteCoordinator) RemoveSource(app string) error {
	_, err := r.client.RemoveSource(context.Background(), &pb.RemoveSourceRequest{App: app})
	return err
}

// --- Token management ---

func (r *RemoteCoordinator) CreateToken(customer, label, app string) (string, error) {
	resp, err := r.client.CreateToken(context.Background(), &pb.CreateTokenRequest{
		Customer: customer,
		Label:    label,
		App:      app,
	})
	if err != nil {
		return "", err
	}
	return resp.Token, nil
}

type TokenInfo struct {
	Token     string
	Label     string
	App       string
	Revoked   bool
	CreatedAt string
}

func (r *RemoteCoordinator) ListTokens(customer string) ([]TokenInfo, error) {
	resp, err := r.client.ListTokens(context.Background(), &pb.ListTokensRequest{Customer: customer})
	if err != nil {
		return nil, err
	}
	var tokens []TokenInfo
	for _, t := range resp.Tokens {
		tokens = append(tokens, TokenInfo{
			Token:     t.Token,
			Label:     t.Label,
			App:       t.App,
			Revoked:   t.Revoked,
			CreatedAt: t.CreatedAt,
		})
	}
	return tokens, nil
}

func (r *RemoteCoordinator) RevokeToken(token string) error {
	_, err := r.client.RevokeToken(context.Background(), &pb.RevokeTokenRequest{Token: token})
	return err
}
