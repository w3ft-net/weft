package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/auth"
	weftcfg "github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/mounts"
	"github.com/graybeardcorp/weft/notices"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// convertNotices maps coordinator-side Notice values to their
// protobuf wire shape. The two types carry the same fields; the
// indirection exists so coordinator/notices.go doesn't need to
// import the proto package.
func convertNotices(in []notices.Notice) []*pb.Notice {
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

// callerCustomer returns the home customer name of the authenticated
// caller, derived from the Session the auth interceptor materialized.
// Any non-empty `Customer` field on the request proto is IGNORED —
// clients don't get to declare which tenant they are; the verified
// principal is authoritative. Returns an Unauthenticated error when
// called without an interceptor-populated context (shouldn't happen in
// production because the interceptor gates every JWT-protected RPC).
func callerCustomer(ctx context.Context) (string, error) {
	s := auth.SessionFromContext(ctx)
	if s == nil {
		return "", status.Errorf(codes.Unauthenticated, "no authenticated caller")
	}
	if s.ActiveCustomerName == "" {
		return "", status.Errorf(codes.PermissionDenied, "authenticated user has no customer")
	}
	return s.ActiveCustomerName, nil
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

// Server implements both the WeftCoordinator (external, JWT-auth) and
// WeftAdmin (localhost-only, shared-token-auth) gRPC services. The same Go
// struct backs both because Go satisfies interfaces structurally; gRPC
// routes calls by service descriptor, so registering Server on each server
// only exposes that service's methods on that listener.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	pb.UnimplementedWeftAdminServer
	Coord *coordinator.Coordinator
	Store *store.Store

	// CfgPtr holds the live runtime config. Today only IngestStream
	// consults it (for the per-stream byte budget); nil is fine in
	// tests and non-production callers — handlers fall through to
	// compile-time defaults via runtimeCfg().
	CfgPtr *atomic.Pointer[weftcfg.Runtime]

	// ingestLimiterOnce/ingestLimiterPtr back tokenLimiter(): lazily
	// constructed so callers building Server{} literals (every test in
	// this package) don't need a constructor, and shared across every
	// IngestStream call on this process so a token's rate-limit bucket
	// persists across reconnects — see docs/ingestion-syslog.md, "Rate limiting".
	ingestLimiterOnce sync.Once
	ingestLimiterPtr  *ingestLimiter

	// Classifier drives LLM-based schema-field classification for
	// pending sources (SubmitSample — see ingest/llmdetect.go). Nil is
	// fine (every test in this package builds Server{} literals without
	// one): ClassifyAsync falls back to deterministic envelope/JSON
	// detection only, same as before this existed.
	Classifier ingest.Classifier
}

func (s *Server) tokenLimiter() *ingestLimiter {
	s.ingestLimiterOnce.Do(func() { s.ingestLimiterPtr = newIngestLimiter() })
	return s.ingestLimiterPtr
}

func (s *Server) runtimeCfg() *weftcfg.Runtime {
	if s.CfgPtr == nil {
		return weftcfg.Defaults()
	}
	return s.CfgPtr.Load()
}

func (s *Server) Execute(req *pb.CoordinatorExecuteRequest, stream pb.WeftCoordinator_ExecuteServer) error {
	start := time.Now()
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

	// Pipeline can be any user-typed shape — log query, plain bash,
	// /bin script, or a mix. Execute is the dispatch layer that
	// classifies and routes; ExecuteFollow is the variant that splices
	// a live broker subscription onto the end of the historical batch.
	ctx := stream.Context()
	// Dirstack courier (in): the delegated client ferries its cached
	// cwd/pushd stack as repeated `weft-dirstack` metadata (index 0 ==
	// cwd) so the server resolves cd/relative paths against the client's
	// live directory. Absent on the follow path and for non-shell callers
	// — ExecuteWithDir treats an empty stack exactly like Execute.
	var dirStack []string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		dirStack = md.Get("weft-dirstack")
	}
	// Trace courier (in): a delegated caller (weft's own api server)
	// ferries the ambient trace ID as `weft-trace-id` metadata, mirroring
	// the dirstack courier above. context.Value doesn't cross a gRPC call
	// on its own — this re-establishes it as an ambient value on ctx so
	// everything downstream (which now threads ctx) sees it via
	// weftsdk.TraceIDFromContext.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if ids := md.Get("weft-trace-id"); len(ids) > 0 {
			ctx = weftsdk.ContextWithTraceID(ctx, ids[0])
		}
		// Same courier, for the span api's AccessLog started (if any) —
		// read before StartSpan below so this RPC's own span parents
		// off it instead of starting a fresh root.
		if ids := md.Get("weft-span-id"); len(ids) > 0 {
			ctx = weftsdk.ContextWithSpanID(ctx, ids[0])
		}
	}
	// "coordinator.Execute" bookends this RPC with weftsdk's own "span
	// start"/"span done" lines when traced (no-op otherwise), parented
	// off whatever span api's own AccessLog started — the ctx it
	// returns carries that parenting forward into ExecuteFollow/
	// ExecuteWithDir, so compute's own span (compute/service/server.go)
	// parents off this one in turn. See api/compute/shard for the
	// matching span at each of those layers.
	//
	// defer span.End() alone, unconditionally, right here — no
	// end-of-function branching on err. RecordError is called at each
	// point below an error is actually discovered, which is what marks
	// the eventual "span done" line's status, not whatever this
	// function's own return value happens to be by the time it exits.
	ctx, span := weftsdk.StartSpan(ctx, "coordinator.Execute")
	defer span.End()
	traceID, traced := weftsdk.TraceIDFromContext(ctx)
	var lineCount int
	var result *coordinator.ExecuteResult
	if req.Follow && s.Coord.Tail != nil {
		result, err = s.Coord.ExecuteFollow(ctx, req.Pipeline, tr, customer, req.UserId, dirStack)
	} else {
		result, err = s.Coord.ExecuteWithDir(ctx, req.Pipeline, tr, customer, req.UserId, dirStack)
	}
	if err != nil {
		span.RecordError(err)
		if traced {
			log.Printf("query rpc: error for customer=%s pipeline=%q trace_id=%s: %v", customer, req.Pipeline, traceID, err)
		} else {
			log.Printf("query rpc: error for customer=%s pipeline=%q: %v", customer, req.Pipeline, err)
		}
		return err
	}

	// Pump ExecuteResult.Stream onto the wire. Lines arrive batched
	// (500 max) to stay under the gRPC message-size limit; notices
	// ride on the FINAL batch and stderr also on the last. With the
	// streaming-runLocal change, result.Notices is populated by the
	// runLocal goroutine AFTER warp finishes (right before the pipe
	// writers close). The consumer sees its terminal Next return
	// ok=false BEFORE the writers close has propagated, but the
	// happens-before relationship (Notices write → pipe close →
	// scanner EOF → channel close → consumer Next) guarantees the
	// read past EOF sees the populated value.
	//
	// Follow mode collapses into the same loop: ExecuteFollow returns
	// a stream that splices historical → broker, so Next exits only
	// when ctx cancels (client disconnect) or, rarely, the broker
	// channel closes. Notices from follow's historical phase are not
	// surfaced today (no /tmp writes happen during a read-only
	// historical batch).
	//
	// Once the historical phase gives way to live broker rows, batching
	// up to batchSize would sit on a single live row indefinitely — a
	// follow session rarely produces 500 rows in a reasonable window and
	// the stream itself never naturally ends. inLivePhase (nil for a
	// non-follow/non-concat stream) lets that phase flush every row
	// immediately instead, while the historical phase — however large,
	// e.g. an explicit `tail -n <big> -f` — still batches normally.
	const batchSize = 500
	var batch []*pb.Line
	var stderrAcc strings.Builder
	firstBatchSent := false

	var inLivePhase func() bool
	if ls, ok := result.Stream.(coordinator.LiveStream); ok {
		inLivePhase = ls.InLivePhase
	}

	flush := func(final bool, notices []*pb.Notice) error {
		if !final && len(batch) == 0 {
			return nil
		}
		if final && len(batch) == 0 && stderrAcc.Len() == 0 && len(notices) == 0 && firstBatchSent {
			return nil
		}
		resp := &pb.CoordinatorExecuteResponse{Lines: batch}
		if final {
			resp.Notices = notices
			resp.Stderr = stderrAcc.String()
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
		batch = nil
		firstBatchSent = true
		return nil
	}

	for {
		line, ok, err := result.Stream.Next(ctx)
		if err != nil {
			// Client disconnect / ctx cancel surfaces here in follow
			// mode; treat it as a clean stream end so the rpc returns
			// nil instead of context.Canceled bubbling to the wire —
			// not a real error, so no RecordError.
			if ctx.Err() != nil {
				return nil
			}
			span.RecordError(err)
			return err
		}
		if !ok {
			break
		}
		if line.Stderr {
			stderrAcc.WriteString(line.Raw)
			stderrAcc.WriteByte('\n')
			continue
		}
		batch = append(batch, &pb.Line{Raw: line.Raw})
		lineCount++
		if len(batch) >= batchSize || (inLivePhase != nil && inLivePhase()) {
			if err := flush(false, nil); err != nil {
				span.RecordError(err)
				return err
			}
		}
	}
	// Stream done — Notices and FinalDirStack are now populated by the
	// runLocal goroutine (same happens-before edge as the stream EOF we
	// just observed). Dirstack courier (out): return the post-run stack
	// as a trailer so the client can cache the cwd the pipeline left
	// behind. Echo the seeded stack when the pipeline left it untouched
	// so every response carries cwd (the cache must never drift).
	finalDir := result.FinalDirStack
	if len(finalDir) == 0 {
		finalDir = dirStack
	}
	if len(finalDir) > 0 {
		md := metadata.MD{}
		for _, d := range finalDir {
			md.Append("weft-final-dirstack", d)
		}
		stream.SetTrailer(md)
	}
	err = flush(true, convertNotices(result.Notices))
	if err != nil {
		span.RecordError(err)
		return err
	}
	if traced {
		log.Printf("query rpc: done customer=%s pipeline=%q lines=%d duration=%s trace_id=%s", customer, req.Pipeline, lineCount, time.Since(start), traceID)
	}
	return nil
}

