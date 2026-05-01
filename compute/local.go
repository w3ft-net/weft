package compute

import (
	"fmt"
	"strings"
	"sync"

	"github.com/kevlarcon/warp/pkg/bash"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// LocalComputePool executes compute stages in-process using warp's bash
// interpreter with chunked parallelism.
type LocalComputePool struct {
	Parallelism int // goroutine count for bash.RunWithParallelism (0 = streaming)
}

func (p *LocalComputePool) NewJob(stages []string, shardCount int, mergeKey string, limit int) ComputeJob {
	return &LocalComputeJob{
		stages:      stages,
		shardCount:  shardCount,
		mergeKey:    mergeKey,
		limit:       limit,
		parallelism: p.Parallelism,
		ch:          make(chan ComputeMessage, 1024),
		done:        make(chan jobResult, 1),
	}
}

type jobResult struct {
	text string
	rows []*pb.ResultRow // populated for zero-stage jobs; nil otherwise
	err  error
}

// LocalComputeJob collects rows from shards via a channel, then executes the
// pipeline once all sentinels have been received.
type LocalComputeJob struct {
	stages      []string
	shardCount  int
	mergeKey    string // see ComputePool.NewJob; empty = concat, otherwise k-way merge
	limit       int    // see ComputePool.NewJob; 0 = no cap
	parallelism int
	ch          chan ComputeMessage
	done        chan jobResult
	once        sync.Once
	resultOnce  sync.Once
	result      jobResult
}

func (j *LocalComputeJob) ensureStarted() {
	j.once.Do(func() { go j.consume() })
}

func (j *LocalComputeJob) Send(msg ComputeMessage) {
	j.ensureStarted()
	j.ch <- msg
}

// SendRow implements shard.RowSink.
func (j *LocalComputeJob) SendRow(row *pb.ResultRow, shardID string) {
	j.Send(ComputeMessage{Row: row, ShardID: shardID})
}

// SendSentinel implements shard.RowSink.
func (j *LocalComputeJob) SendSentinel(shardID string) {
	j.Send(ComputeMessage{ShardID: shardID, Sentinel: true})
}

// readResult blocks until consume() finishes, then caches the result
// so subsequent Result/Stream/MergedRows calls return immediately. The
// done channel is buffered with size 1; without caching, the second
// reader would block forever.
func (j *LocalComputeJob) readResult() jobResult {
	j.ensureStarted()
	j.resultOnce.Do(func() {
		j.result = <-j.done
	})
	return j.result
}

func (j *LocalComputeJob) Result() (string, error) {
	r := j.readResult()
	return r.text, r.err
}

// Stream returns the job's output as a forward-only line stream. The
// underlying merge + pipeline still runs as a single batch (Result is
// invoked once on first Next); the LineStream surface is in place so
// callers (coordinator → gRPC server) can be migrated against it
// before the producer side is converted to true streaming.
func (j *LocalComputeJob) Stream() LineStream {
	return newBufferedLineStream(j.Result)
}

// MergedRows blocks until the job has consumed all sentinels and
// returns the merged rows for a zero-stage job. Returns nil for staged
// jobs (the post-stage text output discards row metadata). Used by the
// coordinator transitionally while QueryResult still carries Rows;
// deleted in Phase 4 when the wire moves to LineStream.
func (j *LocalComputeJob) MergedRows() ([]*pb.ResultRow, error) {
	r := j.readResult()
	if r.err != nil {
		return nil, r.err
	}
	return r.rows, nil
}

// consume reads all messages from the channel until every shard's
// sentinel has arrived, then flattens rows into a single input stream
// for the compute pipeline. Shards' per-stream ordering (baked into
// their SQL via ORDER BY) is preserved through a k-way merge on the
// join column; an empty / unknown mergeKey falls back to arrival-order
// concat inside mergeRowsFull.
//
// For zero-stage jobs the merged rows are also stored on jobResult so
// MergedRows callers can recover the full *pb.ResultRow values
// (line_num, fields, etc.) that the staged-output text path discards.
// This is a transitional affordance for callers that still want the
// Rows shape; phase 4 deletes it when the wire moves to LineStream.
func (j *LocalComputeJob) consume() {
	perShard := map[string][]*pb.ResultRow{}
	sentinels := 0

	for sentinels < j.shardCount {
		msg := <-j.ch
		if msg.Sentinel {
			sentinels++
			continue
		}
		if msg.Row == nil {
			continue
		}
		perShard[msg.ShardID] = append(perShard[msg.ShardID], msg.Row)
	}

	merged := mergeRowsFull(perShard, j.mergeKey)

	// Global tail/head cap: the per-shard LIMIT was already applied
	// by the SQL builder; this truncates the union before downstream
	// stages see it. Concat path (mergeKey == "") also gets capped —
	// without an order it's "first N to arrive" rather than "first N
	// in any deterministic ordering," but downstream stages that need
	// determinism shouldn't be running over an unmerged stream
	// anyway.
	if j.limit > 0 && len(merged) > j.limit {
		merged = merged[:j.limit]
	}

	// Zero-stage job: the merged rows ARE the output. Skip the bash
	// pipeline entirely — running RunWithParallelism with an empty
	// pipeline string returns empty stdout. This is the path a plain
	// `cat /log/foo.log` takes after Phase 2 routes it through compute.
	if len(j.stages) == 0 {
		var sb strings.Builder
		for _, r := range merged {
			sb.WriteString(r.Raw)
			sb.WriteByte('\n')
		}
		j.done <- jobResult{text: sb.String(), rows: merged}
		return
	}

	var sb strings.Builder
	for i, r := range merged {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(r.Raw)
	}
	if len(merged) > 0 {
		sb.WriteByte('\n')
	}
	input := sb.String()

	pipeline := strings.Join(j.stages, " | ")
	stdout, stderr, exitCode, err := bash.RunWithParallelism(pipeline, input, j.parallelism)
	if err != nil {
		j.done <- jobResult{err: fmt.Errorf("compute: %w", err)}
		return
	}
	if exitCode != 0 && stderr != "" {
		j.done <- jobResult{err: fmt.Errorf("compute exit %d: %s", exitCode, stderr)}
		return
	}

	j.done <- jobResult{text: stdout}
}
