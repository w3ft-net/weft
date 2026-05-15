package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kevlarcon/warp/pkg/bash"
	luapkg "github.com/kevlarcon/warp/pkg/lua"
	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/mounts"
	"github.com/kevlarcon/weft/shellfs"
	"github.com/kevlarcon/weft/store"
)

// TimeRange specifies the query time window.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// QueryResult is the legacy buffered shape: Rows for shard output,
// Text for compute output, Stderr for alert output, Notices for one-
// time messages. Internal-only as of phase 4 — the public surface
// (Coordinator.Execute / Query / ExecuteForWatch) now returns
// *ExecuteResult, which carries a LineStream. The internal helpers
// (c.query, executeQuery, runLocal, alert + redirect post-processing)
// still build QueryResult so the buffered code paths can stay
// unchanged; the public boundary converts to ExecuteResult on the
// way out via queryResultToExecuteResult.
type QueryResult struct {
	Text    string
	Stderr  string   // alert/error output — rendered in red by the shell
	Notices []Notice // one-time per-user notices to surface in the response
}

// ExecuteResult is the public shape returned by Coordinator.Execute,
// Query, and ExecuteForWatch. Stream emits all output lines (body
// and alert stderr) in a single forward-only sequence — the gRPC
// server pumps it onto the wire batch-by-batch.
//
// Notices is populated by the producer goroutine before pipe close;
// callers read it after the stream exhausts (Drain handles this).
type ExecuteResult struct {
	Stream  compute.LineStream
	Notices []Notice
}

// queryResultToExecuteResult wraps an internal buffered QueryResult
// in the public ExecuteResult shape. Text and stderr are split into
// lines and replayed via SliceLineStream.
func queryResultToExecuteResult(qr *QueryResult) *ExecuteResult {
	var lines []compute.Line
	if qr.Text != "" {
		for _, t := range strings.Split(strings.TrimRight(qr.Text, "\n"), "\n") {
			lines = append(lines, compute.Line{Raw: t})
		}
	}
	if qr.Stderr != "" {
		for _, t := range strings.Split(strings.TrimRight(qr.Stderr, "\n"), "\n") {
			lines = append(lines, compute.Line{Raw: t, Stderr: true})
		}
	}
	return &ExecuteResult{
		Stream:  compute.NewSliceLineStream(lines, nil),
		Notices: qr.Notices,
	}
}

// DrainedResult is the buffered shape produced by ExecuteResult.Drain.
// Tests and post-processors that don't consume the stream byte-by-byte
// drain the result here; production callers (the gRPC server) pump
// Stream directly onto the wire and skip the drain.
type DrainedResult struct {
	Rows    []*pb.ResultRow
	Text    string
	Stderr  string
	Notices []Notice
}

// Drain reads ExecuteResult.Stream to exhaustion and returns the
// buffered shape. Body lines (Stderr=false) become Rows + Text; alert
// stderr lines (Stderr=true) become Stderr.
//
// Notices are read from r.Notices AFTER the stream exhausts — the
// streaming runLocal populates Notices in its goroutine right before
// closing the pipe writers, so reading Notices at the end of Drain
// (after EOF) is when the populated value is visible. Reading at the
// start would see the goroutine's pre-execution zero value.
func (r *ExecuteResult) Drain(ctx context.Context) (*DrainedResult, error) {
	out := &DrainedResult{}
	if r.Stream == nil {
		out.Notices = r.Notices
		return out, nil
	}
	var stderrSb, textSb strings.Builder
	for {
		line, ok, err := r.Stream.Next(ctx)
		if err != nil {
			out.Notices = r.Notices
			return out, err
		}
		if !ok {
			break
		}
		if line.Stderr {
			stderrSb.WriteString(line.Raw)
			stderrSb.WriteByte('\n')
			continue
		}
		out.Rows = append(out.Rows, &pb.ResultRow{Raw: line.Raw})
		textSb.WriteString(line.Raw)
		textSb.WriteByte('\n')
	}
	out.Text = textSb.String()
	out.Stderr = stderrSb.String()
	// After stream EOF, the runLocal goroutine has populated Notices
	// (its program order: ExecScript → Notices = noticeQueue.Flush() →
	// pipes close → scanners EOF → channel close → consumer Next).
	out.Notices = r.Notices
	return out, nil
}

// Coordinator routes queries to shards and merges results. It holds no
// customer identity of its own — tenant scope is resolved per-request
// from the customer name passed to Query / ExecuteForWatch, which then
// propagates to ShellFS, AlertContext, and /bin script resolution.
//
// Built via NewCoordinator(CoordinatorConfig). Every dependency is
// resolved at construction time; the only post-init mutator is
// Provisioner (it needs to point at coord.Registry, so it can't be
// passed in until the registry exists).
type Coordinator struct {
	Registry    *ShardRegistry
	Pool        compute.ComputePool     // nil = no compute support
	Tail        *TailBroker             // nil = no live-tail subscriptions
	Ingest      *RingBuffer             // write path: IngestStream → buffer → shards; nil disables StartDrain
	Provisioner *Provisioner            // shard lifecycle management (nil = no auto-provisioning); set post-construction
	Usage       *CustomerUsageStats     // nil = no per-customer byte metering
	Store       *store.Store            // nil = no tenant-scoped features (/bin, alert dispatch); test-only
	SeenApps    *SeenApps               // ingest-stream app discovery; constructor always installs
	DLQ         *DLQ                    // S3 dead-letter queue for failed ingests
	Errors      *IngestionErrorRecorder // async error persistence
	Notifier    NotifyDispatcher        // per-customer alert dispatcher (nil = disabled). Interface type so a nil value is true-nil — no typed-nil-via-concrete-pointer trap when callers thread it into AlertContext.
	shardSecret       string
	lambdaFunctionURL string
	// ScriptExecutionTimeout caps how long a single runLocal invocation
	// may run. Zero = use the package-level default (scriptExecutionTimeout).
	// Tests override to shorten the wait.
	ScriptExecutionTimeout time.Duration

	// loweredCache memoizes the decoded planner.LoweredPlan per watch
	// so the hot fire path skips the JSON unmarshal that otherwise
	// dominates the per-fire cost (~9µs of the ~10µs lowered total).
	// Keyed by watch.ID; the value's contentHash mirrors CronWatch.
	// ContentHash so a save (which bumps content_hash) invalidates the
	// entry. Pre-fork in-memory only — restart-as-cache-invalidation.
	loweredCache sync.Map // map[uint]*loweredCacheEntry
}

