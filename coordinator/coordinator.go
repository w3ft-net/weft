package coordinator

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/kevlarcon/weft/compute"
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
	Rows []*pb.LogRow
	Text string
}

// Coordinator routes queries to shards and merges results.
type Coordinator struct {
	Registry *ShardRegistry
	Customer string
	Pool     compute.ComputePool // nil = no compute support
	Tail     *TailBroker
	Ingest   *RingBuffer // write path: IngestStream → buffer → shards
}

// NewCoordinator creates a coordinator with the given initial shard list.
// Shards are registered with empty Apps (legacy: matches all apps).
func NewCoordinator(shards []shard.Shard, customer string, pool compute.ComputePool) *Coordinator {
	reg := NewShardRegistry()
	for _, s := range shards {
		reg.Add(&ShardEntry{
			ID:          s.ID(),
			WindowStart: s.WindowStart(),
			WindowEnd:   s.WindowEnd(),
			State:       StateIngesting,
			Shard:       s,
			CreatedAt:   time.Now(),
		})
	}
	return &Coordinator{
		Registry: reg,
		Customer: customer,
		Pool:     pool,
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

				// Group by app for targeted routing.
				byApp := make(map[string][]*pb.LogRow)
				for _, row := range batch {
					byApp[row.App] = append(byApp[row.App], row)
				}

				for app, rows := range byApp {
					sh := c.Registry.IngestingShard(app)
					if sh == nil {
						sh = c.Registry.DefaultIngestingShard()
					}
					if sh != nil {
						sh.Ingest(rows)
					}
				}
			}
		}
	}()
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
			ID:          s.ID(),
			WindowStart: s.WindowStart(),
			WindowEnd:   s.WindowEnd(),
			State:       StateIngesting,
			Shard:       s,
			CreatedAt:   time.Now(),
		})
	}

	// Note: old shard connections are not closed here. In-flight queries
	// may still hold references. For gRPC shards, the connection will be
	// garbage collected. For in-process DuckDB shards (tests), the test's
	// t.Cleanup or defer handles closure.

	log.Printf("coordinator: shard list updated (%d shards)", c.Registry.Count())
}

// GetShards returns all live shard connections. Used by the poller and
// other code that hasn't been migrated to the registry yet.
func (c *Coordinator) GetShards() []shard.Shard {
	return c.Registry.AllShards()
}

// Query parses a pipeline, routes the pushed-down SQL to matching shards,
// and returns results. If compute stages exist and a Pool is set, shard
// rows are streamed to the compute pool for post-processing.
func (c *Coordinator) Query(pipeline string, tr TimeRange) (*QueryResult, error) {
	plan, err := planner.Plan(pipeline)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	sql := planner.InjectCustomer(plan.ShardSQL, c.Customer)
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

