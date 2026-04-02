package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/kevlarcon/weft/configstore"
	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	shardpkg "github.com/kevlarcon/weft/shard"
	shardservice "github.com/kevlarcon/weft/shard/service"
)

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord *coordinator.Coordinator
	Store *configstore.Store
	Apps  []string
}

func (s *Server) Query(req *pb.CoordinatorQueryRequest, stream pb.WeftCoordinator_QueryServer) error {
	tr := coordinator.TimeRange{
		Start: time.Unix(0, req.TimeStartNs),
		End:   time.Unix(0, req.TimeEndNs),
	}
	if req.TimeStartNs == 0 && req.TimeEndNs == 0 {
		tr.Start = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		tr.End = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	result, err := s.Coord.Query(req.Pipeline, tr)
	if err != nil {
		return err
	}

	if result.Text != "" {
		return stream.Send(&pb.CoordinatorQueryResponse{Text: result.Text})
	}

	// Stream rows in batches to stay under gRPC message size limit.
	const batchSize = 500
	for i := 0; i < len(result.Rows); i += batchSize {
		end := i + batchSize
		if end > len(result.Rows) {
			end = len(result.Rows)
		}
		resp := &pb.CoordinatorQueryResponse{}
		for _, r := range result.Rows[i:end] {
			resp.Rows = append(resp.Rows, shardservice.LogRowToProto(&r))
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ListApps(_ context.Context, _ *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	if s.Store != nil {
		apps, err := s.Store.AppNames()
		if err == nil && len(apps) > 0 {
			return &pb.ListAppsResponse{Apps: apps}, nil
		}
	}
	return &pb.ListAppsResponse{Apps: s.Apps}, nil
}

func (s *Server) AddSource(_ context.Context, req *pb.AddSourceRequest) (*pb.AddSourceResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}

	customer, err := s.Store.FindOrCreateCustomer(req.Customer)
	if err != nil {
		return nil, fmt.Errorf("customer: %w", err)
	}

	account, err := s.Store.FindOrCreateAccount(customer.ID, req.AwsAccountId, req.RoleArn, req.ExternalId)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}

	pollInterval := req.PollInterval
	if pollInterval == "" {
		pollInterval = "30s"
	}
	if err := s.Store.AddLogGroup(account.ID, req.App, req.LogGroup, req.Region, req.SchemaYaml, pollInterval); err != nil {
		return nil, fmt.Errorf("log group: %w", err)
	}

	return &pb.AddSourceResponse{}, nil
}

func (s *Server) ListSources(_ context.Context, _ *pb.ListSourcesRequest) (*pb.ListSourcesResponse, error) {
	if s.Store == nil {
		return &pb.ListSourcesResponse{}, nil
	}
	logGroups, err := s.Store.AllLogGroups()
	if err != nil {
		return nil, err
	}
	var infos []*pb.SourceInfo
	for _, lg := range logGroups {
		infos = append(infos, &pb.SourceInfo{
			App:        lg.App,
			Customer:   lg.CustomerName,
			LogGroup:   lg.LogGroupName,
			Region:     lg.Region,
			Enabled:    lg.Enabled,
			LastPollAt: lg.LastPollAt,
		})
	}
	return &pb.ListSourcesResponse{Sources: infos}, nil
}

func (s *Server) RemoveSource(_ context.Context, req *pb.RemoveSourceRequest) (*pb.RemoveSourceResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}
	if err := s.Store.RemoveLogGroup(req.App); err != nil {
		return nil, err
	}
	return &pb.RemoveSourceResponse{}, nil
}

// Authenticate validates an agent token. Agents stream through the
// coordinator via IngestStream — they never connect to shards directly.
func (s *Server) Authenticate(_ context.Context, req *pb.AuthenticateRequest) (*pb.AuthenticateResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}

	customer, err := s.Store.ValidateAgentToken(req.Token)
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	if customer == nil {
		return nil, fmt.Errorf("invalid or revoked token")
	}

	log.Printf("auth: customer=%s", customer.Name)

	return &pb.AuthenticateResponse{
		Customer: customer.Name,
	}, nil
}

func (s *Server) CreateToken(_ context.Context, req *pb.CreateTokenRequest) (*pb.CreateTokenResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}
	customer, err := s.Store.FindOrCreateCustomer(req.Customer)
	if err != nil {
		return nil, fmt.Errorf("customer: %w", err)
	}
	token, err := s.Store.CreateAgentToken(customer.ID, req.Label)
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	return &pb.CreateTokenResponse{Token: token.Token}, nil
}

func (s *Server) ListTokens(_ context.Context, req *pb.ListTokensRequest) (*pb.ListTokensResponse, error) {
	if s.Store == nil {
		return &pb.ListTokensResponse{}, nil
	}
	customer, err := s.Store.FindOrCreateCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	tokens, err := s.Store.ListAgentTokens(customer.ID)
	if err != nil {
		return nil, err
	}
	var infos []*pb.TokenInfo
	for _, t := range tokens {
		infos = append(infos, &pb.TokenInfo{
			Token:     t.Token,
			Label:     t.Label,
			Revoked:   t.Revoked,
			CreatedAt: t.CreatedAt.Format(time.RFC3339),
		})
	}
	return &pb.ListTokensResponse{Tokens: infos}, nil
}

func (s *Server) RevokeToken(_ context.Context, req *pb.RevokeTokenRequest) (*pb.RevokeTokenResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}
	if err := s.Store.RevokeAgentToken(req.Token); err != nil {
		return nil, err
	}
	return &pb.RevokeTokenResponse{}, nil
}

// IngestStream accepts a stream of LogRows from weftd agents and forwards
// them to the active shard in batches. Flushes every 2 seconds or 100 rows.
func (s *Server) IngestStream(stream pb.WeftCoordinator_IngestStreamServer) error {
	var mu sync.Mutex
	var rows []shardpkg.LogRow
	var count int64

	// Periodic flush for sparse streams.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	go func() {
		for range ticker.C {
			mu.Lock()
			if len(rows) > 0 {
				s.ingestBatch(rows)
				rows = rows[:0]
			}
			mu.Unlock()
		}
	}()

	for {
		row, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		mu.Lock()
		rows = append(rows, shardservice.ProtoToLogRow(row))
		count++
		if len(rows) >= 100 {
			s.ingestBatch(rows)
			rows = rows[:0]
		}
		mu.Unlock()
	}

	mu.Lock()
	if len(rows) > 0 {
		s.ingestBatch(rows)
	}
	mu.Unlock()

	log.Printf("ingest-stream: received %d rows", count)
	return stream.SendAndClose(&pb.IngestStreamResponse{RowsReceived: count})
}

func (s *Server) ingestBatch(rows []shardpkg.LogRow) {
	for _, sh := range s.Coord.GetShards() {
		if err := sh.Ingest(rows); err != nil {
			continue
		}
		return
	}
	log.Printf("ingest-stream: all shards rejected batch of %d rows", len(rows))
}