// loweredCacheEntry pairs a decoded LoweredPlan with the
// CronWatch.ContentHash it was decoded from. Compare hash to detect
// stale entries (the watch was re-saved on this Coordinator or on
// another; either way the cache misses and re-unmarshals).
type loweredCacheEntry struct {
	contentHash string
	plan        *planner.LoweredPlan
}

// CoordinatorConfig wires every Coordinator dependency at construction
// time. Replaces the old "build empty + assign fields after" pattern in
// cmd/coordinator/main.go that the defensive-code audit flagged: when
// the canonical wire path mutates fields after construction, every
// runtime path has to defend against the half-built window.
//
// Required: Pool. Everything else is optional — nil means "feature
// disabled" with a documented runtime fall-through, not "we forgot to
// wire it." Provisioner is intentionally absent: it takes a
// *ShardRegistry that NewCoordinator builds, so circular ordering
// forces it to be assigned after construction. See coord.Provisioner.
type CoordinatorConfig struct {
	Pool   compute.ComputePool
	Shards []shard.Shard

	Store    *store.Store
	Notifier NotifyDispatcher
	Tail     *TailBroker
	Ingest   *RingBuffer
	Usage    *CustomerUsageStats
	DLQ      *DLQ
	Errors   *IngestionErrorRecorder

	// ShardSecret is the bearer token for shard gRPC auth. Passed
	// to compute workers so they can dial shards directly.
	ShardSecret string
	// LambdaFunctionURL is the Lambda endpoint for cold shard queries.
	// Passed to compute workers for archived shard descriptors.
	LambdaFunctionURL string

	// ScriptExecutionTimeout overrides the package-level default for
	// runLocal. Zero leaves the default in place.
	ScriptExecutionTimeout time.Duration
}

// SeenApps tracks distinct (app, env) tuples observed during
// ingestion, keyed by customer. Env-less entries are stored in the
// usual "<ns>/<app>" form; env-bearing entries get the env spliced
// in: "<ns>/<env>/<rest-of-app>". This lets the user-facing
// directory listing show env subdirectories alongside flat .log
// files in the same namespace — see docs/plans/ingest-dimensions.md.
type SeenApps struct {
	mu   sync.RWMutex
	apps map[string]map[string]bool // customer → set of slash-encoded names
}

func NewSeenApps() *SeenApps {
	return &SeenApps{apps: make(map[string]map[string]bool)}
}

// encodeName splices env into the app's namespace prefix. Given
// app="weft/coordinator" and env="prod", returns "weft/prod/coordinator".
// Empty env returns app unchanged. App without a namespace prefix
// (single-segment) gets "<env>/<app>" so single-app deployments still
// produce a valid 3-segment path.
func encodeName(app, env string) string {
	if env == "" {
		return app
	}
	if i := strings.Index(app, "/"); i >= 0 {
		return app[:i] + "/" + env + "/" + app[i+1:]
	}
	return env + "/" + app
}

// Add records an (app, env) tuple for a customer. Returns true if
// the resulting encoded name is new.
func (s *SeenApps) Add(customer, app, env string) bool {
	name := encodeName(app, env)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apps[customer] == nil {
		s.apps[customer] = make(map[string]bool)
	}
	if s.apps[customer][name] {
		return false
	}
	s.apps[customer][name] = true
	return true
}

// Names returns seen entries for a specific customer, sorted. Each
// entry is either "<ns>/<app>" (env-less) or "<ns>/<env>/<app>"
// (env-bearing).
func (s *SeenApps) Names(customer string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cApps := s.apps[customer]
	if len(cApps) == 0 {
		return nil
	}
	names := make([]string, 0, len(cApps))
	for a := range cApps {
		names = append(names, a)
	}
	sort.Strings(names)
	return names
}

// NewCoordinator builds a fully-wired coordinator from cfg. The
// returned coordinator is ready to serve requests immediately; no
// post-construction field mutation is required (or expected) other
// than Provisioner — see CoordinatorConfig for why that one is the
// exception.
//
// Shards in cfg.Shards are registered with no Apps populated; callers
// that need the entries to participate in app-specific query routing
// must add them via coord.Registry.Add(...) with explicit Apps.
func NewCoordinator(cfg CoordinatorConfig) *Coordinator {
	reg := NewShardRegistry()
	for _, s := range cfg.Shards {
		reg.Add(&ShardEntry{
			ID:        s.ID(),
			State:     StateIngesting,
			Shard:     s,
			CreatedAt: time.Now(),
		})
	}
	pool := cfg.Pool
	if pool == nil {
		// Every query routes through the compute pool — including the
		// zero-stage shard-only case — so a Coordinator without a Pool
		// would have nothing to dispatch to. Default to an in-process
		// LocalComputePool; callers wanting remote compute pass a
		// GRPCComputePool explicitly.
		pool = &compute.LocalComputePool{}
	}
	return &Coordinator{
		Registry:               reg,
		Pool:                   pool,
		Tail:                   cfg.Tail,
		Ingest:                 cfg.Ingest,
		Usage:                  cfg.Usage,
		Store:                  cfg.Store,
		SeenApps:               NewSeenApps(),
		DLQ:                    cfg.DLQ,
		Errors:                 cfg.Errors,
		Notifier:               cfg.Notifier,
		shardSecret:            cfg.ShardSecret,
		lambdaFunctionURL:      cfg.LambdaFunctionURL,
		ScriptExecutionTimeout: cfg.ScriptExecutionTimeout,
	}
}