func (s *Server) ListServices(ctx context.Context, _ *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	customer, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}

	// Return app names observed from ingest for this customer.
	if s.Coord.SeenServices != nil && customer != "" {
		if names := s.Coord.SeenServices.Names(customer); len(names) > 0 {
			return &pb.ListServicesResponse{Services: names}, nil
		}
	}

	// Fall back to DB namespaces for this customer.
	if customer != "" {
		cust, err := s.Store.GetOrgByName(customer)
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
		return &pb.ListServicesResponse{Services: names}, nil
	}
	var names []string
	if err := s.Store.DB().WithContext(store.WithoutTenantScope(context.Background())).
		Model(&store.Namespace{}).Distinct().Pluck("name", &names).Error; err != nil {
		return nil, status.Errorf(codes.Internal, "list apps: %v", err)
	}
	return &pb.ListServicesResponse{Services: names}, nil
}

func (s *Server) AddSource(ctx context.Context, req *pb.AddSourceRequest) (*pb.AddSourceResponse, error) {
	if req.Namespace == "" {
		return nil, status.Errorf(codes.InvalidArgument, "namespace required")
	}
	if req.LogGroup == "" {
		return nil, status.Errorf(codes.InvalidArgument, "log_group required")
	}

	callerCust, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := s.Store.GetOrgByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}

	ns, err := s.Store.GetNamespace(customer.ID, req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found — create it first", req.Namespace)
	}

	// Registers the AWS-log-group→service mapping CloudWatchAdapter routes
	// against, StatusPending, finished later via `weft source configure`
	// once a sample has accumulated from the customer's real Firehose
	// deliveries (docs/plans/cloudwatch-kinesis-subscription.md).
	if err := s.Store.CreatePendingKinesisSource(ns.ID, req.Service, req.LogGroup, req.Region); err != nil {
		return nil, fmt.Errorf("log group: %w", err)
	}

	// Attach the new app to its bucket and fan out to INGESTING
	// shards so queries for this app route correctly from the next
	// row onward. Best-effort: a failure here leaves the log_group
	// persisted but the bucket un-updated — the ingest path's
	// SeenServices trigger (below) will retry on first row. Don't fail
	// the client's AddSource on bucket trouble.
	if s.Coord.Provisioner != nil {
		if err := s.Coord.Provisioner.OnboardService(ns.ID, req.Service); err != nil {
			log.Printf("AddSource: OnboardService %d/%q: %v", ns.ID, req.Service, err)
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
			Service:   lg.Service,
			Customer:  lg.CustomerName,
			Namespace: lg.NamespaceName,
			LogGroup:  lg.LogGroupName,
			Region:    lg.Region,
			Enabled:   lg.Enabled,
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
	customer, err := s.Store.GetOrgByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}
	ns, err := s.Store.GetNamespace(customer.ID, req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found", req.Namespace)
	}
	if err := s.Store.DB().Unscoped().
		Where("namespace_id = ? AND app = ?", ns.ID, req.Service).
		Delete(&store.LogGroup{}).Error; err != nil {
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
	sub, err := s.Store.ActiveSubscription(auth.Organization.ID)
	if err != nil || sub == nil {
		log.Printf("auth: customer=%s denied (no active subscription)", auth.Organization.Name)
		return nil, status.Errorf(codes.PermissionDenied,
			"no active subscription — upgrade at w3ft.net/dashboard")
	}

	log.Printf("auth: customer=%s namespace=%s tier=%s", auth.Organization.Name, auth.Namespace, sub.Tier)

	// If the agent reported a metrics config, persist it.
	if mc := req.MetricsConfig; mc != nil {
		if err := s.Store.UpdateAgentMetricsConfig(req.Token, mc.Enabled, mc.Interval, mc.Collectors); err != nil {
			log.Printf("auth: failed to save metrics config for %s: %v", auth.Organization.Name, err)
		}
	}

	return &pb.AuthenticateResponse{
		Customer:  auth.Organization.Name,
		Namespace: auth.Namespace,
	}, nil
}

func (s *Server) CreateToken(ctx context.Context, req *pb.CreateTokenRequest) (*pb.CreateTokenResponse, error) {
	callerCust, err := callerCustomer(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := s.Store.GetOrgByName(callerCust)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "customer %q not found", callerCust)
	}
	ns, err := s.Store.GetNamespace(customer.ID, req.Namespace)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "namespace %q not found — create it first", req.Namespace)
	}
	raw, _, err := s.Store.CreateAgentToken(customer.ID, ns.ID, req.Label, req.Env, "")
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	return &pb.CreateTokenResponse{Token: raw}, nil
}

