package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/kevlarcon/weft/auth"
	weftcfg "github.com/kevlarcon/weft/config"
	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/ingest"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// convertNotices maps coordinator-side Notice values to their
// protobuf wire shape. The two types carry the same fields; the
// indirection exists so coordinator/notices.go doesn't need to
// import the proto package.
func convertNotices(in []coordinator.Notice) []*pb.Notice {
	if len(in) == 0 {
		return nil
	}
	out := make([]*pb.Notice, len(in))
	for i, n := range in {
		out[i] = &pb.Notice{
			Key:   n.Key,
			Kind:  string(n.Kind),
			Title: n.Title,
			Body:  n.Body,
		}
	}
	return out
}

// callerCustomer returns the customer name of the authenticated caller,
// derived from JWT claims injected by the auth interceptor. Any non-empty
// `Customer` field on the request proto is IGNORED — clients don't get to
// declare which tenant they are; the verified principal is authoritative.
// Returns an Unauthenticated error when called without an interceptor-
// populated context (shouldn't happen in production because the
// interceptor gates every JWT-protected RPC).
func callerCustomer(ctx context.Context) (string, error) {
	c := auth.FromContext(ctx)
	if c == nil {
		return "", status.Errorf(codes.Unauthenticated, "no authenticated caller")
	}
	if c.CustomerName == "" {
		return "", status.Errorf(codes.PermissionDenied, "authenticated user has no customer")
	}
	return c.CustomerName, nil
}

// callerUserID returns the authenticated user's ID, or "" when no
// claims are present. Used as the userID argument to store writes
// that record creator/displacing user. Empty string is acceptable
// upstream: the store path treats it as "system" attribution.
func callerUserID(ctx context.Context) string {
	c := auth.FromContext(ctx)
	if c == nil {
		return ""
	}
	return c.UserID
}

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord            *coordinator.Coordinator
	Store            *store.Store
	consecutiveDrops int

	// CfgPtr holds the live runtime config. Today only IngestStream
	// consults it (for the per-stream byte budget); nil is fine in
	// tests and non-production callers — handlers fall through to
	// compile-time defaults via runtimeCfg().
	CfgPtr *atomic.Pointer[weftcfg.Runtime]
}

func (s *Server) runtimeCfg() *weftcfg.Runtime {
	if s.CfgPtr == nil {
		return weftcfg.Defaults()
	}
	return s.CfgPtr.Load()
}