// StartDrain runs a background goroutine that drains the ring buffer
// into shards. Routes rows by app to the correct INGESTING shard.
func (c *Coordinator) StartDrain(ctx context.Context) {
	if c.Ingest == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.Ingest.Notify():
			}

			for {
				batch := c.Ingest.Drain(500)
				if batch == nil {
					break
				}

				// Group by app for targeted routing; count bytes per customer.
				byApp := make(map[string][]*pb.LogRow)
				for _, row := range batch {
					byApp[row.App] = append(byApp[row.App], row)
					if c.Usage != nil && row.Customer != "" {
						c.Usage.Record(row.Customer, int64(len(row.Raw)))
					}
				}

				for app, rows := range byApp {
					sh := c.Registry.IngestingShard(app)
					if sh == nil {
						sh = c.Registry.DefaultIngestingShard()
					}
					if sh == nil {
						// No healthy shards at all — DLQ immediately.
						dlqPath := c.DLQ.Write(rows)
						c.Errors.Record("none", "no healthy shards available", rows, dlqPath)
						log.Printf("drain: DLQ'd %d rows for %s (no healthy shards)", len(rows), app)
						continue
					}
					if err := sh.Ingest(rows); err != nil {
						// Count consecutive failures; only suspend once a
						// sustained failure run crosses suspendFailureThreshold.
						// One flaky RPC shouldn't take a shard out of rotation.
						failures := c.Registry.RecordIngestFailure(sh.ID())
						if c.Registry.Get(sh.ID()).Suspended {
							log.Printf("drain: suspending shard %s after %d consecutive failures: %v",
								sh.ID(), failures, err)
						} else {
							log.Printf("drain: ingest failed on shard %s (%d/%d): %v",
								sh.ID(), failures, suspendFailureThreshold, err)
						}

						// Retry on a different healthy shard.
						fallback := c.Registry.IngestingShard(app)
						if fallback == nil {
							fallback = c.Registry.DefaultIngestingShard()
						}
						if fallback != nil {
							if retryErr := fallback.Ingest(rows); retryErr == nil {
								c.Registry.ResetIngestFailures(fallback.ID())
								if c.Provisioner != nil {
									c.Provisioner.Stats().Record(app, int64(len(rows)))
								}
								continue // retry succeeded
							}
						}

						// All retries exhausted — DLQ.
						dlqPath := c.DLQ.Write(rows)
						c.Errors.Record(sh.ID(), err.Error(), rows, dlqPath)
						log.Printf("drain: DLQ'd %d rows for %s (shard %s failed, no fallback)", len(rows), app, sh.ID())
						continue
					}
					c.Registry.ResetIngestFailures(sh.ID())
					if c.Provisioner != nil {
						c.Provisioner.Stats().Record(app, int64(len(rows)))
					}
				}
			}
		}
	}()
}

// StartUsageFlush runs a background goroutine that periodically drains
// completed hourly usage counters to PostgreSQL. Flushes every 5 minutes
// so at most 5 minutes of data is lost on restart.
func (c *Coordinator) StartUsageFlush(ctx context.Context, store *store.Store) {
	if c.Usage == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// Final flush on shutdown.
				c.flushUsage(store)
				return
			case <-ticker.C:
				c.flushUsage(store)
			}
		}
	}()
}

func (c *Coordinator) flushUsage(store *store.Store) {
	entries := c.Usage.DrainCompleted()
	for _, e := range entries {
		if err := store.UpsertUsageHourly(e.Customer, e.Hour, e.Bytes); err != nil {
			log.Printf("usage flush error: customer=%s hour=%s: %v", e.Customer, e.Hour, err)
		}
	}
	if len(entries) > 0 {
		log.Printf("usage flush: wrote %d hourly entries", len(entries))
	}
}

// GetShards returns all live shard connections.
func (c *Coordinator) GetShards() []shard.Shard {
	return c.Registry.AllShards()
}

// IngestingShard returns the current INGESTING shard for the given app.
func (c *Coordinator) IngestingShard(app string) shard.Shard {
	return c.Registry.IngestingShard(app)
}

// clampTimeRange floors tr.Start at (now − LogRetention(tier)) so that
// the SQL ts predicate the planner emits and the shard-set the registry
// returns share the same lower bound. Without the clamp here, a
// boundary-spanning shard could return rows older than the customer's
// retention because the planner's ts predicate would carry the user's
// raw start time. custID == 0 falls back to the trial retention.
//
// The registry's MatchingEntries re-applies this clamp as defense in
// depth — idempotent because we pass the already-clamped value.
func (c *Coordinator) clampTimeRange(tr TimeRange, custID uint) TimeRange {
	var tier store.Tier
	if custID != 0 && c.Store != nil {
		tier = c.Store.TierForCustomer(custID)
	}
	cap := time.Now().Add(-store.LogRetention(tier))
	if !tr.Start.IsZero() && tr.Start.Before(cap) {
		tr.Start = cap
	}
	return tr
}

// Execute is the single dispatch entry point. It accepts any user-
// typed pipeline (a /log query, a plain shell pipeline, a /bin
// script invocation, a mix) and returns an ExecuteResult whose
// Stream emits output lines forward-only.
//
// Internally Execute always routes through warp + shellfs with the
// log-query rewriter installed: rewriteQueryPipelines walks the
// parsed AST and substitutes any pipeline whose first command is
// `cat`/`head`/`tail` on `/log/...` with a CallbackCommand that
// re-enters runQuery for that one pipeline. So a pipeline like
// `for app in api web; do cat /log/$app.log | wc -l; done` produces
// two callback dispatches (one per loop iteration), interleaved
// with the surrounding bash that warp executes natively. Top-level
// log queries are just the trivial single-pipeline case of the same
// rewrite.
//
// userID identifies the human who invoked the pipeline — used to
// attribute any /bin /cron.d /tmp writes the pipeline performs.
// Empty string means "no attribution" (system invocation, automated
// path).
//
// Callers that don't pump bytes themselves (tests, alert post-
// processors) drain the stream into a buffered DrainedResult via
// res.Drain(ctx).
func (c *Coordinator) Execute(pipeline string, tr TimeRange, customer, userID string) (*ExecuteResult, error) {
	return c.execute(pipeline, tr, customer, userID, nil)
}