// IngestStream accepts a stream of LogRows from weftd agents and forwards
// them to the active shard in batches. Flushes every 2 seconds, at 100
// rows, or immediately when a row matches an active local tail -f
// subscriber.
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
	authCustomer := auth.Organization.Name
	authNamespace := auth.Namespace
	authEnv := auth.Env

	// Read the agent's reported client identity from gRPC metadata and
	// (a) record it on the token row for the operator dashboard and the
	// deprecation-window query, and (b) enforce the hard floor — an
	// agent below WEFTD_HARD_FLOOR is refused outright. Soft tolerance
	// for old wire shapes happens per-row inside the parse path, keyed
	// on the recorded version.
	clientID := readClientID(stream.Context())
	if clientID != "" {
		now := time.Now()
		// Ingest-path write by token id (already authenticated upstream).
		if err := s.Store.DB().WithContext(store.WithoutTenantScope(context.Background())).
			Model(&store.AgentToken{}).Where("id = ?", auth.TokenID).
			Updates(map[string]any{
				"last_seen_version": clientID,
				"last_seen_at":      &now,
			}).Error; err != nil {
			// Don't kill the stream over a version-stamp write failure —
			// the customer's logs flowing is more important than the
			// operator dashboard being current.
			log.Printf("ingest-stream: record agent version (token=%d, %q): %v", auth.TokenID, clientID, err)
		}
	}
	if floor := s.runtimeCfg().WeftdHardFloor; floor != "" {
		if clientBelow(clientID, floor) {
			return status.Errorf(codes.FailedPrecondition,
				"weftd version %q is below the hard floor %q — upgrade weftd; see https://releases.w3ft.net/weftd/latest/",
				clientID, floor)
		}
	}

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
	var firstService string

	// Periodic flush for sparse streams. The flush goroutine must exit
	// when the stream ends: ticker.Stop() halts the ticker but does NOT
	// close ticker.C, so a bare `for range ticker.C` blocks forever and
	// leaks one goroutine (plus the closure state it pins) per stream —
	// the coordinator's linear memory growth under agent reconnect churn.
	// The done channel, closed on return, releases it deterministically.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				if len(rows) > 0 {
					s.ingestBatch(rows)
					rows = rows[:0]
				}
				mu.Unlock()
			}
		}
	}()

	// Run stream.Recv() in its own goroutine so the main loop can also
	// wake on an idle deadline. An abandoned stream (client shipped a
	// batch then went silent without half-closing) would otherwise block
	// the handler in Recv() forever — pinning this goroutine and the
	// flush goroutine above, the dominant coordinator leak. The
	// done-guarded send guarantees this reader exits when the handler
	// returns (close(done) above) even if it's mid-send on an
	// unread row.
	type recvResult struct {
		row *pb.LogRow
		err error
	}
	recvCh := make(chan recvResult, 1)
	go func() {
		for {
			row, err := stream.Recv()
			select {
			case recvCh <- recvResult{row, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Idle reaper: if no row arrives within the window, close the stream.
	// Zero disables (pre-fix behavior). nil channel blocks forever, so the
	// select's idle case is inert when disabled.
	idleTimeout := time.Duration(s.runtimeCfg().IngestStreamIdleTimeoutSeconds) * time.Second
	var idleC <-chan time.Time
	var idle *time.Timer
	if idleTimeout > 0 {
		idle = time.NewTimer(idleTimeout)
		defer idle.Stop()
		idleC = idle.C
	}

recvLoop:
	for {
		var row *pb.LogRow
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-idleC:
			log.Printf("ingest-stream: customer=%s idle for %s, closing stream (agent should reconnect)",
				authCustomer, idleTimeout)
			return status.Errorf(codes.DeadlineExceeded,
				"ingest stream idle for %s; reconnect", idleTimeout)
		case r := <-recvCh:
			if r.err == io.EOF {
				break recvLoop
			}
			if r.err != nil {
				return r.err
			}
			if idle != nil {
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(idleTimeout)
			}
			row = r.row
		}

		rowSize := int64(proto.Size(row))

		if budget > 0 {
			bytesThisStream += rowSize
			if bytesThisStream > budget {
				log.Printf("ingest-stream: customer=%s exceeded %d-byte budget, closing stream (agent should reconnect)",
					authCustomer, budget)
				return status.Errorf(codes.ResourceExhausted,
					"ingest stream budget of %d bytes exceeded; reconnect", budget)
			}
		}

		// Per-token sustained rate limit (render.md Component 6). Read
		// live so an operator's config edit applies to in-flight
		// streams immediately, unlike the accept-time budget above.
		liveCfg := s.runtimeCfg()
		if err := s.tokenLimiter().wait(stream.Context(), auth.TokenID, rowSize,
			liveCfg.IngestTokenBytesPerSec, liveCfg.IngestTokenBurstBytes); err != nil {
			return err
		}

		// Force the authoritative customer on every row.
		row.Customer = authCustomer

		// Token-derived env: stamp it on every row. The agent has no
		// say in the env value — it's bound to the token at mint time.
		// See docs/plans/ingest-dimensions.md.
		if authEnv != "" {
			row.Env = authEnv
		}

		// The token's namespace is authoritative: stamp it on every row,
		// overwriting whatever the agent claimed. An agent with a token
		// for namespace A therefore cannot label rows with namespace B of
		// the same customer and contaminate B's queries — the wire no
		// longer carries the namespace as a trusted, agent-supplied field.
		row.Namespace = authNamespace
		// kind defaults to "main" (the canonical single-stream source).
		if row.Kind == "" {
			row.Kind = "main"
		}

		// svcPath is the namespace/service[/kind] index key the pseudofile
		// tree (SeenServices) and bucket onboarding use. Reassembled from
		// the now-split row fields; routing itself keys on the dedicated
		// namespace/service columns, not this string.
		svcPath := row.Namespace
		if row.Service != "" {
			svcPath = row.Namespace + "/" + row.Service
			if row.Kind != "" {
				svcPath += "/" + row.Kind
			}
		}

		mu.Lock()
		if count == 0 {
			firstService = svcPath
			log.Printf("ingest-stream: first row from customer=%s service=%s", row.Customer, svcPath)
		}
		if s.Coord.SeenServices != nil && row.Service != "" {
			if s.Coord.SeenServices.Add(row.Customer, svcPath, row.Env) {
				log.Printf("ingest-stream: new service discovered: customer=%s service=%s", row.Customer, svcPath)
				// Auto-onboard: raw producers (Heroku, custom POSTs, bare
				// IngestStream) never sample, so there's no log_group until
				// we make one here. OnboardService records the canonical
				// (namespace, service) mapping and stamps the INGESTING
				// shards so routing picks it up from the next row. Idempotent;
				// SeenServices.Add returns true only once per (customer,
				// service) over the coordinator's lifetime.
				if s.Coord.Provisioner != nil {
					if ns, err := s.Store.GetNamespace(auth.Organization.ID, auth.Namespace); err != nil {
						log.Printf("ingest-stream: onboard %q: namespace %s/%s: %v", row.Service, auth.Organization.Name, auth.Namespace, err)
					} else if err := s.Coord.Provisioner.OnboardService(ns.ID, row.Service); err != nil {
						log.Printf("ingest-stream: OnboardService %d/%q: %v", ns.ID, row.Service, err)
					}
				}
			}
		}
		rows = append(rows, row)
		count++
		// Flush early, ahead of the 100-row/2s batch, when a local tail -f
		// subscriber is interested in this row — otherwise a sparse
		// producer's rows sit buffered for up to the ticker interval
		// before a live tail session sees them (docs/known-issues.md).
		flushNow := len(rows) >= 100
		if !flushNow && s.Coord.TailFanout != nil {
			flushNow = s.Coord.TailFanout.HasSubscriber(row)
		}
		if flushNow {
			s.ingestBatch(rows)
			rows = rows[:0]
		}
		mu.Unlock()
	}

	_ = firstService

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
	if auth == nil || auth.Organization == nil || auth.Organization.ID == "" || auth.Organization.Name == "" {
		return nil, status.Errorf(codes.Unauthenticated, "invalid or revoked token")
	}
	return auth, nil
}

func (s *Server) ingestBatch(rows []*pb.LogRow) {
	// Write to ring buffer (non-blocking). Background drain pushes to
	// shards. NewCoordinator always installs a buffer, so no nil-check.
	s.Coord.Ingest.Write(rows)
	pct := s.Coord.Ingest.UsagePct()
	if pct > 80 && s.Coord.Provisioner.TotalIngesting() == 0 {
		log.Printf("CRITICAL: ring buffer at %d%% with no ingesting shards — data will be lost", pct)
	}

	// Fan out to tail -f subscribers (local, plus any other coordinators
	// with interested subscribers if Redis fan-out is configured).
	if s.Coord.TailFanout != nil {
		s.Coord.TailFanout.Publish(rows)
	}
}

// GetUsage returns bytes ingested for a customer in a billing period.
// Combines PostgreSQL (everything flushed as of the last ~30s tick,
// including partial current-hour rows now that flushUsage appends every
// tick rather than only on hour rollover) with memBytes, the delta
// accumulated on *this* instance since its own last flush. On a
// multi-coordinator fleet a customer's traffic can be split across
// instances, so the number this RPC returns can lag the true total by up
// to one flush interval — bounded staleness, not bounded correctness;
// nothing enforcement-critical reads this path (see
// docs/plans/coordinator-horizontal-scaling.md).
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

// resolveCallerOrgID returns the DB id of the customer the
// authenticated caller belongs to. Ignores any customer field on the
// request — the verified principal from the JWT interceptor is the sole
// source of truth for identity.
func (s *Server) resolveCallerOrgID(ctx context.Context) (string, error) {
	name, err := callerCustomer(ctx)
	if err != nil {
		return "", err
	}
	c, err := s.Store.GetOrgByName(name)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "customer %q not found", name)
	}
	return c.ID, nil
}