func (s *Server) Query(req *pb.CoordinatorQueryRequest, stream pb.WeftCoordinator_QueryServer) error {
	customer, err := callerCustomer(stream.Context())
	if err != nil {
		return err
	}

	tr := coordinator.TimeRange{
		Start: time.Unix(0, req.TimeStartNs),
		End:   time.Unix(0, req.TimeEndNs),
	}
	if req.TimeStartNs == 0 && req.TimeEndNs == 0 {
		tr.Start = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		tr.End = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	result, err := s.Coord.Query(req.Pipeline, tr, customer, req.UserId)
	if err != nil {
		log.Printf("query rpc: error for customer=%s pipeline=%q: %v", customer, req.Pipeline, err)
		return err
	}

	notices := convertNotices(result.Notices)
	if result.Text != "" || result.Stderr != "" {
		text := result.Text
		if result.Stderr != "" {
			// Render stderr in red ANSI so the shell displays it appropriately.
			text += "\033[38;5;1m" + result.Stderr + "\033[0m"
		}
		return stream.Send(&pb.CoordinatorQueryResponse{Text: text, Notices: notices})
	}

	// Stream rows in batches to stay under gRPC message size limit.
	// Notices ride along on the first batch so the client surfaces them
	// before any rows render; subsequent batches don't repeat them.
	const batchSize = 500
	for i := 0; i < len(result.Rows); i += batchSize {
		end := i + batchSize
		if end > len(result.Rows) {
			end = len(result.Rows)
		}
		resp := &pb.CoordinatorQueryResponse{
			Rows: result.Rows[i:end],
		}
		if i == 0 {
			resp.Notices = notices
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	// Empty result + notices: nothing above sent anything, but we
	// still want the user to see the warning. Emit a notices-only
	// response.
	if len(result.Rows) == 0 && result.Text == "" && result.Stderr == "" && len(notices) > 0 {
		if err := stream.Send(&pb.CoordinatorQueryResponse{Notices: notices}); err != nil {
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
	// LiveFilter re-runs the user's post-source pipeline stages against
	// each broker row so the live tail and the historical batch stay
	// consistent — without it, `tail -f log | cut -d' ' -f3` shows the
	// projection on the initial DB read and raw whole lines on every
	// row that arrives afterward.
	filter := coordinator.BuildLiveFilter(plan)

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
			lines := filter(row)
			if len(lines) == 0 {
				continue
			}
			rows := make([]*pb.ResultRow, len(lines))
			for i, ln := range lines {
				rows[i] = &pb.ResultRow{LineNum: row.LineNum, Raw: ln}
			}
			if err := stream.Send(&pb.CoordinatorQueryResponse{Rows: rows}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) ListApps(ctx context.Context, _ *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	customer, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}

	// Return app names observed from ingest for this customer.
	if s.Coord.SeenApps != nil && customer != "" {
		if names := s.Coord.SeenApps.Names(customer); len(names) > 0 {
			return &pb.ListAppsResponse{Apps: names}, nil
		}
	}

	// Fall back to DB namespaces for this customer.
	if customer != "" {
		cust, err := s.Store.GetCustomerByName(customer)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "resolve customer %q: %v", customer, err)
		}
		nss, err := s.Store.ListNamespaces(cust.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list namespaces: %v", err)
		}
		names := make([]string, len(nss))
		for i, ns := range nss {
			names[i] = ns.Name
		}
		return &pb.ListAppsResponse{Apps: names}, nil
	}
	names, err := s.Store.AllAppNames()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list apps: %v", err)
	}
	return &pb.ListAppsResponse{Apps: names}, nil
}

// SampleCloudWatchSource pulls up to 50 recent events from a CloudWatch log
// group using the customer's cross-account role, runs ingest.Detect on the
// messages, and returns both so the dashboard can preview the source and
// suggest a schema before the user commits to AddSource.
func (s *Server) SampleCloudWatchSource(ctx context.Context, req *pb.SampleCloudWatchSourceRequest) (*pb.SampleCloudWatchSourceResponse, error) {
	callerCust, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}
	if req.LogGroup == "" || req.RoleArn == "" {
		return nil, status.Errorf(codes.InvalidArgument, "log_group and role_arn required")
	}
	if req.Namespace == "" {
		return nil, status.Errorf(codes.InvalidArgument, "namespace required")
	}
	customer, err := s.Store.GetCustomerByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}
	if _, err := s.Store.GetNamespace(customer.ID, req.Namespace); err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found", req.Namespace)
	}

	result, err := ingest.FetchCloudWatch(ctx, ingest.CloudWatchFetchOpts{
		Region:        req.Region,
		RoleARN:       req.RoleArn,
		ExternalID:    req.ExternalId,
		LogGroup:      req.LogGroup,
		Limit:         50,
		SessionSuffix: "sampler-" + req.Namespace,
	})
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "cloudwatch fetch: %v", err)
	}

	lines := make([]string, len(result.Events))
	for i, e := range result.Events {
		lines[i] = e.Message
	}

	detection := ingest.Detect(lines)
	detectionJSON, err := json.Marshal(detection)
	if err != nil {
		return nil, fmt.Errorf("marshal detection: %w", err)
	}

	return &pb.SampleCloudWatchSourceResponse{
		Lines:         lines,
		DetectionJson: string(detectionJSON),
	}, nil
}