// ExecuteFollow runs pipeline with live-tail semantics. The returned
// stream emits the historical batch (same shape as Execute) and then
// transitions to a TailBroker subscription that emits matching rows
// as the shards receive them, until ctx cancels.
//
// pipeline must be a plannable log query (`tail -f /log/...` or any
// `cat`/`head`/`tail` shape over /log paths) — non-log shapes return
// planner.ErrNotPlannable. c.Tail must be configured.
//
// Notices from the historical phase are not surfaced. Today's only
// notice trigger (/tmp writes) doesn't fire on follow's read-only
// historical pass; if that changes, plumb Notices through the concat
// transition.
func (c *Coordinator) ExecuteFollow(ctx context.Context, pipeline string, tr TimeRange, customer, userID string) (*ExecuteResult, error) {
	if c.Tail == nil {
		return nil, fmt.Errorf("ExecuteFollow: coordinator has no tail broker")
	}
	// Plan again here just to extract the live-filter args
	// (FollowApp/FollowPatterns/LiveStages). The historical batch
	// re-plans inside the rewriter callback, so customer + bounds
	// don't need to match exactly — pass empty/zero so this Plan
	// call never produces SQL we'd care about.
	plan, err := planner.Plan(pipeline, "", time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}

	historical, err := c.execute(pipeline, tr, customer, userID, nil)
	if err != nil {
		return nil, err
	}

	matcher := &RowMatcher{App: plan.FollowApp, Patterns: plan.FollowPatterns}
	filter := BuildLiveFilter(plan)

	next := func() compute.LineStream {
		id, ch := c.Tail.Subscribe(ctx, matcher)
		return &brokerStream{
			ch:     ch,
			filter: filter,
			unsub:  func() { c.Tail.Unsubscribe(id) },
		}
	}

	return &ExecuteResult{
		Stream: &concatStream{primary: historical.Stream, next: next},
	}, nil
}

// ExecuteForWatch runs pipeline with the supplied watch bound to
// the alert stage (if any), so incidents and dispatches use watch
// identity instead of interactive-mode defaults. Watches run as the
// system, not as a user — any writes the watch performs land with
// NULL creator.
//
// Same dispatch surface as Execute; the only difference is the
// AlertContext binding.
func (c *Coordinator) ExecuteForWatch(pipeline string, tr TimeRange, watch *store.CronWatch) (*ExecuteResult, error) {
	// c.Notifier is the NotifyDispatcher interface, so a nil-Notifier
	// coordinator threads true-nil into AlertContext (no typed-nil-
	// via-concrete-pointer trap).
	alertCtx := &AlertContext{
		Store:      c.Store,
		Watch:      watch,
		CustomerID: watch.CustomerID,
		Notifier:   c.Notifier,
	}

	// Fast path: the watch was lowered at save time and the lowered
	// form is still hot. Hydrate args + dispatch directly, bypassing
	// preflight (which hits the DB to resolve the customer name) and
	// Plan (which re-parses + re-walks the pipeline). Phase A profiling
	// pinned customer-resolve + Plan + expandBin at ~15-25% of the
	// per-fire coordinator cost; this is where that saves cleanly.
	if res, ok := c.executeLowered(watch, tr, alertCtx); ok {
		return res, nil
	}

	// Fall-through: lowered form is missing, stale (version bump or
	// /bin invalidation), or unrenderable. Run the original path and
	// opportunistically re-lower + write back so the next fire is hot.
	cust, err := c.Store.GetCustomerByID(watch.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("watch %d: resolve customer %d: %w", watch.ID, watch.CustomerID, err)
	}
	// Refresh runs synchronously rather than via a goroutine. Two
	// reasons: (1) it shares the same DB connection as the rest of
	// the fire, and an async goroutine over a BeginTest-wrapped
	// store thrashes the single conn; (2) on the slow path the
	// fire is already ~1ms, so an extra ~hundred-µs of refresh DB
	// write doesn't move user-perceived latency. The next fire
	// takes the hot path either way.
	c.refreshLoweredSync(watch, cust.Name)
	return c.execute(pipeline, tr, cust.Name, "", alertCtx)
}

// executeLowered tries the hot-path dispatch using watch.LoweredPlan.
// Returns (nil, false) on any condition that should fall through to
// the slow path: no plan persisted, version mismatch, /bin dep hash
// drift, marshalling error, or follow/alert features the lowered
// path doesn't handle yet. The caller surfaces those as a slow-path
// re-plan + opportunistic re-lowering.
func (c *Coordinator) executeLowered(watch *store.CronWatch, tr TimeRange, alertCtx *AlertContext) (*ExecuteResult, bool) {
	if len(watch.LoweredPlan) == 0 {
		return nil, false
	}
	lp, ok := c.loadLoweredPlan(watch)
	if !ok {
		return nil, false
	}
	if lp.PlannerVersion != planner.PlannerVersion {
		log.Printf("watch %d: lowered_plan stamped at version %d, planner is at %d — recompiling",
			watch.ID, lp.PlannerVersion, planner.PlannerVersion)
		// Stale planner version invalidates the cache entry too.
		c.loweredCache.Delete(watch.ID)
		return nil, false
	}
	// Check every recorded /bin dep against the current source hash.
	// A drift (someone edited /bin/foo) invalidates the lowered plan
	// for every watch that referenced it. The slow path re-lowers.
	for _, dep := range lp.BinDeps {
		fn, err := c.Store.GetShellFunction(watch.CustomerID, dep.Name)
		if err != nil || fn == nil {
			return nil, false
		}
		if planner.HashBinSource(fn.Source) != dep.Hash {
			return nil, false
		}
	}
	// TODO Phase B v2: alert post-processing + follow mode through
	// the lowered path. Today fall through to slow path for both —
	// the bench numbers don't include either, and the slow-path
	// re-lowering keeps the lowered_plan populated for non-alert
	// non-follow fires.
	if len(lp.AlertArgs) > 0 || lp.Follow {
		return nil, false
	}
	// Clamp time range using lp.Tier — baked at save time so we skip
	// the TierForCustomer DB hit. A tier upgrade between save and
	// fire stays on the old retention until the watch is re-saved
	// (or another path triggers re-lowering). Acceptable tradeoff
	// for the perf win on the hot path; documented on LoweredPlan.Tier.
	tier := store.Tier(lp.Tier)
	cap := time.Now().Add(-store.LogRetention(tier))
	if !tr.Start.IsZero() && tr.Start.Before(cap) {
		tr.Start = cap
	}
	ctx := planner.ResolveCtx{Customer: lp.Customer, Start: tr.Start, End: tr.End}
	args := lp.Hydrate(ctx)
	res, err := c.dispatchLoweredWithTier(lp, args, tr, tier, alertCtx)
	if err != nil {
		log.Printf("watch %d: lowered dispatch failed: %v — falling back to inline Plan", watch.ID, err)
		return nil, false
	}
	return res, true
}