func (s *Server) SaveFunction(ctx context.Context, req *pb.SaveFunctionRequest) (*pb.SaveFunctionResponse, error) {
	custID, err := s.resolveCallerOrgID(ctx)
	if err != nil {
		return nil, err
	}
	lang, cleanSource := mounts.ClassifyBinScript(req.Name, req.Source)
	if err := s.Store.SaveShellFunction(custID, callerUserID(ctx), req.Name, cleanSource, lang); err != nil {
		return nil, err
	}
	return &pb.SaveFunctionResponse{}, nil
}

func (s *Server) GetFunction(ctx context.Context, req *pb.GetFunctionRequest) (*pb.GetFunctionResponse, error) {
	custID, err := s.resolveCallerOrgID(ctx)
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
	custID, err := s.resolveCallerOrgID(ctx)
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
	custID, err := s.resolveCallerOrgID(ctx)
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
	custID, err := s.resolveCallerOrgID(ctx)
	if err != nil {
		return nil, err
	}
	// Look up customer name for the LoweredPlan's ArgCustomer source.
	// On a save-time race where the customer was deleted between auth
	// and here, skip lowering rather than fail the save.
	var loweredJSON []byte
	if cust, err := s.Store.GetOrgByID(custID); err == nil && cust != nil {
		resolver := storeBinResolver{store: s.Store, orgID: custID}
		tier := string(s.Store.TierForOrg(custID))
		loweredJSON = planner.LowerForStorage(req.Pipeline, cust.Name, tier, resolver)
	}
	// SaveCronWatch's error may be a non-blocking store.Warning (the
	// save already happened) — see store.Split's doc. SaveWatchResponse
	// has no field to carry warning text today, so this surface drops
	// it rather than misreporting a successful save as an RPC failure;
	// the CLI/HTTP/MCP surfaces are where warnings actually reach a
	// user today.
	blocking, _ := store.Split(s.Store.SaveCronWatch(custID, callerUserID(ctx), req.Name, req.Schedule, req.Pipeline, loweredJSON))
	if len(blocking) > 0 {
		return nil, errors.Join(blocking...)
	}
	return &pb.SaveWatchResponse{}, nil
}