func (s *Server) AddSource(ctx context.Context, req *pb.AddSourceRequest) (*pb.AddSourceResponse, error) {
	if req.Namespace == "" {
		return nil, status.Errorf(codes.InvalidArgument, "namespace required")
	}

	callerCust, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := s.Store.GetCustomerByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}

	ns, err := s.Store.GetNamespace(customer.ID, req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found — create it first", req.Namespace)
	}

	account, err := s.Store.FindOrCreateAWSAccount(customer.ID, req.AwsAccountId, req.RoleArn, req.ExternalId)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}

	pollInterval := req.PollInterval
	if pollInterval == "" {
		pollInterval = "30s"
	}
	if err := s.Store.AddLogGroup(
		ns.ID, &account.ID, store.SourceTypeCloudWatch,
		req.App, req.LogGroup, req.Region, req.SchemaYaml, pollInterval,
	); err != nil {
		return nil, fmt.Errorf("log group: %w", err)
	}

	// Attach the new app to its bucket and fan out to INGESTING
	// shards so queries for this app route correctly from the next
	// row onward. Best-effort: a failure here leaves the log_group
	// persisted but the bucket un-updated — the ingest path's
	// SeenApps trigger (below) will retry on first row. Don't fail
	// the client's AddSource on bucket trouble.
	if s.Coord.Provisioner != nil {
		if err := s.Coord.Provisioner.OnboardApp(req.App); err != nil {
			log.Printf("AddSource: OnboardApp %q: %v", req.App, err)
		}
	}

	return &pb.AddSourceResponse{}, nil
}

func (s *Server) ListSources(_ context.Context, _ *pb.ListSourcesRequest) (*pb.ListSourcesResponse, error) {
	logGroups, err := s.Store.AllLogGroups()
	if err != nil {
		return nil, err
	}
	var infos []*pb.SourceInfo
	for _, lg := range logGroups {
		infos = append(infos, &pb.SourceInfo{
			App:        lg.App,
			Customer:   lg.CustomerName,
			Namespace:  lg.NamespaceName,
			LogGroup:   lg.LogGroupName,
			Region:     lg.Region,
			Enabled:    lg.Enabled,
			LastPollAt: lg.LastPollAt,
		})
	}
	return &pb.ListSourcesResponse{Sources: infos}, nil
}

func (s *Server) RemoveSource(ctx context.Context, req *pb.RemoveSourceRequest) (*pb.RemoveSourceResponse, error) {
	if req.Namespace == "" {
		return nil, status.Errorf(codes.InvalidArgument, "namespace required")
	}
	callerCust, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := s.Store.GetCustomerByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}
	ns, err := s.Store.GetNamespace(customer.ID, req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found", req.Namespace)
	}
	if err := s.Store.RemoveLogGroup(ns.ID, req.App); err != nil {
		return nil, err
	}
	return &pb.RemoveSourceResponse{}, nil
}

// Authenticate validates an agent token. Agents stream through the
// coordinator via IngestStream — they never connect to shards directly.
func (s *Server) Authenticate(_ context.Context, req *pb.AuthenticateRequest) (*pb.AuthenticateResponse, error) {
	auth, err := s.Store.ValidateAgentToken(req.Token)
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	if auth == nil {
		return nil, fmt.Errorf("invalid or revoked token")
	}

	// Check for an active subscription.
	sub, err := s.Store.ActiveSubscription(auth.Customer.Name)
	if err != nil || sub == nil {
		log.Printf("auth: customer=%s denied (no active subscription)", auth.Customer.Name)
		return nil, status.Errorf(codes.PermissionDenied,
			"no active subscription — upgrade at w3ft.net/dashboard")
	}

	log.Printf("auth: customer=%s namespace=%s tier=%s", auth.Customer.Name, auth.Namespace, sub.Tier)

	// If the agent reported a metrics config, persist it.
	if mc := req.MetricsConfig; mc != nil {
		if err := s.Store.UpdateAgentMetricsConfig(req.Token, mc.Enabled, mc.Interval, mc.Collectors); err != nil {
			log.Printf("auth: failed to save metrics config for %s: %v", auth.Customer.Name, err)
		}
	}

	return &pb.AuthenticateResponse{
		Customer:  auth.Customer.Name,
		Namespace: auth.Namespace,
	}, nil
}