// dispatchLoweredWithTier ships the hydrated lowered plan to shards.
// Skips preflight (no Plan, no /bin expansion), skips the
// CustomerHasSource validation (the source was valid at save time;
// row-level security at the shard's `customer = ?` predicate still
// holds for cross-tenant safety — the only thing that can change is
// "namespace deleted by owner", which renders the query empty rather
// than wrong), and skips the per-fire TierForCustomer lookup (tier
// is baked into LoweredPlan at save time).
func (c *Coordinator) dispatchLoweredWithTier(lp *planner.LoweredPlan, args []any, tr TimeRange, tier store.Tier, alertCtx *AlertContext) (*ExecuteResult, error) {
	entries := c.Registry.MatchingEntries(tr, lp.FollowApp, tier)
	if len(entries) == 0 {
		return &ExecuteResult{Stream: compute.NewSliceLineStream(nil, nil)}, nil
	}
	descs := make([]*pb.ShardDescriptor, len(entries))
	for i, e := range entries {
		d := &pb.ShardDescriptor{ShardId: e.ID, Apps: e.Apps}
		if e.State == StateArchived {
			d.LambdaUrl = c.lambdaFunctionURL
			d.ParquetPath = e.ParquetPath
		} else {
			d.GrpcAddr = e.Addr
		}
		descs[i] = d
	}
	argStrs := make([]string, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			argStrs[i] = s
		} else {
			argStrs[i] = fmt.Sprintf("%v", a)
		}
	}
	stream, err := c.Pool.ExecuteQuery(&pb.ExecuteQueryRequest{
		Sql:         lp.DirectShardSQL,
		Args:        argStrs,
		Shards:      descs,
		Stages:      lp.ComputeStages,
		MergeKey:    lp.MergeKey,
		Limit:       int32(lp.Limit),
		ShardSecret: c.shardSecret,
		KeepLast:    lp.TailMode,
	})
	if err != nil {
		return nil, err
	}
	return &ExecuteResult{Stream: stream}, nil
}

// loadLoweredPlan decodes watch.LoweredPlan, memoizing the result in
// loweredCache keyed by watch.ID. Cache hits compare contentHash
// against the current watch.ContentHash so a save (which rewrites
// content_hash) misses cleanly and we re-unmarshal. Returns (nil,
// false) if the JSON can't be unmarshalled — the caller treats that
// as cache miss + fallback.
func (c *Coordinator) loadLoweredPlan(watch *store.CronWatch) (*planner.LoweredPlan, bool) {
	if cached, ok := c.loweredCache.Load(watch.ID); ok {
		entry := cached.(*loweredCacheEntry)
		if entry.contentHash == watch.ContentHash {
			return entry.plan, true
		}
	}
	var lp planner.LoweredPlan
	if err := json.Unmarshal(watch.LoweredPlan, &lp); err != nil {
		log.Printf("watch %d: lowered_plan unmarshal: %v — falling back to inline Plan", watch.ID, err)
		return nil, false
	}
	// Store by-value into the entry so concurrent fires share the
	// decoded *LoweredPlan — its fields are read-only past this
	// point (Hydrate constructs a fresh args slice per call).
	c.loweredCache.Store(watch.ID, &loweredCacheEntry{
		contentHash: watch.ContentHash,
		plan:        &lp,
	})
	return &lp, true
}

// refreshLoweredSync is the re-lowering invoked from the fallback
// path. Runs Lower on the watch's current pipeline + writes the JSON
// back via RefreshWatchLoweredPlan. Failures log but don't retry —
// the next fire's fallback path will try again.
//
// Skips watches with ID == 0: those aren't persisted (in-memory test
// fixtures, benchmark scaffolding, ephemeral pipelines from the
// interactive shell that briefly impersonate a watch via
// ExecuteForWatch). Writing back would either fail noisily or thrash
// the test DB conn.
//
// Synchronous (not a goroutine) so it shares the caller's DB
// connection — the BeginTest-wrapped store binds to a single conn
// and a concurrent goroutine over that conn races the main fire's
// own DB activity. The latency cost (~hundreds of µs of DB writes)
// is paid once per slow-path fire; the next fire is on the hot path.
func (c *Coordinator) refreshLoweredSync(watch *store.CronWatch, customerName string) {
	if watch.ID == 0 {
		return
	}
	resolver := watchBinResolver{store: c.Store, customerID: watch.CustomerID}
	tier := string(c.Store.TierForCustomer(watch.CustomerID))
	loweredJSON := planner.LowerForStorage(watch.Pipeline, customerName, tier, resolver)
	if loweredJSON == nil {
		return
	}
	if err := c.Store.RefreshWatchLoweredPlan(watch.ID, loweredJSON); err != nil {
		log.Printf("watch %d: refresh lowered_plan: %v", watch.ID, err)
		return
	}
	// Refresh wrote a new lowered_plan; the next fire's loadLoweredPlan
	// will see the fresh bytes on watch.LoweredPlan + the unchanged
	// ContentHash (refresh doesn't bump it, only save does). Invalidate
	// the cache so the next hit re-unmarshals from the fresh bytes.
	c.loweredCache.Delete(watch.ID)
}