// storeBinResolver adapts the gRPC service's store + customer scope to
// planner.BinResolver for save-time pipeline inlining. Mirrors the
// adapter in api/handlers.go — kept duplicated rather than shared
// because the two save paths sit in different packages and the type
// is genuinely tiny.
type storeBinResolver struct {
	store *store.Store
	orgID string
}

func (r storeBinResolver) Source(name string) (string, string, bool) {
	fn, err := r.store.GetShellFunction(r.orgID, name)
	if err != nil || fn == nil {
		return "", "", false
	}
	return fn.Source, planner.HashBinSource(fn.Source), true
}

func (s *Server) GetWatch(ctx context.Context, req *pb.GetWatchRequest) (*pb.GetWatchResponse, error) {
	custID, err := s.resolveCallerOrgID(ctx)
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
	custID, err := s.resolveCallerOrgID(ctx)
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
	custID, err := s.resolveCallerOrgID(ctx)
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
	custID, err := s.resolveCallerOrgID(ctx)
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
		BinarySHA:  req.BinarySha,
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

// authenticateHeartbeat extracts the bearer from the incoming gRPC
// metadata and hands the verdict to the provisioner. The split keeps
// transport concerns (metadata, status codes) in the server and the
// registry→store token lookup behind the provisioner boundary.
func (s *Server) authenticateHeartbeat(ctx context.Context, shardID string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) == 0 {
		return status.Errorf(codes.Unauthenticated, "heartbeat missing authorization bearer for %s", shardID)
	}
	presented := strings.TrimPrefix(values[0], "Bearer ")
	if err := s.Coord.Provisioner.AuthenticateShard(shardID, presented); err != nil {
		if errors.Is(err, logdb.ErrShardAuthFailed) {
			return status.Errorf(codes.Unauthenticated, "%v", err)
		}
		return status.Errorf(codes.Internal, "%v", err)
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

// SubmitSample receives sample log lines from weftd for a source. A partial
// submit is a progress ping while the agent is still accumulating (the
// source surfaces as "sampling (N/target)"); the final non-partial submit
// carries the complete sample and makes the source configurable.
func (s *Server) SubmitSample(_ context.Context, req *pb.SubmitSampleRequest) (*pb.SubmitSampleResponse, error) {
	auth, err := s.authenticateToken(req.Token)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}

	if req.Service == "" {
		return nil, status.Errorf(codes.InvalidArgument, "app required")
	}
	if !req.Partial && len(req.Lines) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "lines required for a final sample")
	}

	ns, err := s.Store.GetNamespace(auth.Organization.ID, auth.Namespace)
	if err != nil {
		return nil, fmt.Errorf("namespace: %w", err)
	}
	// Store the bare service in log_groups.app — the namespace is already
	// carried by ns.ID. Routing keys on the bare service (the shard rows'
	// service column and the planner's WHERE service=?), so the old
	// namespace-prefixed form ("weft/api") broke service-level shard
	// routing for weftd sources. weftd sends a bare service on onboard
	// and "service/kind" on resample; take the first segment for both.
	service := req.Service
	if i := strings.IndexByte(service, '/'); i >= 0 {
		service = service[:i]
	}

	// ClassifyAsync computes envelope/JSON detection inline and, only for
	// the final (non-partial) sample, starts LLM classification in the
	// background — this RPC returns as soon as the sample is stored,
	// never blocked on Bedrock. See ingest/llmdetect.go.
	namespaceID := ns.ID
	detection := ingest.ClassifyAsync(s.Classifier, req.Lines, !req.Partial, func(d *ingest.Detection) {
		detectionJSON, err := json.Marshal(d)
		if err != nil {
			return
		}
		if err := s.Store.UpdateLogGroupDetection(namespaceID, service, string(detectionJSON)); err != nil {
			log.Printf("classify: update detection for %s/%s: %v", auth.Namespace, service, err)
		}
	})
	sampleJSON, _ := json.Marshal(req.Lines)
	detectionJSON, _ := json.Marshal(detection)

	if err := s.Store.UpsertPendingLogGroup(
		ns.ID, service,
		req.Instance, req.Deployment,
		string(sampleJSON), string(detectionJSON),
		len(req.Lines), req.Partial,
	); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	respStatus := "pending"
	if req.Partial {
		respStatus = "sampling"
	}
	log.Printf("sample: customer=%s namespace=%s app=%s lines=%d partial=%t", auth.Organization.Name, auth.Namespace, req.Service, len(req.Lines), req.Partial)
	return &pb.SubmitSampleResponse{Status: respStatus}, nil
}

