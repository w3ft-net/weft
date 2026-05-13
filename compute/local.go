package compute

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/kevlarcon/warp/pkg/bash"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// LocalComputePool executes compute stages in-process using warp's
// bash interpreter through bash.RunStreaming. Streaming requires
// warp's per-stage io.Pipe execution path, which is mutually
// exclusive with warp's chunked intra-stage parallelism today. See
// docs/plans/streaming-parallel-warp.md for the planned warp-side
// work to combine streaming with chunk-level parallelism; until
// that lands, weft's compute stages are always single-worker.
type LocalComputePool struct {
	AWSCreds  aws.CredentialsProvider
	AWSRegion string
}

func (p *LocalComputePool) ExecuteQuery(req *pb.ExecuteQueryRequest) (LineStream, error) {
	job := p.NewJob(req.Stages, len(req.Shards), req.MergeKey, int(req.Limit), req.KeepLast)
	go FanOutToShards(req, job, p.AWSCreds, p.AWSRegion)
	return job.Stream(), nil
}

func (p *LocalComputePool) NewJob(stages []string, shardCount int, mergeKey string, limit int, keepLast bool) ComputeJob {
	return &LocalComputeJob{
		stages:     stages,
		shardCount: shardCount,
		mergeKey:   mergeKey,
		limit:      limit,
		keepLast:   keepLast,
		ch:         make(chan ComputeMessage, 1024),
		streamCh:   make(chan Line, 1024),
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
	stages     []string
	shardCount int
	mergeKey   string // see ComputePool.NewJob; empty = concat, otherwise k-way merge
	limit      int    // see ComputePool.NewJob; 0 = no cap
	keepLast   bool   // tail-mode: retain last N from the merged stream and emit at EOF

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
// for zero-stage jobs. Returns nil for staged jobs — the post-stage
// text path loses row metadata.
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
		rows[i] = &pb.ResultRow{Raw: l.Raw}
	}
	return rows, nil
}

// consume drives onlineMerge over the per-shard rows arriving on j.ch
// and pushes Lines onto j.streamCh as the merge produces them.
//
// Zero-stage jobs forward each merged row directly to streamCh as
// soon as the k-way merge determines its position.
//
// Staged jobs route the merged stream through warp via an io.Pipe:
// a writer goroutine pumps merged rows into the pipe (one row + "\n"
// per emit); warp's bash.RunStreaming reads from the pipe, runs the
// pipeline (with stream-safe stages emitting incrementally), and
// writes to a second io.Pipe whose read side is scanned line-by-line
// into streamCh. End-to-end this gives stream-safe pipelines (grep,
// awk no-END, sed, head, cut, tr, tee, uniq) per-line latency from
// shard arrival to client; batch-only stages (sort, wc, tail -N)
// still buffer to EOF as their semantics require.
//
// retainLast drains the merged stream to exhaustion and returns the
// final j.limit rows in arrival (i.e. merge-key) order. Used by tail-
// mode (keep_last): per-shard SQL has the tail subquery flip so rows
// arrive ASC, and "last N globally" can't be computed by stopping
// early — we have to see every row to know which ones make the cut.
// The buffer is a fixed-size deque implemented as a circular slice;
// memory stays at O(N) regardless of stream length.
//
// Returns the rows in the order they should be emitted to the
// downstream consumer (ASC for the tail-flip case). The buffer is
// never larger than j.limit, so memory is bounded by the user's
// requested N.
func (j *LocalComputeJob) retainLast() []*pb.ResultRow {
	if j.limit <= 0 {
		return nil
	}
	buf := make([]*pb.ResultRow, 0, j.limit)
	head := 0 // index of the oldest row when len(buf) == j.limit
	full := false
	onlineMerge(j.ch, j.shardCount, j.mergeKey, func(row *pb.ResultRow) bool {
		if !full {
			buf = append(buf, row)
			if len(buf) == j.limit {
				full = true
			}
		} else {
			buf[head] = row
			head++
			if head == j.limit {
				head = 0
			}
		}
		return true
	})
	if !full {
		return buf
	}
	// Rotate so oldest is at index 0.
	out := make([]*pb.ResultRow, j.limit)
	copy(out, buf[head:])
	copy(out[j.limit-head:], buf[:head])
	return out
}

// streamErr is set BEFORE the channel is closed so consumers see the
// error after the channel signals exhaustion.
func (j *LocalComputeJob) consume() {
	defer close(j.streamCh)

	if len(j.stages) == 0 {
		if j.keepLast && j.limit > 0 {
			buf := j.retainLast()
			for i, r := range buf {
				j.streamCh <- Line{Raw: r.Raw, LineNum: int64(i + 1)}
			}
			return
		}
		emitted := 0
		onlineMerge(j.ch, j.shardCount, j.mergeKey, func(row *pb.ResultRow) bool {
			if j.limit > 0 && emitted >= j.limit {
				return false
			}
			emitted++
			j.streamCh <- Line{Raw: row.Raw, LineNum: int64(emitted)}
			return true
		})
		return
	}

	pipeline := strings.Join(j.stages, " | ")
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	// Writer: pump merged rows into warp's stdin. Closing stdinW
	// signals EOF to the pipeline, which lets sort/wc/etc. flush.
	//
	// Tail mode (keep_last): the merge runs to exhaustion into a ring
	// buffer of N rows; only after EOF do we feed the buffer into the
	// pipeline's stdin in order. The bash stages run as usual on those
	// N lines. Tradeoff: tail loses streaming — first user-visible row
	// comes only after all shards drain — which is intrinsic to "last
	// N" semantics (the merge can't know what the last N are until it
	// has seen everything).
	go func() {
		defer stdinW.Close()
		if j.keepLast && j.limit > 0 {
			for _, r := range j.retainLast() {
				if _, err := io.WriteString(stdinW, r.Raw); err != nil {
					return
				}
				if _, err := stdinW.Write([]byte{'\n'}); err != nil {
					return
				}
			}
			return
		}
		emitted := 0
		onlineMerge(j.ch, j.shardCount, j.mergeKey, func(row *pb.ResultRow) bool {
			if j.limit > 0 && emitted >= j.limit {
				return false
			}
			if _, err := io.WriteString(stdinW, row.Raw); err != nil {
				return false
			}
			if _, err := stdinW.Write([]byte{'\n'}); err != nil {
				return false
			}
			emitted++
			return true
		})
	}()

	// Runner: warp executes the pipeline. Its stdout writes go to
	// stdoutW; closing stdoutW after ExecScript returns lets the
	// reader loop below see EOF.
	var stderrBuf strings.Builder
	runErrCh := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		exitCode, err := bash.RunStreaming(pipeline, stdinR, stdoutW, &stderrBuf)
		if err != nil {
			runErrCh <- fmt.Errorf("compute: %w", err)
			return
		}
		if exitCode != 0 && stderrBuf.Len() > 0 {
			runErrCh <- fmt.Errorf("compute exit %d: %s", exitCode, stderrBuf.String())
			return
		}
		runErrCh <- nil
	}()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		j.streamCh <- Line{Raw: scanner.Text()}
	}
	if err := scanner.Err(); err != nil {
		j.streamErr = fmt.Errorf("compute: read stdout: %w", err)
		// Drain runner before returning so we don't leak the goroutine.
		<-runErrCh
		return
	}
	if err := <-runErrCh; err != nil {
		j.streamErr = err
	}
}