// watchBinResolver is the coordinator-side BinResolver used by the
// async refresh path. Mirrors the adapter in api/handlers.go and
// mounts/cron.go — kept duplicated rather than shared (10-line type).
type watchBinResolver struct {
	store      *store.Store
	customerID uint
}

func (r watchBinResolver) Source(name string) (string, string, bool) {
	fn, err := r.store.GetShellFunction(r.customerID, name)
	if err != nil || fn == nil {
		return "", "", false
	}
	return fn.Source, planner.HashBinSource(fn.Source), true
}

// execute is the unified dispatcher. Preflights (resolves customer
// name, expands source-arg globs against SeenApps), peels a
// trailing `| alert ...` stage (warp has no `alert` builtin), runs
// the rest through warp+shellfs+rewriter via streaming runLocal,
// then applies alert post-processing if needed.
//
// alertCtx threads the watch identity through for ExecuteForWatch
// invocations. Public Execute passes nil so an alert stage runs in
// interactive defaults; the rewriter callback also picks up
// alertCtx for interior queries inside cron-driven /bin scripts.
//
// Without an alert stage, the live ExecuteResult from runLocal is
// returned as-is — bytes flow from warp through the pipe to the
// consumer as warp produces them. With an alert stage, the live
// stream is drained into a buffered shape (alerts are inherently
// terminal-aggregate, the whole body is needed before the alert can
// fire), runAlertStage post-processes, and a buffered ExecuteResult
// is returned.
func (c *Coordinator) execute(pipeline string, tr TimeRange, customer, userID string, alertCtx *AlertContext) (*ExecuteResult, error) {
	custID, expanded, err := c.preflight(pipeline, customer)
	if err != nil {
		return nil, err
	}

	queryPart, alertArgs := planner.SplitAlertStage(expanded)

	res, err := c.runLocal(queryPart, tr, customer, custID, userID, alertCtx)
	if err != nil {
		return nil, err
	}

	if alertArgs == nil {
		// No alert: streaming pass-through.
		return res, nil
	}

	// Alert path: drain the live stream into the buffered shape,
	// run the alert against it, return a buffered ExecuteResult.
	drained, drainErr := res.Drain(context.Background())
	if drainErr != nil {
		return nil, drainErr
	}
	if alertCtx == nil {
		alertCtx = &AlertContext{
			Store:       c.Store,
			CustomerID:  custID,
			Interactive: true,
			Notifier:    c.Notifier,
		}
	}
	qr := &QueryResult{
		Text:    drained.Text,
		Stderr:  drained.Stderr,
		Notices: drained.Notices,
	}
	runAlertStage(qr, alertArgs, alertCtx)
	return queryResultToExecuteResult(qr), nil
}

// runQuery plans a single log pipeline (already preflighted, with
// any trailing `| alert ...` collapsed into plan.AlertArgs by the
// planner) and dispatches it through the compute pool. Callable
// only from rewriteQueryPipelines's CallbackCommand — the rewriter
// is the only thing that classifies pipelines as log queries; the
// public Execute / Query / ExecuteForWatch surface goes through the
// warp+rewriter path and never hits runQuery directly.
//
// Forks between a streaming-direct path (compute's live channel
// stream forwarded straight to ExecuteResult.Stream) and a buffered
// path (executeQuery → QueryResult → slice-replay stream). The
// streaming path applies when the plan has zero compute stages and
// no alert; everything else buffers because alert post-processing
// requires the full result up front. RedirectTarget is always empty
// here — the rewriter strips redirects from the snapshot before
// re-rendering, lifting them onto the CallbackCommand so warp's
// own redirect machinery handles the FS write.
func (c *Coordinator) runQuery(pipeline string, tr TimeRange, customer string, custID uint, userID string, alertCtx *AlertContext) (*ExecuteResult, error) {
	// Clamp tr to the customer's tier retention BEFORE planning so the
	// SQL's ts predicates and the shard-selection in MatchingEntries
	// agree on the same start. Without the clamp here, a boundary-
	// spanning shard could return rows older than the customer's
	// retention because the SQL would have no lower-bound predicate.
	tr = c.clampTimeRange(tr, custID)

	plan, err := planner.Plan(pipeline, customer, tr.Start, tr.End)
	if err != nil {
		// The rewriter only substitutes pipelines whose first command
		// is on /log/..., which the planner accepts. ErrNotPlannable
		// here would mean the pipeline shape changed between
		// substitution and re-render — surface it.
		log.Printf("rewriter callback: plan error: %v (pipeline=%q)", err, pipeline)
		return nil, fmt.Errorf("plan: %w", err)
	}

	execResult, err := c.executeQuery(plan, tr, customer, custID, userID, alertCtx)
	if err != nil {
		return nil, err
	}

	if plan.AlertArgs == nil {
		return execResult, nil
	}

	// Alert post-processing needs the full text — drain the stream.
	drained, err := execResult.Drain(context.Background())
	if err != nil {
		return nil, fmt.Errorf("drain for alert: %w", err)
	}
	qr := &QueryResult{Text: drained.Text, Notices: drained.Notices}
	if alertCtx == nil {
		alertCtx = &AlertContext{
			Store:       c.Store,
			CustomerID:  custID,
			Interactive: true,
			Notifier:    c.Notifier,
		}
	}
	runAlertStage(qr, plan.AlertArgs, alertCtx)
	return queryResultToExecuteResult(qr), nil
}