// GetSourceStatus returns the configuration status of a source.
func (s *Server) GetSourceStatus(_ context.Context, req *pb.GetSourceStatusRequest) (*pb.GetSourceStatusResponse, error) {
	auth, err := s.authenticateToken(req.Token)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}

	if req.Service == "" {
		return nil, status.Errorf(codes.InvalidArgument, "app required")
	}

	lg, err := s.Store.FindLogGroup(auth.Organization.ID, auth.Namespace, req.Service)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if lg == nil {
		return &pb.GetSourceStatusResponse{Status: "unknown"}, nil
	}

	switch lg.Status {
	case store.StatusConfigured:
		sum := sha256.Sum256([]byte(lg.SchemaYAML))
		return &pb.GetSourceStatusResponse{
			Status:            "configured",
			SchemaYaml:        lg.SchemaYAML,
			SchemaHash:        hex.EncodeToString(sum[:]),
			ResampleRequested: lg.ResampleRequestedAt != nil,
		}, nil
	case store.StatusSampling:
		return &pb.GetSourceStatusResponse{
			Status:          "sampling",
			SampleCollected: int32(lg.SampleCollected),
			SampleTarget:    store.SampleTarget,
		}, nil
	case store.StatusPending:
		return &pb.GetSourceStatusResponse{
			Status:          "pending",
			SampleCollected: int32(lg.SampleCollected),
			SampleTarget:    store.SampleTarget,
		}, nil
	default:
		return &pb.GetSourceStatusResponse{Status: "unknown"}, nil
	}
}

