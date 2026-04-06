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
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord *coordinator.Coordinator
	Store *configstore.Store
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
		resp := &pb.CoordinatorQueryResponse{
			Rows: result.Rows[i:end],
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}

	if !req.Follow || s.Coord.Tail == nil {
		return nil
	}

	// Follow mode: subscribe and stream live rows until client disconnects.
	plan, err := planner.Plan(req.Pipeline)
	if err != nil {
		return err
	}

	matcher := &coordinator.RowMatcher{
		App:      plan.FollowApp,
		Patterns: plan.FollowPatterns,
	}

	ctx := stream.Context()
	id, ch := s.Coord.Tail.Subscribe(ctx, matcher)
	defer s.Coord.Tail.Unsubscribe(id)

	for {
		select {
		case <-ctx.Done():
			return nil
		case row, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&pb.CoordinatorQueryResponse{
				Rows: []*pb.LogRow{row},
			}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) ListApps(_ context.Context, _ *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	if s.Store == nil {
		return &pb.ListAppsResponse{}, nil
	}

	seen := make(map[string]bool)
	var apps []string

	// Apps with weftd agent tokens.
	if agentApps, err := s.Store.AgentAppNames(); err == nil {
		for _, a := range agentApps {
			if !seen[a] {
				seen[a] = true
				apps = append(apps, a)
			}
		}
	}

	// Apps with CloudWatch polling sources.
	if pollerApps, err := s.Store.AppNames(); err == nil {
		for _, a := range pollerApps {
			if !seen[a] {
				seen[a] = true
				apps = append(apps, a)
			}
		}
	}

	return &pb.ListAppsResponse{Apps: apps}, nil
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
	token, err := s.Store.CreateAgentToken(customer.ID, req.Label, req.App)
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
			App:       t.App,
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
	var rows []*pb.LogRow
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
		rows = append(rows, row)
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

func (s *Server) ingestBatch(rows []*pb.LogRow) {
	for _, sh := range s.Coord.GetShards() {
		if err := sh.Ingest(rows); err != nil {
			continue
		}
		break
	}

	// Fan out to tail -f subscribers.
	if s.Coord.Tail != nil {
		s.Coord.Tail.Broadcast(rows)
	}
}