// preflight resolves the customer name to an ID, pre-expands /bin
// bash invocations (so the planner can see their body), and expands
// source-arg globs against SeenApps. Shared by runQuery and
// executeLocal so non-log pipelines that fall through Execute pay
// the expansion cost twice — acceptable since the steps are
// idempotent and the DB hits are cache-friendly. ExecuteForWatch
// hits preflight once via runQuery on the happy path.
func (c *Coordinator) preflight(pipeline string, customer string) (uint, string, error) {
	var custID uint
	if customer != "" {
		cust, err := c.Store.GetCustomerByName(customer)
		if err != nil {
			return 0, "", fmt.Errorf("resolve customer %q: %w", customer, err)
		}
		custID = cust.ID
	}

	if custID != 0 {
		fs := shellfs.New(c.Store, custID, nil)
		// Reject `tail -f ... | <bin-script>` before inlining. Bash
		// bodies get spliced by expandBinPipeline below, so if we
		// waited until after expansion, a follow pipeline calling a
		// bash /bin script would slip through looking like a regular
		// pipeline (and Lua scripts would pass silently because
		// they're opaque). Failing here keeps the rule uniform and
		// the error pointed at the right stage name.
		if err := rejectBinInFollow(pipeline, fs); err != nil {
			return 0, "", err
		}
		expanded, err := expandBinPipeline(pipeline, fs)
		if err != nil {
			return 0, "", fmt.Errorf("expand /bin: %w", err)
		}
		pipeline = expanded

		// Expand source-arg globs (`cat /log/<ns>/<env>/*.log`) to a
		// concrete multi-arg form the planner can union. Fan-out is
		// scoped to apps observed in this customer's ingest stream
		// (SeenApps) — that includes weftd metrics streams that
		// don't have a configured log_groups row. The per-customer
		// namespace allowlist downstream still vets the result.
		// SeenApps guard supports tests that build Coordinator
		// without going through NewCoordinator.
		if c.SeenApps != nil {
			if apps := c.SeenApps.Names(customer); len(apps) > 0 {
				expanded, err := planner.ExpandSourceGlobs(pipeline, apps)
				if err != nil {
					return 0, "", fmt.Errorf("expand glob: %w", err)
				}
				pipeline = expanded
			}
		}
	}

	return custID, pipeline, nil
}