// shardOpStatus maps a provisioner admin-op error to a gRPC status the
// API server can turn into a meaningful HTTP code (404/409) instead of a
// blanket 500.
func shardOpStatus(err error) error {
	switch {
	case errors.Is(err, logdb.ErrShardNotInRegistry):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, logdb.ErrShardWrongState), errors.Is(err, logdb.ErrShardWithinRetention), errors.Is(err, logdb.ErrShardHasResources):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

func (s *Server) ListShards(_ context.Context, req *pb.ListShardsRequest) (*pb.ListShardsResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	rows, err := (logdb.ShardStore{Store: s.Store}).ListShardsByBucket(req.BucketId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list shards: %v", err)
	}
	out := make([]*pb.ShardInfo, 0, len(rows))
	for _, sh := range rows {
		out = append(out, shardInfoFrom(sh))
	}
	return &pb.ListShardsResponse{Shards: out}, nil
}

// shardInfoFrom marshals a persisted shard row into the ShardInfo wire
// shape — shared between ListShards and GetShard.
func shardInfoFrom(sh *logdb.Shard) *pb.ShardInfo {
	info := &pb.ShardInfo{
		Id:          sh.ID,
		WindowStart: sh.WindowStart.Format(time.RFC3339Nano),
		WindowEnd:   sh.WindowEnd.Format(time.RFC3339Nano),
		State:       string(sh.State),
		Addr:        sh.Addr,
		InstanceId:  sh.InstanceID,
		ParquetPath: sh.ParquetPath,
		Tier:        sh.Tier,
		RowCount:    sh.RowCount,
		CreatedAt:   sh.CreatedAt.Format(time.RFC3339Nano),
		BucketId:    sh.BucketID,
	}
	if !sh.LastHeartbeatAt.IsZero() {
		info.LastHeartbeatAt = sh.LastHeartbeatAt.Format(time.RFC3339Nano)
	}
	info.BinarySha = sh.BinarySHA
	return info
}

func (s *Server) GetShard(_ context.Context, req *pb.GetShardRequest) (*pb.GetShardResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	rec, found, err := (logdb.ShardStore{Store: s.Store}).GetShard(req.ShardId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get shard: %v", err)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "shard %s not found", req.ShardId)
	}
	return &pb.GetShardResponse{Shard: shardInfoFrom(rec)}, nil
}

func (s *Server) ListBuckets(_ context.Context, _ *pb.ListBucketsRequest) (*pb.ListBucketsResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	rows, err := s.Store.ListBuckets()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list buckets: %v", err)
	}
	out := make([]*pb.BucketInfo, 0, len(rows))
	for _, b := range rows {
		out = append(out, &pb.BucketInfo{
			Id:                   b.ID,
			Label:                b.Label,
			TargetIngestingCount: int32(b.TargetIngestingCount),
			Tier:                 b.Tier,
			State:                b.State,
		})
	}
	return &pb.ListBucketsResponse{Buckets: out}, nil
}

