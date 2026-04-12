package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/store"
	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord *coordinator.Coordinator
	Store *store.Store
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

	if result.Text != "" || result.Stderr != "" {
		text := result.Text
		if result.Stderr != "" {
			// Render stderr in red ANSI so the shell displays it appropriately.
			text += "\033[38;5;1m" + result.Stderr + "\033[0m"
		}
		return stream.Send(&pb.CoordinatorQueryResponse{Text: text})
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

	names, err := s.Store.AllAppNames()
	if err != nil {
		return &pb.ListAppsResponse{}, nil
	}

	return &pb.ListAppsResponse{Apps: names}, nil
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

	// Check for an active subscription.
	sub, err := s.Store.ActiveSubscription(customer.Name)
	if err != nil || sub == nil {
		log.Printf("auth: customer=%s denied (no active subscription)", customer.Name)
		return nil, status.Errorf(codes.PermissionDenied,
			"no active subscription — upgrade at w3ft.net/dashboard")
	}

	log.Printf("auth: customer=%s tier=%s", customer.Name, sub.Tier)

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
	// Write to ring buffer (non-blocking). Background drain pushes to shards.
	if s.Coord.Ingest != nil {
		s.Coord.Ingest.Write(rows)
	} else {
		// Fallback: direct shard write (dev mode without ring buffer).
		for _, sh := range s.Coord.GetShards() {
			if err := sh.Ingest(rows); err != nil {
				continue
			}
			break
		}
	}

	// Fan out to tail -f subscribers.
	if s.Coord.Tail != nil {
		s.Coord.Tail.Broadcast(rows)
	}
}

// GetUsage returns bytes ingested for a customer in a billing period.
// Combines persisted data from PostgreSQL with the current in-memory hour.
func (s *Server) GetUsage(_ context.Context, req *pb.GetUsageRequest) (*pb.GetUsageResponse, error) {
	period := req.Period
	if period == "" {
		period = time.Now().UTC().Format("2006-01")
	}

	t, err := time.Parse("2006-01", period)
	if err != nil {
		return nil, fmt.Errorf("invalid period %q: expected YYYY-MM", period)
	}
	periodStart := t
	periodEnd := t.AddDate(0, 1, 0)

	var dbBytes int64
	if s.Store != nil {
		dbBytes, err = s.Store.QueryUsage(req.Customer, periodStart, periodEnd)
		if err != nil {
			log.Printf("GetUsage DB query error: %v", err)
			// Continue with in-memory only.
		}
	}

	// Add the current (not-yet-flushed) hour.
	var memBytes int64
	if s.Coord.Usage != nil {
		memBytes = s.Coord.Usage.CurrentHour(req.Customer)
	}

	return &pb.GetUsageResponse{
		BytesIngested: dbBytes + memBytes,
		PeriodStart:   periodStart.Format(time.RFC3339),
		PeriodEnd:     periodEnd.Format(time.RFC3339),
	}, nil
}

// ── Shell functions (/bin/) ─────────────────────────────────

func (s *Server) resolveCustomer(customerName string) (uint, error) {
	if s.Store == nil {
		return 0, fmt.Errorf("config store not available")
	}
	c, err := s.Store.FindOrCreateCustomer(customerName)
	if err != nil {
		return 0, err
	}
	return c.ID, nil
}

func (s *Server) SaveFunction(_ context.Context, req *pb.SaveFunctionRequest) (*pb.SaveFunctionResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SaveShellFunction(custID, req.Name, req.Source); err != nil {
		return nil, err
	}
	return &pb.SaveFunctionResponse{}, nil
}

func (s *Server) GetFunction(_ context.Context, req *pb.GetFunctionRequest) (*pb.GetFunctionResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	fn, err := s.Store.GetShellFunction(custID, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%s: not found", req.Name)
	}
	return &pb.GetFunctionResponse{Name: fn.Name, Source: fn.Source}, nil
}

func (s *Server) ListFunctions(_ context.Context, req *pb.ListFunctionsRequest) (*pb.ListFunctionsResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	fns, err := s.Store.ListShellFunctions(custID)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(fns))
	for i, f := range fns {
		names[i] = f.Name
	}
	return &pb.ListFunctionsResponse{Names: names}, nil
}

func (s *Server) DeleteFunction(_ context.Context, req *pb.DeleteFunctionRequest) (*pb.DeleteFunctionResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteShellFunction(custID, req.Name); err != nil {
		return nil, err
	}
	return &pb.DeleteFunctionResponse{}, nil
}

// ── Cron watches (/cron.d/) ─────────────────────────────────

func (s *Server) SaveWatch(_ context.Context, req *pb.SaveWatchRequest) (*pb.SaveWatchResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SaveCronWatch(custID, req.Name, req.Schedule, req.Pipeline, req.NotifyDest); err != nil {
		return nil, err
	}
	return &pb.SaveWatchResponse{}, nil
}

func (s *Server) GetWatch(_ context.Context, req *pb.GetWatchRequest) (*pb.GetWatchResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	w, err := s.Store.GetCronWatch(custID, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%s: not found", req.Name)
	}
	return &pb.GetWatchResponse{
		Name:       w.Name,
		Schedule:   w.Schedule,
		Pipeline:   w.Pipeline,
		NotifyDest: w.NotifyDest,
		Enabled:    w.Enabled,
	}, nil
}

func (s *Server) ListWatches(_ context.Context, req *pb.ListWatchesRequest) (*pb.ListWatchesResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	watches, err := s.Store.ListCronWatches(custID)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(watches))
	for i, w := range watches {
		names[i] = w.Name
	}
	return &pb.ListWatchesResponse{Names: names}, nil
}

func (s *Server) GetWatchResults(_ context.Context, req *pb.GetWatchResultsRequest) (*pb.GetWatchResultsResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	w, err := s.Store.GetCronWatch(custID, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%s: not found", req.Name)
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 20
	}
	results, err := s.Store.ListCronResults(w.ID, limit)
	if err != nil {
		return nil, err
	}
	var out []*pb.WatchResult
	for _, r := range results {
		out = append(out, &pb.WatchResult{
			Stdout:     r.Stdout,
			Stderr:     r.Stderr,
			ExitCode:   int32(r.ExitCode),
			DurationMs: r.DurationMs,
			RanAt:      r.RanAt.Format(time.RFC3339),
		})
	}
	return &pb.GetWatchResultsResponse{Results: out}, nil
}

func (s *Server) DeleteWatch(_ context.Context, req *pb.DeleteWatchRequest) (*pb.DeleteWatchResponse, error) {
	custID, err := s.resolveCustomer(req.Customer)
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteCronWatch(custID, req.Name); err != nil {
		return nil, err
	}
	return &pb.DeleteWatchResponse{}, nil
}