// executeQuery runs the query half of a plan (everything except alert).
// customer is the name used for shard routing; custID (0 if unresolved)
// scopes tenant-backed features (ShellFS, weft.*). alertCtx is forwarded
// to runLocal so interior queries dispatched by the interceptor inherit
// the caller's alert identity; may be nil.
func (c *Coordinator) executeQuery(plan *planner.QueryPlan, tr TimeRange, customer string, custID uint, userID string, alertCtx *AlertContext) (*ExecuteResult, error) {
	if plan.ShardSQL == "" {
		return &ExecuteResult{Stream: compute.NewSliceLineStream(nil, nil)}, nil
	}

	// Allowlist: the pipeline's source app (the part before '/') must
	// resolve to a namespace this customer owns. Gate here, before the
	// app name ever reaches SQL interpolation — the planner escapes
	// string literals, but an allowlist is the correct posture for a
	// user-controlled identifier.
	if plan.SourceApp != "" {
		cust, err := c.Store.GetCustomerByName(customer)
		if err != nil {
			return nil, fmt.Errorf("validate source: resolve customer %q: %w", customer, err)
		}
		ok, err := c.Store.CustomerHasSource(cust.ID, plan.SourceApp)
		if err != nil {
			return nil, fmt.Errorf("validate source: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("unknown source: %s", plan.SourceApp)
		}
	}

	// Every shard query goes through plan.DirectShardSQL so each shard
	// emits rows in the order compute's k-way merge expects (see
	// docs/plans/direct-to-compute.md §3). The compute job handles
	// merge / limit / staged transforms uniformly; zero stages means
	// "just merge and limit, no transforms." Customer + ts predicates
	// are baked in by the planner.
	directSQL := plan.DirectShardSQL

	// Shard selection honors plan-tier retention: tr.Start is clamped
	// up to (now − LogRetention(tier)) inside MatchingEntries as defense
	// in depth. The caller (runQuery) has already applied the same clamp
	// before invoking Plan so the SQL's ts predicate is consistent with
	// the shard set.
	var tier store.Tier
	if custID != 0 {
		tier = c.Store.TierForCustomer(custID)
	}
	entries := c.Registry.MatchingEntries(tr, plan.FollowApp, tier)
	if len(entries) == 0 {
		queryLog.Printf("query: no matching shards (app=%s, customer=%s)", plan.SourceApp, customer)
		return &ExecuteResult{Stream: compute.NewSliceLineStream(nil, nil)}, nil
	}

	descs := make([]*pb.ShardDescriptor, len(entries))
	shardIDs := make([]string, len(entries))
	for i, e := range entries {
		shardIDs[i] = e.ID
		d := &pb.ShardDescriptor{ShardId: e.ID, Apps: e.Apps}
		if e.State == StateArchived {
			d.LambdaUrl = c.lambdaFunctionURL
			d.ParquetPath = e.ParquetPath
		} else {
			d.GrpcAddr = e.Addr
		}
		descs[i] = d
	}
	queryLog.Printf("query: app=%s shards=%v compute=%v sql=%s",
		plan.SourceApp, shardIDs, plan.ComputeStages, directSQL)

	argStrs := make([]string, len(plan.Args))
	for i, a := range plan.Args {
		if s, ok := a.(string); ok {
			argStrs[i] = s
		} else {
			// Today the planner only emits string args (customer name,
			// namespace name, ISO timestamps). Non-string slips through
			// as %v rendering — surface it as a typed bug if it ever
			// happens rather than silent data loss.
			argStrs[i] = fmt.Sprintf("%v", a)
			queryLog.Printf("query: non-string arg at position %d (%T %v) — bound via fmt", i, a, a)
		}
	}
	stream, err := c.Pool.ExecuteQuery(&pb.ExecuteQueryRequest{
		Sql:         directSQL,
		Args:        argStrs,
		Shards:      descs,
		Stages:      plan.ComputeStages,
		MergeKey:    plan.ShardOrderBy,
		Limit:       int32(plan.Limit),
		ShardSecret: c.shardSecret,
		// tail-mode: shard SQL wraps to ASC; compute must retain
		// last-N instead of stopping after first-N emitted.
		KeepLast: plan.TailMode,
	})
	if err != nil {
		queryLog.Printf("query: compute error: %v", err)
		return nil, err
	}
	return &ExecuteResult{Stream: stream}, nil
}

// runAlertStage feeds a QueryResult's text into AlertCommand and
// merges its stderr into result.Stderr so the caller sees the alert
// message.
func runAlertStage(result *QueryResult, args []string, ctx *AlertContext) {
	var out, errOut strings.Builder
	AlertCommand(args, strings.NewReader(result.Text), &out, &errOut, ctx)
	if errOut.Len() == 0 {
		return
	}
	if ctx != nil && ctx.Interactive {
		result.Text = ""
	}
	if result.Stderr != "" && !strings.HasSuffix(result.Stderr, "\n") {
		result.Stderr += "\n"
	}
	result.Stderr += errOut.String()
}

// scriptExecutionTimeout caps how long a single runLocal invocation may
// run before the interpreter is cancelled. Guards against runaway saved
// scripts (infinite loops, pathological computation) burning server CPU
// or holding cron slots. Interior shard queries still complete on their
// own; the cap stops bash-level work between AST boundaries.
const scriptExecutionTimeout = 30 * time.Second

// runLocal executes a pipeline through warp locally with the log-
// query rewriter installed. Returns immediately with an
// ExecuteResult whose Stream is backed by io.Pipes that warp writes
// to as it runs — bytes flow to the consumer as they're produced,
// not after warp finishes.
//
// custID (non-zero) scopes ShellFS and weft.* functions to one
// tenant; zero disables both. tr/customer/alertCtx are captured by
// BinScriptTransform so that interior query pipelines inside /bin
// scripts — substituted at load time with CallbackCommand nodes —
// inherit the caller's time window, customer scope, and alert
// identity.
//
// Interior query pipelines are turned into callbacks by
// rewriteQueryPipelines, invoked at two points:
//  1. On the top-level AST here, covering query pipelines the caller
//     typed directly.
//  2. Via interp.BinScriptTransform, on each /bin/<name> script body
//     as warp parses it at invocation time — that's where the
//     interesting cases (loops, command substitutions, function
//     bodies) live, because /bin scripts are how users compose
//     multi-line / multi-pipeline logic.
//
// Both paths call the same rewriter, so the AST that warp executes
// has no pipelines left that need weft-side dispatching at runtime.
//
// Execution is capped at scriptExecutionTimeout. When the cap fires
// the interpreter is cancelled cooperatively; warp stops at the
// next AST boundary (between top-level lists or loop iterations).
// The timeout error is surfaced as the stream's terminal Next
// error, after the partial output already written has drained
// through.
//
// Notices are populated on the returned ExecuteResult AFTER warp
// finishes, BEFORE the pipe writers close — so consumers reading
// Notices past stream EOF see the populated value.
func (c *Coordinator) runLocal(pipeline string, tr TimeRange, customer string, custID uint, userID string, alertCtx *AlertContext) (*ExecuteResult, error) {
	ast, err := bash.Parse(pipeline)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	interp := bash.NewInterpreter()
	interp.SetStdin(strings.NewReader(""))

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	interp.SetStdout(stdoutW)
	interp.SetStderr(stderrW)

	bash.RegisterToolCommands(interp)

	noticeQueue := NewNoticeQueue(c.Store, userID)
	if custID != 0 {
		tier := c.Store.TierForCustomer(custID)
		opts := []shellfs.Option{
			shellfs.WithTier(tier),
			shellfs.WithCustomerName(customer),
			shellfs.WithNoticeQueue(noticeQueue),
		}
		if userID != "" {
			// The coordinator runLocal path is reached by query
			// execution and script runs — never by interactive vim
			// writes (those land on /api/fs/write). Defaulting to
			// zero perms means a pipeline redirect like `... > /bin/x`
			// is denied for non-owners; owners route through the FS
			// endpoint with their session, where it'll succeed.
			opts = append(opts, shellfs.WithUser(userID, mounts.OrgPerms{}))
		}
		interp.SetFS(shellfs.New(c.Store, custID, nil, opts...))

		wf := &WeftFuncs{Store: c.Store, CustomerID: custID}
		RegisterWeftCommands(interp, wf)
		RegisterWeftPredicates(interp, wf)
		interp.LuaExtensions = []luapkg.Extension{wf.LuaExtension()}

		// `wit` version-history surface — log/show/diff/checkout/amend
		// for /bin and /cron.d entities. Same scope as the weft.*
		// helpers; one WitFuncs per request carries the acting user.
		witFn := &WitFuncs{Store: c.Store, CustomerID: custID, UserID: userID}
		RegisterWitCommands(interp, witFn)

		rewriteQueryPipelines(ast, c, tr, customer, custID, userID, alertCtx)
		interp.BinScriptTransform = func(_ string, script *bash.Script) {
			rewriteQueryPipelines(script, c, tr, customer, custID, userID, alertCtx)
		}
	}

	stream := newLiveScriptStream(stdoutR, stderrR)
	res := &ExecuteResult{Stream: stream}

	// Run warp asynchronously. The goroutine sets res.Notices and any
	// terminal error on the stream BEFORE closing the pipe writers,
	// so a consumer that reads res.Notices after stream EOF sees the
	// populated value (program-order: write Notices → write runErr →
	// close writers; pipe-close synchronizes-with scanner-EOF on the
	// reader side, which synchronizes-with the consumer's terminal
	// Next via the channel send/receive in liveScriptStream).
	timeout := c.ScriptExecutionTimeout
	if timeout <= 0 {
		timeout = scriptExecutionTimeout
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		// Watcher: on timeout, ask the interpreter to stop at the
		// next AST boundary. ExecScript winds down cooperatively.
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				interp.Cancel()
			case <-watchDone:
			}
		}()
		interp.ExecScript(ast)
		close(watchDone)

		res.Notices = noticeQueue.Flush()
		if ctx.Err() == context.DeadlineExceeded {
			stream.setRunErr(fmt.Errorf("script exceeded %s execution time limit", timeout))
		}
		stdoutW.Close()
		stderrW.Close()
	}()

	return res, nil
}