func (s *Server) ForceArchiveShard(_ context.Context, req *pb.ForceArchiveShardRequest) (*pb.ForceArchiveShardResponse, error) {
	if s.Coord.Provisioner == nil {
		return nil, status.Errorf(codes.Unavailable, "provisioner not running")
	}
	if err := s.Coord.Provisioner.ForceArchive(req.ShardId); err != nil {
		return nil, shardOpStatus(err)
	}
	return &pb.ForceArchiveShardResponse{}, nil
}

func (s *Server) DeleteArchivedShard(_ context.Context, req *pb.DeleteArchivedShardRequest) (*pb.DeleteArchivedShardResponse, error) {
	if s.Coord.Provisioner == nil {
		return nil, status.Errorf(codes.Unavailable, "provisioner not running")
	}
	if err := s.Coord.Provisioner.DeleteArchived(req.ShardId, req.Force); err != nil {
		return nil, shardOpStatus(err)
	}
	return &pb.DeleteArchivedShardResponse{}, nil
}

// ForceRemoveShard is the WeftAdmin escape hatch for a shard stuck in
// ARCHIVING whose archive can never complete. See Provisioner.ForceRemove.
func (s *Server) ForceRemoveShard(_ context.Context, req *pb.ForceRemoveShardRequest) (*pb.ForceRemoveShardResponse, error) {
	if s.Coord.Provisioner == nil {
		return nil, status.Errorf(codes.Unavailable, "provisioner not running")
	}
	if err := s.Coord.Provisioner.ForceRemove(req.ShardId, req.Force); err != nil {
		return nil, shardOpStatus(err)
	}
	return &pb.ForceRemoveShardResponse{}, nil
}

func (s *Server) RelaunchShard(_ context.Context, req *pb.RelaunchShardRequest) (*pb.RelaunchShardResponse, error) {
	if s.Coord.Provisioner == nil {
		return nil, status.Errorf(codes.Unavailable, "provisioner not running")
	}
	if err := s.Coord.Provisioner.RelaunchShard(req.ShardId); err != nil {
		return nil, shardOpStatus(err)
	}
	return &pb.RelaunchShardResponse{}, nil
}
