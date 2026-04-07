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
	mu       sync.RWMutex
	shards   []shard.Shard
	Customer string
	Pool     compute.ComputePool // nil = no compute support
	Tail     *TailBroker
	Ingest   *RingBuffer // write path: IngestStream → buffer → shards
}

// NewCoordinator creates a coordinator with the given initial shard list.
func NewCoordinator(shards []shard.Shard, customer string, pool compute.ComputePool) *Coordinator {
	return &Coordinator{
		shards:   shards,
		Customer: customer,
		Pool:     pool,
	}
}

// StartDrain runs a background goroutine that drains the ring buffer
// into shards. Call this after setting up shards and the ring buffer.
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
				for _, sh := range c.GetShards() {
					if err := sh.Ingest(batch); err != nil {
						continue
					}
					break
				}
			}
		}
	}()
}

// SetShards replaces the coordinator's shard list. Thread-safe.
func (c *Coordinator) SetShards(shards []shard.Shard) {
	c.mu.Lock()
	old := c.shards
	c.shards = shards
	c.mu.Unlock()

	// Close old connections that are no longer in the new list.
	newAddrs := make(map[string]bool)
	for _, s := range shards {
		newAddrs[s.ID()] = true
	}
	for _, s := range old {
		if !newAddrs[s.ID()] {
			s.Close()
		}
	}

	log.Printf("coordinator: shard list updated (%d shards)", len(shards))
}

// Shards returns a snapshot of the current shard list. Thread-safe.
func (c *Coordinator) GetShards() []shard.Shard {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]shard.Shard, len(c.shards))
	copy(out, c.shards)
	return out
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

	matching := c.matchingShards(tr)
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

// matchingShards returns shards whose time windows overlap the given range.
func (c *Coordinator) matchingShards(tr TimeRange) []shard.Shard {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []shard.Shard
	for _, s := range c.shards {
		if s.WindowStart().Before(tr.End) && s.WindowEnd().After(tr.Start) {
			out = append(out, s)
		}
	}
	return out
}
