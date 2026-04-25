package coordinator

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/shellfs"
	"github.com/kevlarcon/weft/store"
)

// TimeRange specifies the query time window.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// QueryResult holds the output of a coordinated query. Either Rows is
// populated (no compute stages) or Text is populated (compute stages ran).
type QueryResult struct {
	Rows    []*pb.ResultRow
	Text    string
	Stderr  string   // alert/error output — rendered in red by the shell
	Notices []Notice // one-time per-user notices to surface in the response
}

// Coordinator routes queries to shards and merges results. It holds no
// customer identity of its own — tenant scope is resolved per-request
// from the customer name passed to Query / QueryForWatch, which then
// propagates to ShellFS, AlertContext, and /bin script resolution.
type Coordinator struct {
	Registry    *ShardRegistry
	Pool        compute.ComputePool // nil = no compute support
	Tail        *TailBroker
	Ingest      *RingBuffer    // write path: IngestStream → buffer → shards
	Provisioner *Provisioner   // shard lifecycle management (nil = no auto-provisioning)
	Usage       *CustomerUsageStats // per-customer byte metering
	Store       *store.Store   // nil = no tenant-scoped features (/bin, alert dispatch)
	SeenApps    *SeenApps      // distinct app names from ingest stream
	DLQ         *DLQ           // S3 dead-letter queue for failed ingests
	Errors      *IngestionErrorRecorder // async error persistence
	Notifier    *Notifier      // per-customer alert dispatcher (nil = disabled)
	// ScriptExecutionTimeout caps how long a single runLocal invocation
	// may run. Zero = use the package-level default (scriptExecutionTimeout).
	// Tests override to shorten the wait.
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

// NewCoordinator creates a coordinator with the given initial shard list.
// Shards are registered with empty Apps (legacy: matches all apps).
// Callers are responsible for setting Store, Notifier, and other
// tenant-scoped dependencies before the coordinator serves requests.
func NewCoordinator(shards []shard.Shard, pool compute.ComputePool) *Coordinator {
	reg := NewShardRegistry()
	for _, s := range shards {
		reg.Add(&ShardEntry{
			ID:        s.ID(),
			State:     StateIngesting,
			Shard:     s,
			CreatedAt: time.Now(),
		})
	}
	return &Coordinator{
		Registry: reg,
		Pool:     pool,
		SeenApps: NewSeenApps(),
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

// SetShards replaces the shard list via the registry. Used by ASG re-discovery.
// Entries are created with empty Apps (legacy: matches all apps).
func (c *Coordinator) SetShards(shards []shard.Shard) {
	// Build set of new IDs.
	newIDs := make(map[string]bool)
	for _, s := range shards {
		newIDs[s.ID()] = true
	}

	// Collect old shards to close after updating.
	var toClose []shard.Shard
	for _, e := range c.Registry.Entries() {
		if !newIDs[e.ID] {
			if e.Shard != nil {
				toClose = append(toClose, e.Shard)
			}
			c.Registry.Remove(e.ID)
		}
	}

	// Add/update new entries.
	for _, s := range shards {
		if existing := c.Registry.Get(s.ID()); existing != nil {
			old := existing.Shard
			existing.Shard = s
			if old != nil && old != s {
				toClose = append(toClose, old)
			}
			continue
		}
		c.Registry.Add(&ShardEntry{
			ID:        s.ID(),
			State:     StateIngesting,
			Shard:     s,
			CreatedAt: time.Now(),
		})
	}

	// Note: old shard connections are not closed here. In-flight queries
	// may still hold references. For gRPC shards, the connection will be
	// garbage collected. For in-process DuckDB shards (tests), the test's
	// t.Cleanup or defer handles closure.

	log.Printf("coordinator: shard list updated (%d shards)", c.Registry.Count())
}

// GetShards returns all live shard connections.
func (c *Coordinator) GetShards() []shard.Shard {
	return c.Registry.AllShards()
}

// IngestingShard returns the current INGESTING shard for the given app.
func (c *Coordinator) IngestingShard(app string) shard.Shard {
	return c.Registry.IngestingShard(app)
}

// Query parses a pipeline, routes the pushed-down SQL to matching shards,
// and returns results. If compute stages exist and a Pool is set, shard
// rows are streamed to the compute pool for post-processing. A trailing
// `| alert ...` stage, if present, runs in interactive AlertContext after
// the query; cron uses QueryForWatch to bind the alert to a watch.
//
// userID identifies the human who invoked the pipeline — used to attribute
// any /bin /cron.d /tmp writes the pipeline performs. Empty string means
// "no attribution" (system invocation, automated path).
func (c *Coordinator) Query(pipeline string, tr TimeRange, customer, userID string) (*QueryResult, error) {
	return c.query(pipeline, tr, customer, userID, nil)
}

// QueryForWatch runs pipeline with the supplied watch bound to the alert
// stage (if any), so incidents and dispatches use watch identity instead
// of interactive-mode defaults. Watches run as the system, not as a user
// — any writes the watch performs land with NULL creator.
func (c *Coordinator) QueryForWatch(pipeline string, tr TimeRange, watch *store.CronWatch) (*QueryResult, error) {
	ctx := &AlertContext{
		Store:      c.Store,
		Watch:      watch,
		CustomerID: watch.CustomerID,
	}
	if c.Notifier != nil {
		ctx.Notifier = c.Notifier
	}
	// The watch carries its own CustomerID so we can resolve the name
	// here without a second Store round-trip. Used by the /bin
	// pre-expand to build a tenant-scoped ShellFS.
	var custName string
	if c.Store != nil {
		if cust, err := c.Store.GetCustomerByID(watch.CustomerID); err == nil {
			custName = cust.Name
		}
	}
	return c.query(pipeline, tr, custName, "", ctx)
}

// query is the shared implementation for Query and QueryForWatch.
// alertCtx = nil means "use interactive defaults if the pipeline has an
// alert stage." A non-nil alertCtx binds the alert to that context.
func (c *Coordinator) query(pipeline string, tr TimeRange, customer, userID string, alertCtx *AlertContext) (*QueryResult, error) {
	// Resolve the customer name to an ID once. A non-zero result gates
	// everything tenant-scoped downstream: /bin pre-expand, ShellFS
	// wiring in runLocal, the interactive AlertContext. Failures here
	// just mean the request runs without those features (no /bin, no
	// alert dispatch) — queries against shards still work via the
	// customer NAME that shard routing already uses.
	var custID uint
	if c.Store != nil && customer != "" {
		if cust, err := c.Store.GetCustomerByName(customer); err == nil {
			custID = cust.ID
		}
	}

	// Pre-expand /bin bash invocations so the planner can see their body
	// and push down `cat /log/...` stages. Lua and complex bodies pass
	// through untouched; warp resolves them at execution time.
	if custID != 0 {
		fs := shellfs.New(c.Store, custID, nil)
		// Reject `tail -f ... | <bin-script>` before inlining. Bash bodies
		// get spliced by expandBinPipeline below, so if we waited until
		// after expansion, a follow pipeline calling a bash /bin script
		// would slip through looking like a regular pipeline (and Lua
		// scripts would pass silently because they're opaque). Failing
		// here keeps the rule uniform and the error pointed at the
		// right stage name.
		if err := rejectBinInFollow(pipeline, fs); err != nil {
			return nil, err
		}
		if expanded, err := expandBinPipeline(pipeline, fs); err != nil {
			return nil, fmt.Errorf("expand /bin: %w", err)
		} else {
			pipeline = expanded
		}
	}

	plan, err := planner.Plan(pipeline)
	if err != nil {
		log.Printf("query: plan error: %v (pipeline=%q)", err, pipeline)
		return nil, fmt.Errorf("plan: %w", err)
	}

	result, err := c.executeQuery(plan, tr, customer, custID, userID, alertCtx)
	if err != nil {
		return nil, err
	}

	if plan.AlertArgs != nil {
		if alertCtx == nil {
			alertCtx = &AlertContext{
				Store:       c.Store,
				CustomerID:  custID,
				Interactive: true,
			}
			// Guard: assigning a nil *Notifier to the NotifyDispatcher
			// interface field would wrap a typed-nil and defeat the
			// `ctx.Notifier != nil` guard inside evaluateAlertRow.
			if c.Notifier != nil {
				alertCtx.Notifier = c.Notifier
			}
		}
		runAlertStage(result, plan.AlertArgs, alertCtx)
	}

	return result, nil
}

// executeQuery runs the query half of a plan (everything except alert).
// customer is the name used for shard routing; custID (0 if unresolved)
// scopes tenant-backed features (ShellFS, weft.*). alertCtx is forwarded
// to runLocal so interior queries dispatched by the interceptor inherit
// the caller's alert identity; may be nil.
func (c *Coordinator) executeQuery(plan *planner.QueryPlan, tr TimeRange, customer string, custID uint, userID string, alertCtx *AlertContext) (*QueryResult, error) {
	if plan.Local {
		if plan.LocalPipeline == "" {
			return &QueryResult{}, nil
		}
		return c.runLocal(plan.LocalPipeline, tr, customer, custID, userID, alertCtx)
	}

	// Allowlist: the pipeline's source app (the part before '/') must
	// resolve to a namespace this customer owns. Gate here, before the
	// app name ever reaches SQL interpolation — the planner escapes
	// string literals, but an allowlist is the correct posture for a
	// user-controlled identifier.
	if plan.SourceApp != "" && c.Store != nil {
		cust, err := c.Store.GetCustomerByName(customer)
		if err != nil || cust == nil {
			return nil, fmt.Errorf("unknown customer")
		}
		ok, err := c.Store.CustomerHasSource(cust.ID, plan.SourceApp)
		if err != nil {
			return nil, fmt.Errorf("validate source: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("unknown source: %s", plan.SourceApp)
		}
	}

	// For the compute-stage branch we use plan.DirectShardSQL so each
	// shard emits rows in the order compute's k-way merge expects
	// (see docs/plans/direct-to-compute.md §3). For the no-compute-
	// stage path the legacy ShardSQL keeps the old ts-ordered
	// behavior — callers that iterate rows directly don't need the
	// merge ordering.
	sql := planner.InjectCustomer(plan.ShardSQL, customer)
	directSQL := planner.InjectCustomer(plan.DirectShardSQL, customer)

	// Shard selection honors plan-tier retention: tr.Start is clamped
	// up to (now − LogRetention(tier)) inside MatchingShards, so a
	// customer never reaches a shard whose data is entirely past their
	// advertised retention. Unresolved customer (custID==0) yields tier
	// "" which conservatively falls back to the trial retention. A nil
	// Store means we're in a test/dev context with no subscription
	// table at all — fall back to the most permissive tier so tests
	// whose shards live in the distant past keep querying as before.
	var tier store.Tier
	switch {
	case c.Store == nil:
		tier = store.TierBolt
	case custID != 0:
		tier = c.Store.TierForCustomer(custID)
	}
	matching := c.Registry.MatchingShards(tr, plan.FollowApp, tier)
	if len(matching) == 0 {
		log.Printf("query: no matching shards (app=%s, customer=%s)", plan.SourceApp, customer)
		return &QueryResult{}, nil
	}

	shardIDs := make([]string, len(matching))
	for i, s := range matching {
		shardIDs[i] = s.ID()
	}
	log.Printf("query: app=%s shards=%v compute=%v sql=%s",
		plan.SourceApp, shardIDs, plan.ComputeStages, sql)

	// Direct-to-compute branch: shards stream rows straight into the
	// compute job (no coordinator-side buffer). Each shard's SQL is
	// ORDER BY'd to match plan.ShardOrderBy so compute's k-way merge
	// can advance a single heap head without re-sorting. See
	// docs/plans/direct-to-compute.md §§4-5.
	if len(plan.ComputeStages) > 0 && c.Pool != nil {
		job := c.Pool.NewJob(plan.ComputeStages, len(matching), plan.ShardOrderBy)

		var wg sync.WaitGroup
		wg.Add(len(matching))
		errs := make(chan error, len(matching))
		for _, s := range matching {
			go func(s shard.Shard) {
				defer wg.Done()
				defer job.SendSentinel(s.ID())
				rows, err := s.Query(directSQL)
				if err != nil {
					errs <- fmt.Errorf("shard %s: %w", s.ID(), err)
					return
				}
				for _, r := range rows {
					job.SendRow(r, s.ID())
				}
			}(s)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			log.Printf("query: %v", e)
			return nil, e
		}

		text, err := job.Result()
		if err != nil {
			log.Printf("query: compute error: %v (stages=%v)", err, plan.ComputeStages)
			return nil, err
		}
		log.Printf("query: compute complete, output=%d bytes (stages=%v)", len(text), plan.ComputeStages)
		return &QueryResult{Text: text}, nil
	}

	// No compute stages — fall back to fetch-and-return rows. This
	// path stays on ShardSQL (legacy ts-ordered SQL) because plan.
	// OrderDesc + plan.Limit still describe the expected post-merge
	// shape for callers that iterate rows directly.
	type shardResult struct {
		rows []*pb.ResultRow
		err  error
	}
	results := make([]shardResult, len(matching))
	var wg sync.WaitGroup
	wg.Add(len(matching))
	for i, s := range matching {
		go func(i int, s shard.Shard) {
			defer wg.Done()
			rows, err := s.Query(sql)
			results[i] = shardResult{rows, err}
		}(i, s)
	}
	wg.Wait()

	var allRows []*pb.ResultRow
	for i, r := range results {
		if r.err != nil {
			log.Printf("query: shard %s returned error: %v", matching[i].ID(), r.err)
			return nil, fmt.Errorf("shard %s: %w", matching[i].ID(), r.err)
		}
		allRows = append(allRows, r.rows...)
	}
	log.Printf("query: %d shards returned %d total rows", len(matching), len(allRows))

	if plan.OrderDesc {
		sort.Slice(allRows, func(i, j int) bool {
			return allRows[i].LineNum > allRows[j].LineNum
		})
	} else {
		sort.Slice(allRows, func(i, j int) bool {
			return allRows[i].LineNum < allRows[j].LineNum
		})
	}

	if plan.Limit > 0 && len(allRows) > plan.Limit {
		allRows = allRows[:plan.Limit]
	}

	return &QueryResult{Rows: allRows}, nil
}

// runAlertStage feeds a QueryResult's text (or joined raw rows) into
// AlertCommand and merges its stderr into result.Stderr so the caller
// sees the alert message. AlertCommand's stdout is a tee of stdin and
// is discarded — the original result.Text/Rows already carry that.
func runAlertStage(result *QueryResult, args []string, ctx *AlertContext) {
	stdin := result.Text
	if stdin == "" && len(result.Rows) > 0 {
		var sb strings.Builder
		for _, r := range result.Rows {
			sb.WriteString(r.Raw)
			sb.WriteByte('\n')
		}
		stdin = sb.String()
	}
	var out, errOut strings.Builder
	AlertCommand(args, strings.NewReader(stdin), &out, &errOut, ctx)
	if errOut.Len() == 0 {
		return
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

// runLocal executes a sourceless pipeline (no top-level log query) through
// warp locally. The planner strips any trailing `alert` before this point,
// so the interpreter doesn't need to register it. custID (non-zero) scopes
// the ShellFS and weft.* functions to one tenant; zero disables both.
//
// tr/customer/alertCtx are captured by BinScriptTransform so that interior
// query pipelines inside /bin scripts — substituted at load time with
// CallbackCommand nodes — inherit the caller's time window, customer
// scope, and alert identity. Tests that don't exercise interior queries
// can pass zero values.
//
// Interior query pipelines are turned into callbacks by rewriteQueryPipelines,
// invoked at two points:
//   1. On the top-level AST here, covering query pipelines the caller
//      typed directly.
//   2. Via interp.BinScriptTransform, on each /bin/<name> script body
//      as warp parses it at invocation time — that's where the
//      interesting cases (loops, command substitutions, function
//      bodies) live, because /bin scripts are how users compose
//      multi-line / multi-pipeline logic.
//
// Both paths call the same rewriter, so the AST that warp executes has
// no pipelines left that need weft-side dispatching at runtime.
//
// Execution is capped at scriptExecutionTimeout. When the cap fires the
// interpreter is cancelled cooperatively; warp stops at the next AST
// boundary (between top-level lists or loop iterations). Whatever the
// script produced before the cut-off is returned along with a timeout
// error.
func (c *Coordinator) runLocal(pipeline string, tr TimeRange, customer string, custID uint, userID string, alertCtx *AlertContext) (*QueryResult, error) {
	ast, err := bash.Parse(pipeline)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	interp := bash.NewInterpreter()
	interp.SetStdin(strings.NewReader(""))

	var outBuf, errBuf bytes.Buffer
	interp.SetStdout(&outBuf)
	interp.SetStderr(&errBuf)

	bash.RegisterToolCommands(interp)

	noticeQueue := NewNoticeQueue(c.Store, userID)
	if c.Store != nil && custID != 0 {
		tier := c.Store.TierForCustomer(custID)
		opts := []shellfs.Option{
			shellfs.WithTier(tier),
			shellfs.WithCustomerName(customer),
			shellfs.WithNoticeQueue(noticeQueue),
		}
		if userID != "" {
			opts = append(opts, shellfs.WithUser(userID, false))
		}
		interp.SetFS(shellfs.New(c.Store, custID, nil, opts...))

		wf := &WeftFuncs{Store: c.Store, CustomerID: custID}
		RegisterWeftCommands(interp, wf)
		interp.LuaWeftFuncs = wf

		rewriteQueryPipelines(ast, c, tr, customer, userID, alertCtx)
		interp.BinScriptTransform = func(_ string, script *bash.Script) {
			rewriteQueryPipelines(script, c, tr, customer, userID, alertCtx)
		}
	}

	// Run the interpreter under a hard time cap. The watcher goroutine
	// signals cooperative cancellation; warp observes this at its next
	// list/loop boundary and winds down. If the script completes in
	// time the deferred cancel() makes the watcher exit cleanly.
	timeout := c.ScriptExecutionTimeout
	if timeout <= 0 {
		timeout = scriptExecutionTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		interp.ExecScript(ast)
		close(done)
	}()

	select {
	case <-done:
		return &QueryResult{
			Text:    outBuf.String(),
			Stderr:  errBuf.String(),
			Notices: noticeQueue.Flush(),
		}, nil
	case <-ctx.Done():
		interp.Cancel()
		// Give the interpreter a brief grace period to stop at the
		// next AST boundary before we return. Anything that's busy in
		// an unresponsive path (a shard query mid-flight, a
		// runaway awk program past AwkMaxSteps safeguards) will still
		// be running in the background goroutine when we return; the
		// caller sees the timeout promptly.
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			log.Printf("runLocal: interpreter did not halt within grace period after %s timeout", timeout)
		}
		return &QueryResult{
			Text:    outBuf.String(),
			Stderr:  errBuf.String(),
			Notices: noticeQueue.Flush(),
		}, fmt.Errorf("script exceeded %s execution time limit", timeout)
	}
}

