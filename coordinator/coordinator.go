package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/warp/pkg/bash"
	luapkg "github.com/graybeardcorp/warp/pkg/lua"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/mounts"
	"github.com/graybeardcorp/weft/notices"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/shellfs"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/theme"
	"github.com/graybeardcorp/weft/witrepo"
)

// TimeRange aliases logdb.TimeRange so callers inside the coordinator
// package can keep using the unqualified name. The canonical type
// lives in logdb because the registry (which lives there too) bounds
// its MatchingEntries query on it.
type TimeRange = logdb.TimeRange

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
	Stderr  string           // alert/error output — rendered in red by the shell
	Notices []notices.Notice // one-time per-user notices to surface in the response
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
	Notices []notices.Notice
	// FinalDirStack is the interpreter's dirstack (index 0 == cwd) after
	// the pipeline ran — the "ferry back" half of the dirstack courier.
	// Populated by the runLocal goroutine right before it closes the pipe
	// writers, on the same happens-before edge as Notices, so a caller
	// that reads it AFTER the stream exhausts sees the final value. Nil
	// when the pipeline was seeded with no dirstack (the non-courier
	// callers), in which case the cwd never left "/".
	FinalDirStack []string
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
	Notices []notices.Notice
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
	// (its program order: ExecScript → Notices = noticeQueue.Drain() →
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
// resolved at construction time; main.go later swaps in the real
// logdb.Provisioner once logdb.Start has wired its launcher and store.
// The default provisioner installed by NewCoordinator carries an empty
// registry so c.Provisioner is never nil — drain and query paths
// read fields fresh on every pass, so the swap is safe.
type Coordinator struct {
	Compute      compute.Executor        // required; every query dispatches through it
	Tail         *TailBroker             // nil = no live-tail subscriptions
	TailFanout   *TailFanout             // nil exactly when Tail is nil; wraps Tail, with or without Redis configured
	Ingest       *RingBuffer             // write path: IngestStream → buffer → shards. NewCoordinator always installs one; StartDrain still has to be called explicitly.
	Provisioner  *logdb.Provisioner      // shard lifecycle + registry. Never nil after NewCoordinator; main.go swaps in the real one once logdb.Start completes.
	Usage        *CustomerUsageStats     // nil = no per-customer byte metering
	Store        *store.Store            // nil = no tenant-scoped features (/bin, alert dispatch); test-only
	SeenServices *SeenServices           // ingest-stream app discovery; constructor always installs
	DLQ          *DLQ                    // S3 dead-letter queue for failed ingests
	Errors       *IngestionErrorRecorder // async error persistence
	Notifier     NotifyDispatcher        // per-customer alert dispatcher (nil = disabled). Interface type so a nil value is true-nil — no typed-nil-via-concrete-pointer trap when callers thread it into AlertContext.
	// ScriptExecutionTimeout caps how long a single runLocal invocation
	// may run. Zero = use the package-level default (scriptExecutionTimeout).
	// Tests override to shorten the wait.
	ScriptExecutionTimeout time.Duration
	// ShareURLBase is the absolute base URL `clipboard` short links are
	// built on. Empty = https://w3ft.net.
	ShareURLBase string

	// loweredCache memoizes the decoded planner.LoweredPlan per watch
	// so the hot fire path skips the JSON unmarshal that otherwise
	// dominates the per-fire cost (~9µs of the ~10µs lowered total).
	// Keyed by watch.ID; the value's contentHash mirrors CronWatch.
	// ContentHash so a save (which bumps content_hash) invalidates the
	// entry. Pre-fork in-memory only — restart-as-cache-invalidation.
	loweredCache sync.Map // map[uint]*loweredCacheEntry

	// nsIDCache memoizes (customer, namespace) → namespaces.id for the
	// ingest drain's hot path, so resolving a row's routing namespace_id
	// doesn't hit the DB per batch. Keyed by "customer\x00namespace".
	nsIDCache sync.Map // map[string]uint
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
// Required: Compute. Everything else is optional — nil means "feature
// disabled" with a documented runtime fall-through, not "we forgot to
// wire it." logdb.Provisioner is intentionally absent: NewCoordinator
// installs a no-op stub so c.Provisioner is never nil, and main.go
// swaps in the real one once logdb.Start completes.
type CoordinatorConfig struct {
	Compute compute.Executor

	Store    *store.Store
	Notifier NotifyDispatcher
	Tail     *TailBroker
	Ingest   *RingBuffer
	Usage    *CustomerUsageStats
	DLQ      *DLQ
	Errors   *IngestionErrorRecorder

	// RedisAddr/RedisTLS configure TailFanout's cross-coordinator
	// delivery (docs/plans/coordinator-horizontal-scaling.md item 4).
	// RedisAddr == "" leaves tail -f local-only, same as before this
	// field existed.
	RedisAddr string
	RedisTLS  bool

	// LambdaFunctionURL is the Lambda endpoint for cold shard queries.
	// Passed to compute workers for archived shard descriptors.
	LambdaFunctionURL string

	// ScriptExecutionTimeout overrides the package-level default for
	// runLocal. Zero leaves the default in place.
	ScriptExecutionTimeout time.Duration

	// ShareURLBase is the base URL `clipboard` short links are built on
	// ("" = https://w3ft.net).
	ShareURLBase string
}

// SeenServices tracks distinct (app, env) tuples observed during
// ingestion, keyed by customer. Env-less entries are stored in the
// usual "<ns>/<app>" form; env-bearing entries get the env spliced
// in: "<ns>/<env>/<rest-of-app>". This lets the user-facing
// directory listing show env subdirectories alongside flat .log
// files in the same namespace — see docs/plans/ingest-dimensions.md.
type SeenServices struct {
	mu   sync.RWMutex
	apps map[int64]map[string]bool // customer id → set of slash-encoded names
}

func NewSeenServices() *SeenServices {
	return &SeenServices{apps: make(map[int64]map[string]bool)}
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
func (s *SeenServices) Add(customerID int64, app, env string) bool {
	name := encodeName(app, env)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apps[customerID] == nil {
		s.apps[customerID] = make(map[string]bool)
	}
	if s.apps[customerID][name] {
		return false
	}
	s.apps[customerID][name] = true
	return true
}

// Names returns seen entries for a specific customer, sorted. Each
// entry is either "<ns>/<app>" (env-less) or "<ns>/<env>/<app>"
// (env-bearing).
func (s *SeenServices) Names(customerID int64) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cApps := s.apps[customerID]
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
// returned coordinator is ready to serve requests immediately. The
// installed Provisioner is a bare stub with an empty registry and no
// launcher; main.go swaps in the real one once logdb.Start has wired
// the production launcher and persistence layer. Tests live on the
// stub directly and seed it via logdb.SeedShard(c.Provisioner, ...)
// — registry population is the provisioner's concern, not the
// coordinator's.
func NewCoordinator(cfg CoordinatorConfig) *Coordinator {
	// Every query routes through compute — including the zero-stage
	// shard-only case — so there is no meaningful default. Production
	// passes a compute.Client; tests pass a compute.Engine.
	if cfg.Compute == nil {
		panic("coordinator: CoordinatorConfig.Compute is required")
	}
	ingest := cfg.Ingest
	if ingest == nil {
		// The ring buffer has no external deps and the write path
		// always goes through it — there's no scenario where a
		// caller benefits from skipping it. Default keeps test
		// setups terse without losing the buffer; production wires
		// a larger capacity in cmd/coordinator/main.go.
		ingest = NewRingBuffer(1024)
	}
	// A no-op Provisioner gives the coordinator a usable registry
	// surface during the boot window between NewCoordinator and
	// logdb.Start. Tests run on this default — they never start the
	// ticker, so a nil launcher/store is fine. LambdaFunctionURL is
	// the only config field tests routinely set (descriptor-building
	// stamps it onto archived entries); production overwrites the
	// whole Provisioner via logdb.Start anyway.
	prov := logdb.NewProvisioner(logdb.NewShardRegistry(), nil, logdb.ProvisionerConfig{
		LambdaFunctionURL: cfg.LambdaFunctionURL,
	})
	// When a Store is supplied (production, plus tests that exercise
	// the store-backed fallbacks like the rebind-after-restart auth
	// path), wire it through to the stub provisioner. logdb.Start
	// later replaces this provisioner wholesale in production; for
	// the boot window and for tests, this is what gives
	// AuthenticateShard et al. a store to read from.
	if cfg.Store != nil {
		prov.SetStore(cfg.Store)
	}
	c := &Coordinator{
		Compute:                cfg.Compute,
		Provisioner:            prov,
		Tail:                   cfg.Tail,
		Ingest:                 ingest,
		Usage:                  cfg.Usage,
		Store:                  cfg.Store,
		SeenServices:           NewSeenServices(),
		DLQ:                    cfg.DLQ,
		Errors:                 cfg.Errors,
		Notifier:               cfg.Notifier,
		ScriptExecutionTimeout: cfg.ScriptExecutionTimeout,
		ShareURLBase:           cfg.ShareURLBase,
	}
	if cfg.Tail != nil {
		c.TailFanout = NewTailFanout(cfg.Tail, cfg.RedisAddr, cfg.RedisTLS)
	}
	return c
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

				// Group by (customer, namespace, service) for targeted
				// routing; count bytes per customer. The customer must be
				// part of the key — two customers can use the same
				// (namespace, service), but they resolve to different
				// namespace_ids and so route to different shards.
				byKey := make(map[drainKey][]*pb.LogRow)
				for _, row := range batch {
					k := drainKey{row.CustomerId, row.Namespace, row.Service}
					byKey[k] = append(byKey[k], row)
					if c.Usage != nil && row.CustomerId != 0 {
						c.Usage.Record(row.CustomerId, int64(len(row.Raw)))
					}
				}

				for k, rows := range byKey {
					// Route by (namespace_id, service). namespace_id is
					// resolved through a (customer, namespace) cache so the
					// per-batch grouping keeps the DB off the hot path.
					service := k.service
					nsID := c.ingestNamespaceID(k.customerID, k.namespace)
					// Strict service-scoped routing only — no whole-registry
					// fallback, which would commingle a different tenant's
					// rows onto the wrong shard. A service onboarded after
					// the INGESTING shards christened is stamped onto them by
					// the reconciler (OnboardService triggers it on first-seen);
					// rows that arrive before that lands go to DLQ and the
					// scanner re-ingests once a shard serves them.
					sh := c.Provisioner.IngestingShard(nsID, service)
					if sh == nil {
						dlqPath := c.DLQ.Write(rows)
						c.Errors.Record("none", "no shard serves "+k.namespace+"/"+k.service, rows, dlqPath)
						log.Printf("drain: DLQ'd %d rows for %s (no shard serves this service)", len(rows), k.namespace+"/"+k.service)
						continue
					}
					// sh.Ingest reports its own outcome to the provisioner —
					// incrementing the consecutive-failure counter on error,
					// resetting it on success. The returned error names the
					// shard and the running failure count (N/M) for the log
					// line. (Routing exclusion is the FSM's job now, not a
					// per-failure ingest latch.)
					if err := sh.Ingest(rows, c.Provisioner); err != nil {
						log.Printf("drain: %v", err)

						// Retry once. IngestingShard round-robins, so a
						// second call lands on a different candidate if any
						// exist for this service; a recently-suspended
						// primary is excluded automatically. With small
						// per-service candidate sets the two attempts are
						// typically exhaustive.
						if fallback := c.Provisioner.IngestingShard(nsID, service); fallback != nil {
							if retryErr := fallback.Ingest(rows, c.Provisioner); retryErr == nil {
								c.Provisioner.Stats().Record(k.namespace+"/"+k.service, int64(len(rows)))
								continue // retry succeeded
							}
						}

						// All retries exhausted — DLQ.
						dlqPath := c.DLQ.Write(rows)
						c.Errors.Record(sh.ID(), err.Error(), rows, dlqPath)
						log.Printf("drain: DLQ'd %d rows for %s (shard %s failed, no fallback)", len(rows), k.namespace+"/"+k.service, sh.ID())
						continue
					}
					c.Provisioner.Stats().Record(k.namespace+"/"+k.service, int64(len(rows)))
				}
			}
		}
	}()
}

