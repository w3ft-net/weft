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
	Rows   []*pb.ResultRow
	Text   string
	Stderr string // alert/error output — rendered in red by the shell
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
}

// SeenApps tracks distinct app names observed during ingestion, keyed by customer.
type SeenApps struct {
	mu   sync.RWMutex
	apps map[string]map[string]bool // customer → set of app names
}

func NewSeenApps() *SeenApps {
	return &SeenApps{apps: make(map[string]map[string]bool)}
}

// Add records an app name for a customer. Returns true if it was new.
func (s *SeenApps) Add(customer, app string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apps[customer] == nil {
		s.apps[customer] = make(map[string]bool)
	}
	if s.apps[customer][app] {
		return false
	}
	s.apps[customer][app] = true
	return true
}

// Names returns seen app names for a specific customer, sorted.
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
func (c *Coordinator) Query(pipeline string, tr TimeRange, customer string) (*QueryResult, error) {
	return c.query(pipeline, tr, customer, nil)
}

// QueryForWatch runs pipeline with the supplied watch bound to the alert
// stage (if any), so incidents and dispatches use watch identity instead
// of interactive-mode defaults.
func (c *Coordinator) QueryForWatch(pipeline string, tr TimeRange, watch *store.CronWatch) (*QueryResult, error) {
	ctx := &AlertContext{
		Store:      c.Store,
		Watch:      watch,
		Notifier:   c.Notifier,
		CustomerID: watch.CustomerID,
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
	return c.query(pipeline, tr, custName, ctx)
}

// query is the shared implementation for Query and QueryForWatch.
// alertCtx = nil means "use interactive defaults if the pipeline has an
// alert stage." A non-nil alertCtx binds the alert to that context.
func (c *Coordinator) query(pipeline string, tr TimeRange, customer string, alertCtx *AlertContext) (*QueryResult, error) {
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

	result, err := c.executeQuery(plan, tr, customer, custID)
	if err != nil {
		return nil, err
	}

	if plan.AlertArgs != nil {
		if alertCtx == nil {
			alertCtx = &AlertContext{
				Store:       c.Store,
				Notifier:    c.Notifier,
				CustomerID:  custID,
				Interactive: true,
			}
		}
		runAlertStage(result, plan.AlertArgs, alertCtx)
	}

	return result, nil
}

// executeQuery runs the query half of a plan (everything except alert).
// customer is the name used for shard routing; custID (0 if unresolved)
// scopes tenant-backed features (ShellFS, weft.*).
func (c *Coordinator) executeQuery(plan *planner.QueryPlan, tr TimeRange, customer string, custID uint) (*QueryResult, error) {
	if plan.Local {
		if plan.LocalPipeline == "" {
			return &QueryResult{}, nil
		}
		return c.runLocal(plan.LocalPipeline, custID)
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

	sql := planner.InjectCustomer(plan.ShardSQL, customer)

	matching := c.Registry.MatchingShards(tr, plan.FollowApp)
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

	// Fan out query to all matching shards in parallel.
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

	// Sort by line_num for deterministic merge ordering.
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

	// If compute stages exist, pipe rows through the compute pool.
	if len(plan.ComputeStages) > 0 && c.Pool != nil {
		job := c.Pool.NewJob(plan.ComputeStages, 1) // already merged
		for _, r := range allRows {
			job.SendRow(r, "merged")
		}
		job.SendSentinel("merged")

		text, err := job.Result()
		if err != nil {
			log.Printf("query: compute error: %v (stages=%v)", err, plan.ComputeStages)
			return nil, err
		}
		log.Printf("query: compute complete, output=%d bytes (stages=%v)", len(text), plan.ComputeStages)
		return &QueryResult{Text: text}, nil
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

// runLocal executes a sourceless pipeline (no log query) through warp locally.
// The planner strips any trailing `alert` before this point, so the
// interpreter doesn't need to register it. custID (non-zero) scopes the
// ShellFS and weft.* functions to one tenant; zero disables both.
func (c *Coordinator) runLocal(pipeline string, custID uint) (*QueryResult, error) {
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

	// Wire a tenant-scoped ShellFS so /bin scripts resolve server-side for
	// both bash (PATH resolution) and lua (LangFS-aware dispatch).
	if c.Store != nil && custID != 0 {
		interp.SetFS(shellfs.New(c.Store, custID, nil))

		wf := &WeftFuncs{Store: c.Store, CustomerID: custID}
		RegisterWeftCommands(interp, wf)
		interp.LuaWeftFuncs = wf
	}

	interp.ExecScript(ast)

	return &QueryResult{
		Text:   outBuf.String(),
		Stderr: errBuf.String(),
	}, nil
}

