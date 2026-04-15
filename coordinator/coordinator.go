package coordinator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/store"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// TimeRange specifies the query time window.
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// QueryResult holds the output of a coordinated query. Either Rows is
// populated (no compute stages) or Text is populated (compute stages ran).
type QueryResult struct {
	Rows   []*pb.LogRow
	Text   string
	Stderr string // alert/error output — rendered in red by the shell
}

// Coordinator routes queries to shards and merges results.
type Coordinator struct {
	Registry    *ShardRegistry
	Customer    string
	Pool        compute.ComputePool // nil = no compute support
	Tail        *TailBroker
	Ingest      *RingBuffer    // write path: IngestStream → buffer → shards
	Provisioner *Provisioner   // shard lifecycle management (nil = no auto-provisioning)
	Usage       *CustomerUsageStats // per-customer byte metering
	Store       *store.Store   // for weft.* functions in /bin/ scripts (nil = disabled)
	CustomerID  uint           // for weft.* customer scoping
	SeenApps    *SeenApps      // distinct app names from ingest stream
	DLQ         *DLQ           // S3 dead-letter queue for failed ingests
	Errors      *IngestionErrorRecorder // async error persistence
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
func NewCoordinator(shards []shard.Shard, customer string, pool compute.ComputePool) *Coordinator {
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
		Customer: customer,
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
						log.Printf("drain: suspending shard %s: %v", sh.ID(), err)
						c.Registry.Suspend(sh.ID())

						// Retry on a different healthy shard.
						fallback := c.Registry.IngestingShard(app)
						if fallback == nil {
							fallback = c.Registry.DefaultIngestingShard()
						}
						if fallback != nil {
							if retryErr := fallback.Ingest(rows); retryErr == nil {
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
// rows are streamed to the compute pool for post-processing.
func (c *Coordinator) Query(pipeline string, tr TimeRange, customerOverride ...string) (*QueryResult, error) {
	plan, err := planner.Plan(pipeline)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	// Local pipeline (no log source) — run through warp directly.
	if plan.Local {
		return c.runLocal(plan.LocalPipeline)
	}

	customer := c.Customer
	if len(customerOverride) > 0 && customerOverride[0] != "" {
		customer = customerOverride[0]
	}
	sql := planner.InjectCustomer(plan.ShardSQL, customer)
	sql = planner.ApplyProjection(sql, plan.Project)

	matching := c.Registry.MatchingShards(tr, plan.FollowApp)
	if len(matching) == 0 {
		return &QueryResult{}, nil
	}

	// Complete projection with no compute stages: the SQL handles everything.
	// Return rows (which have narrowed raw/fields from the projection).
	if plan.Project != nil && plan.Project.Complete && len(plan.ComputeStages) == 0 {
		return c.queryRowsOnly(sql, plan, matching)
	}

	// If there are compute stages and a pool, use the streaming path.
	if len(plan.ComputeStages) > 0 && c.Pool != nil {
		return c.queryWithCompute(sql, plan.ComputeStages, matching)
	}

	// No compute: query shards directly, merge, return rows.
	return c.queryRowsOnly(sql, plan, matching)
}

// queryRowsOnly fans out to all matching shards in parallel, then merges.
func (c *Coordinator) queryRowsOnly(sql string, plan *planner.QueryPlan, shards []shard.Shard) (*QueryResult, error) {
	type shardResult struct {
		rows []*pb.LogRow
		err  error
	}

	results := make([]shardResult, len(shards))
	var wg sync.WaitGroup
	wg.Add(len(shards))
	for i, s := range shards {
		go func(i int, s shard.Shard) {
			defer wg.Done()
			rows, err := s.Query(sql)
			results[i] = shardResult{rows, err}
		}(i, s)
	}
	wg.Wait()

	var allRows []*pb.LogRow
	for i, r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("shard %s: %w", shards[i].ID(), r.err)
		}
		allRows = append(allRows, r.rows...)
	}

	if plan.OrderDesc {
		sort.Slice(allRows, func(i, j int) bool {
			return allRows[i].TsUnixNs > allRows[j].TsUnixNs
		})
	} else {
		sort.Slice(allRows, func(i, j int) bool {
			return allRows[i].TsUnixNs < allRows[j].TsUnixNs
		})
	}

	if plan.Limit > 0 && len(allRows) > plan.Limit {
		allRows = allRows[:plan.Limit]
	}

	return &QueryResult{Rows: allRows}, nil
}

// queryWithCompute creates a compute job, fans out shard queries that stream
// rows directly to the job, and waits for the result.
func (c *Coordinator) queryWithCompute(sql string, stages []string, shards []shard.Shard) (*QueryResult, error) {
	job := c.Pool.NewJob(stages, len(shards))

	for _, s := range shards {
		go s.QueryToSink(sql, job)
	}

	text, err := job.Result()
	if err != nil {
		return nil, err
	}

	return &QueryResult{Text: text}, nil
}

// runLocal executes a sourceless pipeline (no log query) through warp locally.
// Registers alert and other builtins so `echo "42" | alert above 0` works.
func (c *Coordinator) runLocal(pipeline string) (*QueryResult, error) {
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

	// Register alert for interactive use (no watch context, no history).
	interp.RegisterCommand("alert", func(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
		return AlertCommand(args, stdin, stdout, stderr, &AlertContext{
			Interactive: true,
		})
	})

	// Register weft.* functions for cron result access.
	if c.Store != nil && c.CustomerID != 0 {
		wf := &WeftFuncs{Store: c.Store, CustomerID: c.CustomerID}
		RegisterWeftCommands(interp, wf)
		interp.LuaWeftFuncs = wf
	}

	interp.ExecScript(ast)

	return &QueryResult{
		Text:   outBuf.String(),
		Stderr: errBuf.String(),
	}, nil
}