// StartUsageFlush runs a background goroutine that periodically appends
// accumulated usage deltas to PostgreSQL. Flushes every 30 seconds so at
// most 30 seconds of data is lost on restart, and so a customer's
// displayed usage (GetUsage combines this with the not-yet-flushed
// remainder) converges quickly even when their traffic is split across
// multiple coordinator instances — see
// docs/plans/coordinator-horizontal-scaling.md.
func (c *Coordinator) StartUsageFlush(ctx context.Context, store *store.Store) {
	if c.Usage == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
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

// flushUsage appends each drained delta as its own row. Append-only,
// not upsert: usage_hourly has no uniqueness constraint on
// (org_id, hour) (migration 074) precisely so concurrent
// coordinators can each insert their own delta without racing on a
// shared row. Readers (QueryUsage, rollup_usage_daily) already
// SUM(bytes_ingested) across however many rows land for an hour.
func (c *Coordinator) flushUsage(store *store.Store) {
	entries := c.Usage.Drain()
	for _, e := range entries {
		if err := store.DB().Exec(`
			INSERT INTO usage_hourly (org_id, hour, bytes_ingested)
			VALUES (?, ?, ?)`,
			e.CustomerID, e.Hour, e.Bytes).Error; err != nil {
			log.Printf("usage flush error: customer=%d hour=%s: %v", e.CustomerID, e.Hour, err)
		}
	}
	if len(entries) > 0 {
		log.Printf("usage flush: wrote %d hourly entries", len(entries))
	}
}

// IngestingShard returns the current INGESTING shard serving
// (namespaceID, service). Exists for poller.ShardProvider; non-poller
// call sites go through c.Provisioner directly.
func (c *Coordinator) IngestingShard(namespaceID uint, service string) shard.Shard {
	return c.Provisioner.IngestingShard(namespaceID, service)
}

// drainKey groups ingest rows for routing: the customer scopes the
// namespace resolution (two customers can share a composite app), and
// the app is the composite the row carries.
type drainKey struct {
	customerID int64
	namespace  string
	service    string
}

// ingestNamespaceID resolves (customer, namespace) to namespaces.id for
// the ingest hot path, memoizing in nsIDCache so the per-batch drain
// grouping keeps the DB lookup off the per-row path. Only non-zero
// results are cached, so a namespace that doesn't exist yet (race with
// its creation) is retried rather than pinned to 0.
func (c *Coordinator) ingestNamespaceID(customerID int64, namespace string) uint {
	key := strconv.FormatInt(customerID, 10) + "\x00" + namespace
	if v, ok := c.nsIDCache.Load(key); ok {
		return v.(uint)
	}
	id := c.resolveNamespaceID(store.OrgID(customerID), namespace)
	if id != 0 {
		c.nsIDCache.Store(key, id)
	}
	return id
}

// resolveCustomerID maps a customer name to its org id. The empty name is the
// no-customer test shape and resolves to 0 (the planner then emits no customer
// predicate); any other name must exist.
func (c *Coordinator) resolveCustomerID(customer string) (store.OrgID, error) {
	if customer == "" {
		return 0, nil
	}
	cust, err := c.Store.GetOrgByName(customer)
	if err != nil {
		return 0, fmt.Errorf("resolve customer %q: %w", customer, err)
	}
	return cust.ID, nil
}

// resolveNamespaceID maps a (customer id, namespace-name) to namespaces.id
// for service-level shard routing. Returns 0 — the "no namespace
// filter" sentinel ShardDescriptorsFor honors — when the namespace is
// empty (all-namespaces fan-out) or can't be resolved (unknown customer,
// or a namespace the owner deleted). A 0 widens the shard fan-out but
// stays correct: the SQL still carries the namespace/service predicates,
// and the shard's `customer_id = ?` predicate preserves tenant isolation.
func (c *Coordinator) resolveNamespaceID(custID store.OrgID, namespace string) uint {
	if namespace == "" || c.Store == nil || custID == 0 {
		return 0
	}
	ns, err := c.Store.GetNamespace(custID, namespace)
	if err != nil {
		return 0
	}
	return ns.ID
}

// clampTimeRange floors tr.Start at (now − LogRetention(tier)) so that
// the SQL ts predicate the planner emits and the shard-set the registry
// returns share the same lower bound. Without the clamp here, a
// boundary-spanning shard could return rows older than the customer's
// retention because the planner's ts predicate would carry the user's
// raw start time. custID == "" falls back to the trial retention.
//
// The registry's MatchingEntries re-applies this clamp as defense in
// depth — idempotent because we pass the already-clamped value.
func (c *Coordinator) clampTimeRange(tr TimeRange, custID store.OrgID) TimeRange {
	var tier store.Tier
	if custID != 0 && c.Store != nil {
		tier = c.Store.TierForOrg(custID)
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
func (c *Coordinator) Execute(ctx context.Context, pipeline string, tr TimeRange, customer, userID string) (*ExecuteResult, error) {
	return c.execute(ctx, pipeline, tr, customer, userID, nil, nil)
}

// ExecuteWithDir is Execute plus the dirstack courier: dirStack (index
// 0 == cwd) seeds the interpreter's cwd/pushd state before the pipeline
// runs, so a `cd`-bearing line resolves relative paths against the
// caller's current directory. The post-run dirstack is returned on the
// result's FinalDirStack (read after the stream exhausts) for the caller
// to cache. dirStack nil/empty behaves exactly like Execute (cwd "/").
func (c *Coordinator) ExecuteWithDir(ctx context.Context, pipeline string, tr TimeRange, customer, userID string, dirStack []string) (*ExecuteResult, error) {
	return c.execute(ctx, pipeline, tr, customer, userID, nil, dirStack)
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
func (c *Coordinator) ExecuteFollow(ctx context.Context, pipeline string, tr TimeRange, customer, userID string, dirStack []string) (*ExecuteResult, error) {
	if c.TailFanout == nil {
		return nil, fmt.Errorf("ExecuteFollow: coordinator has no tail broker")
	}
	// Plan again here just to extract the live-filter args
	// (FollowApp/FollowPatterns/LiveStages). The historical batch
	// re-plans inside the rewriter callback, so customer + bounds
	// don't need to match exactly — pass empty/zero so this Plan
	// call never produces SQL we'd care about. cwd (index 0 of the
	// courier's stack) resolves a relative source path so the live
	// RowMatcher filters on the right namespace/service/kind — e.g.
	// `tail -f gamestate.log` from /log/vimgym/api tails vimgym/api,
	// not the bogus namespace "gamestate".
	cwd := ""
	if len(dirStack) > 0 {
		cwd = dirStack[0]
	}
	followCust, err := c.resolveCustomerID(customer)
	if err != nil {
		return nil, err
	}
	plan, err := planner.Plan(pipeline, int64(followCust), cwd, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}

	historical, err := c.execute(ctx, pipeline, tr, customer, userID, nil, dirStack)
	if err != nil {
		return nil, err
	}

	matcher := &RowMatcher{
		Customer:  plan.SourceCustomerID,
		Namespace: plan.SourceNamespace,
		Service:   plan.SourceService,
		Kind:      plan.SourceKind,
		Patterns:  plan.FollowPatterns,
	}
	filter := BuildLiveFilter(plan)

	next := func() compute.LineStream {
		id, ch := c.TailFanout.Subscribe(ctx, matcher)
		return &brokerStream{
			ch:     ch,
			filter: filter,
			unsub:  func() { c.TailFanout.Unsubscribe(id, matcher) },
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
func (c *Coordinator) ExecuteForWatch(ctx context.Context, pipeline string, tr TimeRange, watch *store.CronWatch) (*ExecuteResult, error) {
	// c.Notifier is the NotifyDispatcher interface, so a nil-Notifier
	// coordinator threads true-nil into AlertContext (no typed-nil-
	// via-concrete-pointer trap).
	alertCtx := &AlertContext{
		Store:    c.Store,
		Watch:    watch,
		OrgID:    watch.OrgID,
		Notifier: c.Notifier,
	}

	// Fast path: the watch was lowered at save time and the lowered
	// form is still hot. Hydrate args + dispatch directly, bypassing
	// preflight (which hits the DB to resolve the customer name) and
	// Plan (which re-parses + re-walks the pipeline). Phase A profiling
	// pinned customer-resolve + Plan + expandBin at ~15-25% of the
	// per-fire coordinator cost; this is where that saves cleanly.
	if res, ok := c.executeLowered(ctx, watch, tr, alertCtx); ok {
		return res, nil
	}

	// Fall-through: lowered form is missing, stale (version bump or
	// /bin invalidation), or unrenderable. Run the original path and
	// opportunistically re-lower + write back so the next fire is hot.
	cust, err := c.Store.GetOrgByID(watch.OrgID)
	if err != nil {
		return nil, fmt.Errorf("cron %d: resolve org %s: %w", watch.ID, watch.OrgID, err)
	}
	// Refresh runs synchronously rather than via a goroutine. Two
	// reasons: (1) it shares the same DB connection as the rest of
	// the fire, and an async goroutine over a BeginTest-wrapped
	// store thrashes the single conn; (2) on the slow path the
	// fire is already ~1ms, so an extra ~hundred-µs of refresh DB
	// write doesn't move user-perceived latency. The next fire
	// takes the hot path either way.
	c.refreshLoweredSync(watch)
	return c.execute(ctx, pipeline, tr, cust.Name, "", alertCtx, nil)
}

// executeLowered tries the hot-path dispatch using watch.LoweredPlan.
// Returns (nil, false) on any condition that should fall through to
// the slow path: no plan persisted, version mismatch, /bin dep hash
// drift, marshalling error, or follow/alert features the lowered
// path doesn't handle yet. The caller surfaces those as a slow-path
// re-plan + opportunistic re-lowering.
func (c *Coordinator) executeLowered(ctx context.Context, watch *store.CronWatch, tr TimeRange, alertCtx *AlertContext) (*ExecuteResult, bool) {
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
		fn, err := c.Store.GetShellFunction(watch.OrgID, dep.Name)
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
	// the TierForOrg DB hit. A tier upgrade between save and
	// fire stays on the old retention until the watch is re-saved
	// (or another path triggers re-lowering). Acceptable tradeoff
	// for the perf win on the hot path; documented on LoweredPlan.Tier.
	tier := store.Tier(lp.Tier)
	cap := time.Now().Add(-store.LogRetention(tier))
	if !tr.Start.IsZero() && tr.Start.Before(cap) {
		tr.Start = cap
	}
	resolveCtx := planner.ResolveCtx{CustomerID: lp.CustomerID, Start: tr.Start, End: tr.End}
	args := lp.Hydrate(resolveCtx)
	res, err := c.dispatchLoweredWithTier(ctx, lp, args, tr, tier, alertCtx)
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
// than wrong), and skips the per-fire TierForOrg lookup (tier
// is baked into LoweredPlan at save time).
func (c *Coordinator) dispatchLoweredWithTier(ctx context.Context, lp *planner.LoweredPlan, args []any, tr TimeRange, tier store.Tier, alertCtx *AlertContext) (*ExecuteResult, error) {
	nsID := c.resolveNamespaceID(store.OrgID(lp.CustomerID), lp.SourceNamespace)
	descs := c.Provisioner.ShardDescriptorsFor(tr, nsID, lp.SourceService, tier)
	if len(descs) == 0 {
		return &ExecuteResult{Stream: compute.NewSliceLineStream(nil, nil)}, nil
	}
	argStrs := make([]string, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			argStrs[i] = s
		} else {
			argStrs[i] = fmt.Sprintf("%v", a)
		}
	}
	stream, err := c.Compute.ExecuteQuery(ctx, &pb.ExecuteQueryRequest{
		Sql:               lp.DirectShardSQL,
		Args:              argStrs,
		Shards:            descs,
		Stages:            lp.ComputeStages,
		RowStages:         lp.RowStages,
		StagesReadPayload: lp.StagesReadPayload,
		MergeKey:          lp.MergeKey,
		Limit:             int32(lp.Limit),
		KeepLast:          lp.TailMode,
		// TraceTag is never set here: this path only ever serves a
		// cron/watch/dashboard fire (dispatchLoweredWithTier is only
		// reachable via ExecuteForWatch's lowered-plan hot path), never
		// an interactive shell query. Splicing a synthetic summary row
		// and ANSI codes into a saved trace watch's output would corrupt
		// AlertCommand's stdin-line evaluation and pollute dashboard
		// tiles — see docs/plans/request-tracing.md's "interactively-
		// triggered queries only" note.
		TraceTag: false,
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
func (c *Coordinator) refreshLoweredSync(watch *store.CronWatch) {
	if watch.ID == 0 {
		return
	}
	resolver := watchBinResolver{store: c.Store, orgID: watch.OrgID}
	tier := string(c.Store.TierForOrg(watch.OrgID))
	loweredJSON := planner.LowerForStorage(watch.Pipeline, int64(watch.OrgID), tier, resolver)
	if loweredJSON == nil {
		return
	}
	if err := c.Store.DB().WithContext(store.WithoutTenantScope(context.Background())).Model(&store.CronWatch{}).
		Where("id = ?", watch.ID).
		Update("lowered_plan", loweredJSON).
		Error; err != nil {
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
	store *store.Store
	orgID store.OrgID
}

func (r watchBinResolver) Source(name string) (string, string, bool) {
	fn, err := r.store.GetShellFunction(r.orgID, name)
	if err != nil || fn == nil {
		return "", "", false
	}
	return fn.Source, planner.HashBinSource(fn.Source), true
}

// execute is the unified dispatcher. Preflights (resolves customer
// name, expands source-arg globs against SeenServices), peels a
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
func (c *Coordinator) execute(ctx context.Context, pipeline string, tr TimeRange, customer, userID string, alertCtx *AlertContext, dirStack []string) (*ExecuteResult, error) {
	custID, expanded, err := c.preflight(pipeline, customer)
	if err != nil {
		return nil, err
	}

	queryPart, alertArgs := planner.SplitAlertStage(expanded)

	res, err := c.runLocal(ctx, queryPart, tr, customer, custID, userID, alertCtx, dirStack)
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
	// Stderr from the pipeline ahead of the alert means it failed (an
	// unknown source, a bad stage), and its empty stdout is not data:
	// `alert absent` would page on a broken query. Don't evaluate; the
	// error reaches the caller, and a cron run records status "error".
	if drained.Stderr != "" {
		return nil, fmt.Errorf("alert not evaluated, upstream failed: %s", strings.TrimSpace(drained.Stderr))
	}
	if alertCtx == nil {
		alertCtx = &AlertContext{
			Store:       c.Store,
			OrgID:       custID,
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
func (c *Coordinator) runQuery(ctx context.Context, pipeline, cwd string, tr TimeRange, customer string, custID store.OrgID, userID string, alertCtx *AlertContext) (*ExecuteResult, error) {
	// Clamp tr to the customer's tier retention BEFORE planning so the
	// SQL's ts predicates and the shard-selection in MatchingEntries
	// agree on the same start. Without the clamp here, a boundary-
	// spanning shard could return rows older than the customer's
	// retention because the SQL would have no lower-bound predicate.
	tr = c.clampTimeRange(tr, custID)

	// Expand source-arg globs (`cat /log/<ns>/<env>/*.log`) to a
	// concrete multi-arg form the planner can union. Fan-out is scoped
	// to apps observed in this customer's ingest stream (SeenServices),
	// which includes weftd metrics streams that have no configured
	// log_groups row; the namespace allowlist downstream still vets the
	// result. Done here, not in preflight, so a source command that
	// isn't the line's first (`cd X && tail *.log`, a /bin body) expands
	// too. The SeenServices guard supports tests that build a
	// Coordinator without NewCoordinator.
	if custID != 0 && c.SeenServices != nil {
		if apps := c.SeenServices.Names(int64(custID)); len(apps) > 0 {
			expanded, err := planner.ExpandSourceGlobs(pipeline, cwd, apps)
			if err != nil {
				return nil, fmt.Errorf("expand glob: %w", err)
			}
			pipeline = expanded
		}
	}

	plan, err := planner.Plan(pipeline, int64(custID), cwd, tr.Start, tr.End)
	if err != nil {
		// The rewriter only substitutes pipelines whose first command
		// is on /log/..., which the planner accepts. ErrNotPlannable
		// here would mean the pipeline shape changed between
		// substitution and re-render — surface it.
		log.Printf("rewriter callback: plan error: %v (pipeline=%q)", err, pipeline)
		return nil, fmt.Errorf("plan: %w", err)
	}

	execResult, err := c.executeQuery(ctx, plan, tr, customer, custID, userID, alertCtx)
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
			OrgID:       custID,
			Interactive: true,
			Notifier:    c.Notifier,
		}
	}
	runAlertStage(qr, plan.AlertArgs, alertCtx)
	return queryResultToExecuteResult(qr), nil
}

// preflight resolves the customer name to an ID and pre-expands /bin
// bash invocations so the planner can see their body. Source-arg globs
// are expanded later, per query pipeline, in runQuery: this sees only
// the line's first command, which for `cd X && tail *.log` is the cd.
func (c *Coordinator) preflight(pipeline, customer string) (store.OrgID, string, error) {
	custID, err := c.resolveCustomerID(customer)
	if err != nil {
		return 0, "", err
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
	}

	return custID, pipeline, nil
}

// executeQuery runs the query half of a plan (everything except alert).
// customer is the name used for shard routing; custID (0 if unresolved)
// scopes tenant-backed features (ShellFS, weft.*). alertCtx is forwarded
// to runLocal so interior queries dispatched by the interceptor inherit
// the caller's alert identity; may be nil.
func (c *Coordinator) executeQuery(ctx context.Context, plan *planner.QueryPlan, tr TimeRange, customer string, custID store.OrgID, userID string, alertCtx *AlertContext) (*ExecuteResult, error) {
	if plan.ShardSQL == "" {
		return &ExecuteResult{Stream: compute.NewSliceLineStream(nil, nil)}, nil
	}

	// Allowlist: the pipeline's source namespace must resolve to a
	// namespace this customer owns. Gate here, before the name ever
	// reaches SQL interpolation — the planner escapes string literals,
	// but an allowlist is the correct posture for a user-controlled
	// identifier. The planner already split the namespace out of the
	// pseudofile path, so this checks it directly.
	if plan.SourceNamespace != "" {
		var count int64
		if err := c.Store.DB().WithContext(store.WithOrg(context.Background(), custID)).Model(&store.Namespace{}).
			Where("name = ?", plan.SourceNamespace).
			Count(&count).Error; err != nil {
			return nil, fmt.Errorf("validate source: %w", err)
		}
		if count == 0 {
			return nil, fmt.Errorf("unknown source: %s", plan.SourceNamespace)
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
		tier = c.Store.TierForOrg(custID)
	}
	nsID := c.resolveNamespaceID(custID, plan.SourceNamespace)
	descs := c.Provisioner.ShardDescriptorsFor(tr, nsID, plan.SourceService, tier)
	if len(descs) == 0 {
		logQuery("query: no matching shards (namespace=%s, service=%s, customer=%s)", plan.SourceNamespace, plan.SourceService, customer)
		return &ExecuteResult{Stream: compute.NewSliceLineStream(nil, nil)}, nil
	}
	shardIDs := make([]string, len(descs))
	for i, d := range descs {
		shardIDs[i] = d.ShardId
	}
	if traceID, ok := weftsdk.TraceIDFromContext(ctx); ok {
		logQuery("query: namespace=%s service=%s shards=%v compute=%v sql=%s trace_id=%s",
			plan.SourceNamespace, plan.SourceService, shardIDs, plan.ComputeStages, directSQL, traceID)
	} else {
		logQuery("query: namespace=%s service=%s shards=%v compute=%v sql=%s",
			plan.SourceNamespace, plan.SourceService, shardIDs, plan.ComputeStages, directSQL)
	}

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
			logQuery("query: non-string arg at position %d (%T %v) — bound via fmt", i, a, a)
		}
	}
	stream, err := c.Compute.ExecuteQuery(ctx, &pb.ExecuteQueryRequest{
		Sql:               directSQL,
		Args:              argStrs,
		Shards:            descs,
		Stages:            plan.ComputeStages,
		RowStages:         plan.RowStages,
		StagesReadPayload: plan.StagesReadPayload,
		MergeKey:          plan.ShardOrderBy,
		Limit:             int32(plan.Limit),
		// tail-mode: shard SQL wraps to ASC; compute must retain
		// last-N instead of stopping after first-N emitted.
		KeepLast: plan.TailMode,
		// Summary line + colored [namespace/service] badges only for a
		// bare, interactively-typed `trace <id>` or `trace spans
		// [name]` — never for a saved cron/watch/dashboard fire
		// (alertCtx != nil) reached through this same shared path, and
		// never once the user has piped into their own awk/jq/grep
		// stage (they already have $SERVICE/$NAMESPACE available there
		// — see planner.go's includeMetaVars forcing for trace).
		// trace_spans reuses this exact buffer-then-emit path (see
		// docs/plans/span-display.md): the merged "raw" rows are
		// already weft-go's own "span done"/"span error" text, which
		// theme.RenderSpanRollup parses identically for both phases.
		TraceTag: alertCtx == nil && (plan.SourceCmd == "trace" || plan.SourceCmd == "trace_spans") && len(plan.ComputeStages) == 0,
	})
	if err != nil {
		logQuery("query: compute error: %v", err)
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
func (c *Coordinator) runLocal(ctx context.Context, pipeline string, tr TimeRange, customer string, custID store.OrgID, userID string, alertCtx *AlertContext, dirStack []string) (*ExecuteResult, error) {
	ast, err := bash.Parse(pipeline)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	interp := bash.NewInterpreter()
	interp.SetStdin(strings.NewReader(""))
	// Dirstack courier (in): seed the interpreter's cwd/pushd state from
	// the caller's cached dirstack so a `cd`-bearing line resolves
	// relative paths against the client's current directory. Empty means
	// "fresh shell" — the interpreter's own default (cwd "/") stands.
	seedDirStack(interp.DirStack(), dirStack)
	// Highlight server-side `cat` in weft's branded palette so a
	// delegated client that ships `cat /bin/x` to the exec backend gets
	// back the same weave it paints for a locally-read file. /bin and
	// /cron.d entries self-identify their language via shellfs's LangFS
	// hook, so the palette alone is enough — we deliberately do NOT set
	// SyntaxFallback (the client does, for typed-prompt context): forcing
	// a bash fallback here would highlight extensionless /tmp files too,
	// breaking the raw-content contract (TestCoordinatorQuery_CatTmpReturnsContent).
	// The palette definition is shared via weft/theme so client and
	// server can't drift.
	interp.SetPalette(theme.Palette())

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	interp.SetStdout(stdoutW)
	interp.SetStderr(stderrW)

	bash.RegisterToolCommands(interp)

	noticeQueue := notices.NewQueue(c.Store, userID)
	if custID != 0 {
		tier := c.Store.TierForOrg(custID)
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
		// LogServices feeds LogMount's directory listing. Source it from
		// the same per-customer ingest discovery the /api/fs path uses
		// (SeenServices, ferried out via ListServices), so `ls`/`tree`
		// over /log enumerate the same services the client sees while
		// navigating — without this the slice is empty and /log lists
		// nothing. Guarded for tests that build a Coordinator directly.
		var logServices []string
		if c.SeenServices != nil {
			logServices = c.SeenServices.Names(int64(custID))
		}
		// Wrap the mount in a cwd resolver keyed to the (already seeded)
		// dirstack. pkg/bash's file builtins (ls, cat, grep FILE, …) hand
		// their raw argument straight to the FS — they don't normalise
		// relative paths; that's the embedder's job, exactly as warp's own
		// local-exec seam does. Without the wrapper a bare `ls` (arg ".")
		// or any relative path cleans to "/" and returns the root mount
		// set instead of the cwd's contents. Absolute paths pass through
		// untouched, so the nil-seed callers (cron/watch, cwd "/") are
		// unaffected. interp.DirStack().Cwd is read lazily, so an in-line
		// `cd` within the same pipeline is tracked too.
		interp.SetFS(bash.NewCwdFS(shellfs.New(c.Store, custID, logServices, opts...), interp.DirStack().Cwd, ""))

		wf := &WeftFuncs{Store: c.Store, OrgID: custID}
		RegisterWeftCommands(interp, wf)
		RegisterWeftPredicates(interp, wf)
		interp.LuaExtensions = []luapkg.Extension{wf.LuaExtension()}

		// `git` over /bin, /cron.d and /etc history — a strict subset of git
		// backed by the live and history rows (see witrepo). Same scope
		// as the weft.* helpers; the config carries the acting user.
		RegisterGitCommand(interp, witrepo.Config{
			Store: c.Store, OrgID: custID, UserID: userID,
			Cwd: interp.DirStack().Cwd,
		})

		RegisterClipboardCommand(interp, c.Store, custID, userID, c.ShareURLBase, tr.Start, tr.End)

		rewriteQueryPipelines(ctx, ast, c, tr, customer, custID, userID, alertCtx)
		interp.BinScriptTransform = func(_ string, script *bash.Script) {
			rewriteQueryPipelines(ctx, script, c, tr, customer, custID, userID, alertCtx)
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

		res.Notices = noticeQueue.Drain()
		// Dirstack courier (out): publish the post-run dirstack on the
		// same happens-before edge as Notices (write → pipe close →
		// consumer's post-EOF read), so the caller can cache the cwd the
		// pipeline left behind.
		res.FinalDirStack = interp.DirStack().Stack()
		if ctx.Err() == context.DeadlineExceeded {
			stream.setRunErr(fmt.Errorf("script exceeded %s execution time limit", timeout))
		}
		stdoutW.Close()
		stderrW.Close()
	}()

	return res, nil
}

// seedDirStack rewrites ds to match stack (index 0 == cwd) using only the
// public DirStack interface, so it works against any embedder impl. A nil
// or empty stack is a no-op — the interpreter keeps its default cwd "/".
// Mirrors warp's own dirstackReplace: clear to a single entry, set the
// bottom, then push the rest in reverse so stack[0] ends up current.
func seedDirStack(ds bash.DirStack, stack []string) {
	if len(stack) == 0 {
		return
	}
	ds.Clear()
	_ = ds.Chdir(stack[len(stack)-1])
	for i := len(stack) - 2; i >= 0; i-- {
		_ = ds.Push(stack[i])
	}
}
