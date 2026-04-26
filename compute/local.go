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

func (j *LocalComputeJob) Result() (string, error) {
	j.ensureStarted()
	r := <-j.done
	return r.text, r.err
}

// consume reads all messages from the channel until every shard's
// sentinel has arrived, then flattens rows into a single input stream
// for the compute pipeline. When mergeKey is set, shards' per-stream
// ordering (baked into their SQL via ORDER BY) is preserved through a
// k-way merge on the join column; when empty, rows land in arrival
// order (the legacy single-sink concat).
func (j *LocalComputeJob) consume() {
	var lines []string
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
		if j.mergeKey == "" {
			// Fast path: just append in arrival order.
			lines = append(lines, msg.Row.Raw)
		} else {
			perShard[msg.ShardID] = append(perShard[msg.ShardID], msg.Row)
		}
	}

	if j.mergeKey != "" {
		lines = mergeRows(perShard, j.mergeKey)
	}

	// Global tail/head cap: the per-shard LIMIT was already applied
	// by the SQL builder; this truncates the union before downstream
	// stages see it. Concat path (mergeKey == "") also gets capped —
	// without an order it's "first N to arrive" rather than "first N
	// in any deterministic ordering," but downstream stages that need
	// determinism shouldn't be running over an unmerged stream
	// anyway.
	if j.limit > 0 && len(lines) > j.limit {
		lines = lines[:j.limit]
	}

	input := strings.Join(lines, "\n")
	if len(lines) > 0 {
		input += "\n"
	}

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
