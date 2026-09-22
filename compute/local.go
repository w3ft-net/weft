package compute

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/graybeardcorp/warp/pkg/bash"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/theme"
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
	// ShardSecret is the SHARD_SECRET bearer token attached to outbound
	// gRPC-shard dials. cmd/compute/main.go reads it from env at startup;
	// tests that never reach a real shard can leave it empty.
	ShardSecret string

	// overrides lets callers register pre-built Resolvers keyed by
	// ShardID. Checked before falling back to descriptor-derived
	// resolution; the in-process test path (computetest.InProcessResolver)
	// flows through here. Nothing in production code calls
	// RegisterResolver, so production builds never have overrides.
	overrides sync.Map // map[string]Resolver

	// cache holds the dialed shard.Shard for each ShardID alongside
	// the addr it was dialed at. A subsequent query reuses the cached
	// handle when the descriptor's addr matches; addr mismatch (which
	// only happens on rebind) invalidates the entry, closes the stale
	// connection, and re-dials. Override-derived entries cache too —
	// their addr is empty and never changes, so the cache hit is
	// permanent until the pool is dropped.
	cache sync.Map // map[string]*cachedShard
}

// cachedShard pairs a dialed handle with the addr it was dialed at so
// a rebind (descriptor's GrpcAddr changes) is detected on the next
// query and the stale connection is closed before re-dialing.
type cachedShard struct {
	addr string
	sh   shard.Shard
}

// RegisterResolver installs an override for a specific ShardID. Used
// by tests (with computetest.InProcessResolver) and any other caller
// that wants compute to bypass descriptor-based dialing for a
// known-id shard. The override applies until the next call for the
// same id, at which point this one is replaced.
func (p *LocalComputePool) RegisterResolver(r Resolver) {
	p.overrides.Store(r.ShardID(), r)
}

// resolveShard turns a descriptor into a live shard.Shard handle.
// Precedence: registered override → cache → descriptor-derived
// resolver. A cache hit with matching addr returns immediately. A
// cache hit with mismatching addr (rebind) closes the stale handle
// and re-dials. The freshly-dialed handle is stored in the cache so
// the next query for this id is a hit.
func (p *LocalComputePool) resolveShard(d *pb.ShardDescriptor) (shard.Shard, error) {
	if v, ok := p.overrides.Load(d.ShardId); ok {
		r := v.(Resolver)
		// Overrides bypass the addr-based invalidation logic — their
		// handle is the source of truth, addr is empty/synthetic.
		// Cache the result so repeat queries skip the Dial call,
		// which for in-process resolvers is already cheap but for
		// future override kinds may not be.
		if cached, ok := p.cache.Load(d.ShardId); ok {
			return cached.(*cachedShard).sh, nil
		}
		sh, err := r.Dial()
		if err != nil {
			return nil, err
		}
		p.cache.Store(d.ShardId, &cachedShard{sh: sh})
		return sh, nil
	}

	if cached, ok := p.cache.Load(d.ShardId); ok {
		c := cached.(*cachedShard)
		if c.addr == d.GrpcAddr {
			return c.sh, nil
		}
		// Rebind: addr changed, drop the stale conn. Close errors
		// are logged but not propagated — the new dial below is the
		// path that matters.
		if err := c.sh.Close(); err != nil {
			log.Printf("compute: close stale handle for %s (addr %s → %s): %v", d.ShardId, c.addr, d.GrpcAddr, err)
		}
	}

	r, err := ResolverFromDescriptor(d, p.ShardSecret, p.AWSCreds, p.AWSRegion)
	if err != nil {
		return nil, err
	}
	sh, err := r.Dial()
	if err != nil {
		return nil, err
	}
	p.cache.Store(d.ShardId, &cachedShard{addr: d.GrpcAddr, sh: sh})
	return sh, nil
}

func (p *LocalComputePool) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (LineStream, error) {
	job := p.NewJob(req.Stages, len(req.Shards), req.MergeKey, int(req.Limit), req.KeepLast, req.TraceTag)
	go FanOutToShards(ctx, req, job, p.resolveShard)
	return job.Stream(), nil
}

