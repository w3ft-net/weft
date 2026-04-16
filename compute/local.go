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

func (p *LocalComputePool) NewJob(stages []string, shardCount int) ComputeJob {
	return &LocalComputeJob{
		stages:      stages,
		shardCount:  shardCount,
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

// consume reads all messages from the channel, collecting row text until all
// sentinels arrive, then runs the pipeline.
func (j *LocalComputeJob) consume() {
	var lines []string
	sentinels := 0

	for sentinels < j.shardCount {
		msg := <-j.ch
		if msg.Sentinel {
			sentinels++
			continue
		}
		if msg.Row != nil {
			lines = append(lines, msg.Row.Raw)
		}
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