func (s *Server) CreateToken(ctx context.Context, req *pb.CreateTokenRequest) (*pb.CreateTokenResponse, error) {
	callerCust, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := s.Store.GetCustomerByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}
	ns, err := s.Store.GetNamespace(customer.ID, req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found — create it first", req.Namespace)
	}
	raw, _, err := s.Store.CreateAgentToken(customer.ID, ns.ID, req.Label, req.Env)
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	return &pb.CreateTokenResponse{Token: raw}, nil
}

// IngestStream accepts a stream of LogRows from weftd agents and forwards
// them to the active shard in batches. Flushes every 2 seconds or 100 rows.
//
// The stream requires an `authorization: Bearer <token>` gRPC metadata
// header. The token's customer is authoritative — row.Customer is
// overwritten with it regardless of what the client sends. This prevents
// a weftd whose customer has been deleted from continuing to push rows
// against that customer, and prevents a compromised agent from claiming
// to be someone else.
func (s *Server) IngestStream(stream pb.WeftCoordinator_IngestStreamServer) error {
	auth, err := s.authenticateStream(stream.Context())
	if err != nil {
		return err
	}
	authCustomer := auth.Customer.Name
	authNamespace := auth.Namespace
	authEnv := auth.Env

	// Per-stream byte budget — caps a hostile agent's ability to OOM
	// the coordinator by holding a stream open and firing rows forever.
	// Read once at accept time; an operator tightening the budget mid-
	// stream doesn't retroactively evict an in-flight stream, but every
	// agent reconnect picks up the new value.
	budget := s.runtimeCfg().IngestStreamBudgetBytes
	var bytesThisStream int64

	var mu sync.Mutex
	var rows []*pb.LogRow
	var count int64
	var firstApp string

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

		if budget > 0 {
			bytesThisStream += int64(proto.Size(row))
			if bytesThisStream > budget {
				log.Printf("ingest-stream: customer=%s exceeded %d-byte budget, closing stream (agent should reconnect)",
					authCustomer, budget)
				return status.Errorf(codes.ResourceExhausted,
					"ingest stream budget of %d bytes exceeded; reconnect", budget)
			}
		}

		// Force the authoritative customer on every row.
		row.Customer = authCustomer

		// Token-derived env: stamp it on every row. The agent has no
		// say in the env value — it's bound to the token at mint time.
		// See docs/plans/ingest-dimensions.md.
		if authEnv != "" {
			row.Env = authEnv
		}

		// Enforce the token's namespace scope on every row. row.App must
		// either equal the token's namespace or sit beneath it
		// ("<ns>/<subapp>"). Without this an agent with a token for
		// namespace A can label rows with namespace B of the same customer
		// and contaminate B's queries.
		if row.App != authNamespace && !strings.HasPrefix(row.App, authNamespace+"/") {
			return status.Errorf(codes.PermissionDenied,
				"token scoped to namespace %q; cannot ingest app %q", authNamespace, row.App)
		}

		mu.Lock()
		if count == 0 {
			firstApp = row.App
			log.Printf("ingest-stream: first row from customer=%s app=%s", row.Customer, row.App)
		}
		if s.Coord.SeenApps != nil && row.App != "" {
			if s.Coord.SeenApps.Add(row.Customer, row.App, row.Env) {
				log.Printf("ingest-stream: new app discovered: customer=%s app=%s", row.Customer, row.App)
				// Onboard to the default bucket + propagate to
				// INGESTING shards so query routing picks up the new
				// app. Idempotent; SeenApps.Add only returns true the
				// first time, so this fires once per (customer, app)
				// pair over the coordinator's lifetime.
				if s.Coord.Provisioner != nil {
					if err := s.Coord.Provisioner.OnboardApp(row.App); err != nil {
						log.Printf("ingest-stream: OnboardApp %q: %v", row.App, err)
					}
				}
			}
		}
		rows = append(rows, row)
		count++
		if len(rows) >= 100 {
			s.ingestBatch(rows)
			rows = rows[:0]
		}
		mu.Unlock()
	}

	_ = firstApp

	mu.Lock()
	if len(rows) > 0 {
		s.ingestBatch(rows)
	}
	mu.Unlock()

	log.Printf("ingest-stream: received %d rows from customer=%s", count, authCustomer)
	return stream.SendAndClose(&pb.IngestStreamResponse{RowsReceived: count})
}