func (p *LocalComputePool) NewJob(stages []string, shardCount int, mergeKey string, limit int, keepLast bool, traceTag bool) ComputeJob {
	return &LocalComputeJob{
		stages:     stages,
		shardCount: shardCount,
		mergeKey:   mergeKey,
		limit:      limit,
		keepLast:   keepLast,
		traceTag:   traceTag,
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
	traceTag   bool   // bare interactive `trace <id>`: buffer, summarize, badge each row — see ComputePool.NewJob

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

// emitTraceTagged buffers the entire merged stream for a bare
// interactive `trace <id>` (no downstream compute stages), then emits
// a synthetic summary line followed by every row prefixed with a
// colored [namespace/service] badge. Buffering (rather than streaming
// as rows arrive) is required to know the final line/service counts
// for the summary before emitting it first — accepted as a v1
// tradeoff because a trace ID scopes to one bounded request's log
// lines, unlike a general cat/tail over an app's full volume. j.limit
// still caps the buffer when set (0 is the common case: trace sets no
// default limit).
func (j *LocalComputeJob) emitTraceTagged() {
	var rows []*pb.ResultRow
	onlineMerge(j.ch, j.shardCount, j.mergeKey, func(row *pb.ResultRow) bool {
		if j.limit > 0 && len(rows) >= j.limit {
			return false
		}
		rows = append(rows, row)
		return true
	})

	if len(rows) == 0 {
		j.streamCh <- Line{Raw: theme.Palette().Comment + "# no lines found" + theme.Palette().Reset}
		return
	}

	services := map[string]bool{}
	for _, r := range rows {
		ns, svc := r.MetaVars["namespace"], r.MetaVars["service"]
		if ns == "" && svc == "" {
			continue
		}
		services[ns+"/"+svc] = true
	}
	firstTs := time.Unix(0, rows[0].TsUnixNs).UTC().Format(time.RFC3339)
	lastTs := time.Unix(0, rows[len(rows)-1].TsUnixNs).UTC().Format(time.RFC3339)
	line := fmt.Sprintf("# %d line(s) across %d service(s), %s to %s", len(rows), len(services), firstTs, lastTs)
	j.streamCh <- Line{Raw: theme.Palette().Comment + line + theme.Palette().Reset}

	for i, row := range rows {
		badge := theme.TraceTag(row.MetaVars["namespace"], row.MetaVars["service"])
		j.streamCh <- Line{Raw: badge + row.Raw, LineNum: int64(i + 1)}
	}
}

// streamErr is set BEFORE the channel is closed so consumers see the
// error after the channel signals exhaustion.
func (j *LocalComputeJob) consume() {
	defer close(j.streamCh)

	if len(j.stages) == 0 {
		if j.traceTag {
			j.emitTraceTagged()
			return
		}
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

	// awkVarsCh carries each row's meta-var values, in the exact order
	// rows are written to stdinW, to an AwkPerRecordVars hook the
	// Runner goroutine installs — awk has no per-row side channel like
	// jq's JSON-body splice (a bare reserved name is just an ordinary
	// awk global, unrelated to the record's own text), so this queue is
	// the correlation mechanism instead: one send per row written, one
	// receive per record the awk stage reads, both in the same order.
	// Only allocated when a stage is actually awk — the common case (no
	// awk stage) skips it entirely, since nothing would ever drain it
	// (a full buffer would then wedge the writer goroutine).
	var awkVarsCh chan map[string]string
	if stagesHaveAwk(j.stages) {
		awkVarsCh = make(chan map[string]string, 1024)
	}

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
		if awkVarsCh != nil {
			defer close(awkVarsCh)
		}
		if j.keepLast && j.limit > 0 {
			for _, r := range j.retainLast() {
				if _, err := io.WriteString(stdinW, mergeMetaVars(r.Raw, r.MetaVars)); err != nil {
					return
				}
				if _, err := stdinW.Write([]byte{'\n'}); err != nil {
					return
				}
				if awkVarsCh != nil {
					awkVarsCh <- awkVarsFromMeta(r.MetaVars)
				}
			}
			return
		}
		emitted := 0
		onlineMerge(j.ch, j.shardCount, j.mergeKey, func(row *pb.ResultRow) bool {
			if j.limit > 0 && emitted >= j.limit {
				return false
			}
			if _, err := io.WriteString(stdinW, mergeMetaVars(row.Raw, row.MetaVars)); err != nil {
				return false
			}
			if _, err := stdinW.Write([]byte{'\n'}); err != nil {
				return false
			}
			if awkVarsCh != nil {
				awkVarsCh <- awkVarsFromMeta(row.MetaVars)
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
		var exitCode int
		var err error
		if awkVarsCh != nil {
			exitCode, err = bash.RunStreamingWithAwkHook(pipeline, stdinR, stdoutW, &stderrBuf, func(string) map[string]string {
				return <-awkVarsCh
			})
		} else {
			exitCode, err = bash.RunStreaming(pipeline, stdinR, stdoutW, &stderrBuf)
		}
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
