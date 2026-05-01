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
		streamCh:    make(chan Line, 1024),
	}
}

// LocalComputeJob collects rows from shards via a channel and pushes
// merged + (optionally) staged output as Lines into streamCh.
//
// The job is single-consumer on the output side: callers either drain
// streamCh via Stream() OR call Result()/MergedRows() (which drain
// internally into a cache). Mixing the two on one job is not
// supported; the cache only fires when no live consumer has the
// channel.
type LocalComputeJob struct {
	stages      []string
	shardCount  int
	mergeKey    string // see ComputePool.NewJob; empty = concat, otherwise k-way merge
	limit       int    // see ComputePool.NewJob; 0 = no cap
	parallelism int

	ch        chan ComputeMessage // shards push rows + sentinels here
	streamCh  chan Line           // consume() pushes output here
	streamErr error               // set by consume before closing streamCh

	consumeOnce sync.Once

	// drain cache — populated on first Result/MergedRows call.
	drainOnce    sync.Once
	drainedLines []Line
	drainedErr   error
}

func (j *LocalComputeJob) ensureStarted() {
	j.consumeOnce.Do(func() { go j.consume() })
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

// Stream returns a forward-only line stream of the job's output. Lines
// are emitted as the merge advances (zero-stage jobs) or as the bash
// pipeline finishes and its stdout is split (staged jobs). Single-
// consumer: callers must NOT also call Result/MergedRows on the same
// job once Stream has been claimed.
func (j *LocalComputeJob) Stream() LineStream {
	j.ensureStarted()
	return newChannelLineStream(j.streamCh, &j.streamErr)
}

// drain reads streamCh to exhaustion into a cached slice. Used by
// Result and MergedRows; fires once via sync.Once so multiple batch-
// API callers see consistent data.
func (j *LocalComputeJob) drain() ([]Line, error) {
	j.ensureStarted()
	j.drainOnce.Do(func() {
		for line := range j.streamCh {
			j.drainedLines = append(j.drainedLines, line)
		}
		j.drainedErr = j.streamErr
	})
	return j.drainedLines, j.drainedErr
}

// Result drains the stream and returns the joined text. Convenience
// for callers that want the whole output as one string.
func (j *LocalComputeJob) Result() (string, error) {
	lines, err := j.drain()
	if err != nil {
		return "", err
	}
	return linesToText(lines), nil
}

// MergedRows drains the stream and reconstructs *pb.ResultRow entries
// for zero-stage jobs (each line maps back to one row, preserving
// LineNum). Returns nil for staged jobs — the post-stage text path
// loses row metadata. Used by the coordinator transitionally while
// QueryResult still carries Rows; phase 4 follow-ups will route
// callers to Stream() directly.
func (j *LocalComputeJob) MergedRows() ([]*pb.ResultRow, error) {
	if len(j.stages) > 0 {
		return nil, nil
	}
	lines, err := j.drain()
	if err != nil {
		return nil, err
	}
	rows := make([]*pb.ResultRow, len(lines))
	for i, l := range lines {
		rows[i] = &pb.ResultRow{LineNum: l.LineNum, Raw: l.Raw}
	}
	return rows, nil
}

// consume reads all messages from j.ch until every shard's sentinel
// has arrived, k-way-merges the per-shard buffers, applies the
// global head/tail cap, and pushes Lines onto j.streamCh as the
// merge produces them. For zero-stage jobs the merged rows ARE the
// output; for staged jobs the merged rows are joined + fed through
// warp and the stdout is split into Lines after the pipeline
// completes (warp doesn't expose a streaming bytes API today).
//
// streamErr is set BEFORE the channel is closed so consumers see
// the error after the channel signals exhaustion.
func (j *LocalComputeJob) consume() {
	defer close(j.streamCh)

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

	if len(j.stages) == 0 {
		// Zero stages: merged rows ARE the output. Push each line as
		// the heap walk produces it (today mergeRowsFull is a single
		// pass that returns a slice; in a future iteration we can
		// inline the heap walk here to truly stream).
		for _, r := range merged {
			j.streamCh <- Line{Raw: r.Raw, LineNum: r.LineNum}
		}
		return
	}

	// Staged: feed merged rows into warp as one batch. warp's
	// RunWithParallelism is non-streaming; once it returns we split
	// the stdout into Lines. Future warp work could expose a
	// streaming exec API and turn this into per-line pushes too.
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
		j.streamErr = fmt.Errorf("compute: %w", err)
		return
	}
	if exitCode != 0 && stderr != "" {
		j.streamErr = fmt.Errorf("compute exit %d: %s", exitCode, stderr)
		return
	}

	if stdout == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		j.streamCh <- Line{Raw: line}
	}
}