// authenticateStream validates the bearer token in incoming gRPC metadata
// and returns the resolved customer/namespace. Used by streaming RPCs
// where the token isn't part of the request body.
func (s *Server) authenticateStream(ctx context.Context) (*store.AgentAuthResult, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "missing authorization metadata")
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" {
		return nil, status.Errorf(codes.Unauthenticated, "empty bearer token")
	}
	auth, err := s.Store.ValidateAgentToken(token)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "validate token: %v", err)
	}
	// auth is nil when the token doesn't exist (e.g. after customer
	// deletion cascaded the token away). Customer.ID == 0 guards against
	// any lingering orphan rows from pre-cascade deletions.
	if auth == nil || auth.Customer == nil || auth.Customer.ID == 0 || auth.Customer.Name == "" {
		return nil, status.Errorf(codes.Unauthenticated, "invalid or revoked token")
	}
	return auth, nil
}

func (s *Server) ingestBatch(rows []*pb.LogRow) {
	// Write to ring buffer (non-blocking). Background drain pushes to shards.
	if s.Coord.Ingest != nil {
		s.Coord.Ingest.Write(rows)

		// Check buffer pressure.
		pct := s.Coord.Ingest.UsagePct()
		if pct > 80 && s.Coord.Registry.TotalIngesting() == 0 {
			log.Printf("CRITICAL: ring buffer at %d%% with no ingesting shards — data will be lost", pct)
		}
	} else {
		// Fallback: direct shard write (dev mode without ring buffer).
		shards := s.Coord.GetShards()
		if len(shards) == 0 {
			s.consecutiveDrops++
			if s.consecutiveDrops >= 3 {
				log.Printf("DROPPING %d rows (no shards available, %d consecutive drops)", len(rows), s.consecutiveDrops)
			}
			return
		}
		s.consecutiveDrops = 0
		for _, sh := range shards {
			if err := sh.Ingest(rows); err != nil {
				log.Printf("ingest: shard write error: %v", err)
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
func (s *Server) GetUsage(ctx context.Context, req *pb.GetUsageRequest) (*pb.GetUsageResponse, error) {
	customer, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}

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
	dbBytes, err = s.Store.QueryUsage(customer, periodStart, periodEnd)
	if err != nil {
		log.Printf("GetUsage DB query error: %v", err)
		// Continue with in-memory only.
	}

	// Add the current (not-yet-flushed) hour.
	var memBytes int64
	if s.Coord.Usage != nil {
		memBytes = s.Coord.Usage.CurrentHour(customer)
	}

	return &pb.GetUsageResponse{
		BytesIngested: dbBytes + memBytes,
		PeriodStart:   periodStart.Format(time.RFC3339),
		PeriodEnd:     periodEnd.Format(time.RFC3339),
	}, nil
}

// ── Shell functions (/bin/) ─────────────────────────────────

// resolveCallerCustomerID returns the DB id of the customer the
// authenticated caller belongs to. Ignores any customer field on the
// request — the verified principal from the JWT interceptor is the sole
// source of truth for identity.
func (s *Server) resolveCallerCustomerID(ctx context.Context) (uint, error) {
	name, err := callerCustomer(ctx)
	if err != nil {
		return 0, err
	}
	c, err := s.Store.GetCustomerByName(name)
	if err != nil {
		return 0, status.Errorf(codes.NotFound, "customer %q not found", name)
	}
	return c.ID, nil
}

func (s *Server) SaveFunction(ctx context.Context, req *pb.SaveFunctionRequest) (*pb.SaveFunctionResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SaveShellFunction(custID, callerUserID(ctx), req.Name, req.Source); err != nil {
		return nil, err
	}
	return &pb.SaveFunctionResponse{}, nil
}

func (s *Server) GetFunction(ctx context.Context, req *pb.GetFunctionRequest) (*pb.GetFunctionResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
	if err != nil {
		return nil, err
	}
	fn, err := s.Store.GetShellFunction(custID, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%s: not found", req.Name)
	}
	return &pb.GetFunctionResponse{Name: fn.Name, Source: fn.Source}, nil
}

func (s *Server) ListFunctions(ctx context.Context, req *pb.ListFunctionsRequest) (*pb.ListFunctionsResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
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

func (s *Server) DeleteFunction(ctx context.Context, req *pb.DeleteFunctionRequest) (*pb.DeleteFunctionResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteShellFunction(custID, req.Name); err != nil {
		return nil, err
	}
	return &pb.DeleteFunctionResponse{}, nil
}

// ── Cron watches (/cron.d/) ─────────────────────────────────

func (s *Server) SaveWatch(ctx context.Context, req *pb.SaveWatchRequest) (*pb.SaveWatchResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SaveCronWatch(custID, callerUserID(ctx), req.Name, req.Schedule, req.Pipeline); err != nil {
		return nil, err
	}
	return &pb.SaveWatchResponse{}, nil
}

func (s *Server) GetWatch(ctx context.Context, req *pb.GetWatchRequest) (*pb.GetWatchResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
	if err != nil {
		return nil, err
	}
	w, err := s.Store.GetCronWatch(custID, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "%s: not found", req.Name)
	}
	return &pb.GetWatchResponse{
		Name:     w.Name,
		Schedule: w.Schedule,
		Pipeline: w.Pipeline,
		Enabled:  w.Enabled,
	}, nil
}

func (s *Server) ListWatches(ctx context.Context, req *pb.ListWatchesRequest) (*pb.ListWatchesResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
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

func (s *Server) GetWatchResults(ctx context.Context, req *pb.GetWatchResultsRequest) (*pb.GetWatchResultsResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
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
	results, err := s.Store.ListCronResults(w.ID, limit, "")
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

func (s *Server) DeleteWatch(ctx context.Context, req *pb.DeleteWatchRequest) (*pb.DeleteWatchResponse, error) {
	custID, err := s.resolveCallerCustomerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteCronWatch(custID, req.Name); err != nil {
		return nil, err
	}
	return &pb.DeleteWatchResponse{}, nil
}

// Heartbeat receives a shard's periodic status report. Persists the
// heartbeat for the provisioner's tick-based reconciliation pass,
// and also drives christen / rebind / adopt directly so state
// transitions don't wait up to PollInterval for the next tick.
//
// Empty ShardId heartbeats are the christen bootstrap: the shard has
// no identity yet, so it cannot yet hold a heartbeat token. These pass
// through without auth so the provisioner can mint an id + token and
// hand them back via Configure. For known shards, the heartbeat's
// authorization metadata must carry the per-shard token the coordinator
// minted at christen, validated here in constant time.
func (s *Server) Heartbeat(ctx context.Context, req *pb.ShardHeartbeatRequest) (*pb.HeartbeatResponse, error) {
	if req.ShardId != "" {
		if err := s.authenticateHeartbeat(ctx, req.ShardId); err != nil {
			return nil, err
		}
	}

	hb := store.ShardHeartbeat{
		ShardID:    req.ShardId,
		Addr:       req.Addr,
		InstanceID: req.InstanceId,
		State:      req.State,
		RowCount:   req.RowCount,
		DiskBytes:  req.DiskBytes,
	}
	// Persistence is opt-in: a Store-less Server still validates and
	// dispatches, it just doesn't keep a row.
	if s.Store != nil {
		if err := s.Store.InsertHeartbeat(&hb); err != nil {
			log.Printf("heartbeat: failed to persist for %s: %v", req.ShardId, err)
		}
	}
	// Runs in a goroutine so the RPC returns within the shard's 5s
	// timeout even when christen has to CreateVolume + Configure
	// over gRPC.
	if s.Coord.Provisioner != nil {
		go s.Coord.Provisioner.ProcessHeartbeat(hb)
	}
	return &pb.HeartbeatResponse{}, nil
}

// authenticateHeartbeat validates the bearer token attached to an
// incoming Heartbeat for a known shard. Returns Unauthenticated on
// any miss: missing bearer, mismatched bearer, unknown shard id, or
// shard whose persisted HeartbeatToken is empty.
//
// Lookup order is registry → store: a coord restart leaves the
// in-memory registry empty for live shards (LoadFromStore filters to
// ARCHIVED), so the first heartbeat from a returning shard has to
// validate against the persisted ShardRecord.
func (s *Server) authenticateHeartbeat(ctx context.Context, shardID string) error {
	expectedToken := ""
	if entry := s.Coord.Registry.Get(shardID); entry != nil {
		expectedToken = entry.HeartbeatToken
	} else if s.Store != nil {
		rec, ok, err := s.Store.GetShard(shardID)
		if err != nil {
			return status.Errorf(codes.Internal, "lookup shard %s: %v", shardID, err)
		}
		if ok {
			expectedToken = rec.HeartbeatToken
		}
	}
	if expectedToken == "" {
		return status.Errorf(codes.Unauthenticated, "unknown or untokened shard %s", shardID)
	}

	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Errorf(codes.Unauthenticated, "heartbeat missing authorization bearer for %s", shardID)
	}
	presented := strings.TrimPrefix(values[0], "Bearer ")
	if !store.ConstantTimeTokenEqual(expectedToken, presented) {
		return status.Errorf(codes.Unauthenticated, "heartbeat bearer mismatch for %s", shardID)
	}
	return nil
}

// ── Source onboarding ──────────────────────────────────────

// authenticateToken is a helper that validates an agent token and returns auth info.
func (s *Server) authenticateToken(token string) (*store.AgentAuthResult, error) {
	auth, err := s.Store.ValidateAgentToken(token)
	if err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	if auth == nil {
		return nil, fmt.Errorf("invalid or revoked token")
	}
	return auth, nil
}

// SubmitSample receives sample log lines from weftd for a new source.
func (s *Server) SubmitSample(_ context.Context, req *pb.SubmitSampleRequest) (*pb.SubmitSampleResponse, error) {
	auth, err := s.authenticateToken(req.Token)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}

	if req.App == "" || len(req.Lines) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "app and lines required")
	}

	detection := ingest.Detect(req.Lines)
	sampleJSON, _ := json.Marshal(req.Lines)
	detectionJSON, _ := json.Marshal(detection)

	ns, err := s.Store.GetNamespace(auth.Customer.ID, auth.Namespace)
	if err != nil {
		return nil, fmt.Errorf("namespace: %w", err)
	}
	fullApp := auth.Namespace + "/" + req.App
	if err := s.Store.UpsertPendingLogGroup(
		ns.ID, fullApp,
		req.Instance, req.Deployment,
		string(sampleJSON), string(detectionJSON),
	); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	log.Printf("sample: customer=%s namespace=%s app=%s lines=%d", auth.Customer.Name, auth.Namespace, req.App, len(req.Lines))
	return &pb.SubmitSampleResponse{Status: "pending"}, nil
}

// GetSourceStatus returns the configuration status of a source.
func (s *Server) GetSourceStatus(_ context.Context, req *pb.GetSourceStatusRequest) (*pb.GetSourceStatusResponse, error) {
	auth, err := s.authenticateToken(req.Token)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}

	if req.App == "" {
		return nil, status.Errorf(codes.InvalidArgument, "app required")
	}

	// Check if fully configured.
	configured, schemaYAML, err := s.Store.SourceIsConfigured(auth.Customer.Name, auth.Namespace, req.App)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if configured {
		sum := sha256.Sum256([]byte(schemaYAML))
		return &pb.GetSourceStatusResponse{
			Status:     "configured",
			SchemaYaml: schemaYAML,
			SchemaHash: hex.EncodeToString(sum[:]),
		}, nil
	}

	// Check if pending.
	ns, err := s.Store.GetNamespace(auth.Customer.ID, auth.Namespace)
	if err != nil {
		return nil, fmt.Errorf("namespace: %w", err)
	}
	fullApp := auth.Namespace + "/" + req.App
	pending, err := s.Store.GetPendingLogGroup(ns.ID, fullApp)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if pending != nil {
		return &pb.GetSourceStatusResponse{Status: "pending"}, nil
	}

	return &pb.GetSourceStatusResponse{Status: "unknown"}, nil
}

