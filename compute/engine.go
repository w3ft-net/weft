package compute

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/graybeardcorp/warp/pkg/awk"
	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/jq"
	"github.com/tidwall/gjson"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
	"github.com/w3ft-net/weft/theme"
)

// Engine runs queries: it fans out SQL to shards, k-way merges their
// rows, applies the global limit, and runs the pipeline's compute
// stages through warp's bash.RunStreaming. It is what the compute
// worker (cmd/compute) serves; tests use it in-process. Streaming requires
// warp's per-stage io.Pipe execution path, which is mutually
// exclusive with warp's chunked intra-stage parallelism today. See
// docs/plans/streaming-parallel-warp.md for the planned warp-side
// work to combine streaming with chunk-level parallelism; until
// that lands, weft's compute stages are always single-worker.
type Engine struct {
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
	// only happens on rebind) repoints a live handle to the new addr, or
	// closes any other kind and re-dials. Override-derived entries cache too —
	// their addr is empty and never changes, so the cache hit is
	// permanent until the engine is dropped.
	cache sync.Map // map[string]*cachedShard
}

// cachedShard pairs a dialed handle with the addr it was dialed at so
// a rebind (descriptor's GrpcAddr changes) is detected on the next
// query and the handle follows it.
type cachedShard struct {
	addr string
	sh   shard.Shard
}

// RegisterResolver installs an override for a specific ShardID. Used
// by tests (with computetest.InProcessResolver) and any other caller
// that wants compute to bypass descriptor-based dialing for a
// known-id shard. The override applies until the next call for the
// same id, at which point this one is replaced.
func (p *Engine) RegisterResolver(r Resolver) {
	p.overrides.Store(r.ShardID(), r)
}

// resolveShard turns a descriptor into a live shard.Shard handle.
// Precedence: registered override → cache → descriptor-derived
// resolver. A cache hit with matching addr returns immediately. A
// cache hit with mismatching addr (rebind) repoints a live handle, or
// closes any other and re-dials. The freshly-dialed handle is stored in the cache so
// the next query for this id is a hit.
func (p *Engine) resolveShard(ctx context.Context, d *pb.ShardDescriptor) (shard.Shard, error) {
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
		// Rebind of a live shard: follow it to the new address on the
		// same handle. Closing it instead would fail every query that
		// resolved it a moment ago, or is still running on it.
		if g, ok := c.sh.(*shard.GRPCShard); ok && d.GrpcAddr != "" {
			g.Repoint(d.GrpcAddr)
			p.cache.Store(d.ShardId, &cachedShard{addr: d.GrpcAddr, sh: g})
			return g, nil
		}
		// Any other handle (a shard that went cold): drop it and re-dial.
		// Close errors are logged but not propagated — the new dial
		// below is the path that matters.
		if err := c.sh.Close(); err != nil {
			weftsdk.Infof(ctx, "compute: close stale handle for %s (addr %s → %s): %v", d.ShardId, c.addr, d.GrpcAddr, err)
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

func (p *Engine) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (LineStream, error) {
	job := p.NewJob(req.Stages, len(req.Shards), req.MergeKey, int(req.Limit), req.KeepLast, req.TraceTag)
	if len(req.RowStages) > 0 {
		rs, err := NewRowStages(req.RowStages)
		if err != nil {
			return nil, err
		}
		job.rowStages = rs
	}
	job.stagesReadPayload = req.StagesReadPayload
	job.fieldSpec = req.FieldSpec
	aggregate := req.Aggregate
	if isGroupAggregate(aggregate) {
		// A coordinator from before GroupSpec; every one sends it now.
		return nil, fmt.Errorf("compute: unknown aggregate %q: grouping is a GroupSpec", aggregate)
	}
	if req.Group != nil {
		if aggregate != "" {
			return nil, errors.New("compute: a request names both an aggregate and a group")
		}
		var err error
		if aggregate, err = groupAggregate(req.Group); err != nil {
			return nil, err
		}
		if job.orderByCount, job.groupLimit, err = groupOrder(req.Group); err != nil {
			return nil, err
		}
		if job.valueOrder, err = groupValueOrder(req.Group); err != nil {
			return nil, err
		}
	}
	if aggregate != "" {
		// Check the name now, so an aggregate this engine doesn't know
		// fails the query instead of passing the shards' rows through.
		if _, err := newAggregator(aggregate, nil); err != nil {
			return nil, err
		}
		job.aggregate = aggregate
	}
	go FanOutToShards(ctx, req, job, p.resolveShard)
	return job.Stream(), nil
}

// NewJob creates a job that merges rows from shardCount shards and runs
// stages over the result.
//
// keepLast selects the limit semantic: false is the default head-style
// "stop after N emitted"; true switches to tail-style "retain last N,
// emit at EOF" — required when shards return ASC after the tail
// subquery flip (see planner/sql.go::Query.Wrap).
//
// traceTag, when true, switches the job's zero-stage output from plain
// streaming to buffer-then-summarize-then-tag: every row is prefixed
// with a colored [namespace/service] badge and a synthetic summary line
// precedes the batch. Only ever set true for a bare, interactive
// `trace <id>` — see coordinator.go's executeQuery.
func (p *Engine) NewJob(stages []string, shardCount int, mergeKey string, limit int, keepLast bool, traceTag bool) *Job {
	j := &Job{
		stages:     stages,
		shardCount: shardCount,
		mergeKey:   mergeKey,
		limit:      limit,
		keepLast:   keepLast,
		traceTag:   traceTag,
		ch:         make(chan ComputeMessage, 1024),
		streamCh:   make(chan Line, 1024),
	}
	if len(stages) > 0 {
		j.meta = planner.StageMetaRead(stages[0])
	}
	if j.meta.Reads() {
		switch {
		case firstStageIsJq(stages):
			j.jqBinder = NewJqBinder(j.meta)
		case firstStageIsAwk(stages):
			j.binder = newAwkBinder(j.meta)
		}
	}
	return j
}

// Job collects rows from shards via a channel and pushes
// merged + (optionally) staged output as Lines into streamCh.
//
// The job is single-consumer on the output side: callers either drain
// streamCh via Stream() OR call Result()/MergedRows() (which drain
// internally into a cache). Mixing the two on one job is not
// supported; the cache only fires when no live consumer has the
// channel.
type Job struct {
	stages     []string
	shardCount int
	mergeKey   string // see Engine.NewJob; empty = concat, otherwise k-way merge
	limit      int    // see Engine.NewJob; 0 = no cap
	keepLast   bool   // tail-mode: retain last N from the merged stream and emit at EOF
	traceTag   bool   // bare interactive `trace <id>`: buffer, summarize, badge each row — see Engine.NewJob
	// rowStages, when set, run on each merged row after the limit and
	// before the stages; see RowStages.
	rowStages *RowStages
	// stagesReadPayload feeds the first stage each row's payload instead
	// of its raw line (planner.QueryPlan.StagesReadPayload).
	stagesReadPayload bool
	// fieldSpec, when set, says which fields the first stage reads: a row
	// that arrives in field form is rebuilt into the line it reads as it
	// enters the job (RebuildRecord).
	fieldSpec *pb.FieldSpec
	// meta is the meta-variables the first stage reads, the only stage that
	// reads shard rows': a jq there has them bound for each input (jqBinder),
	// an awk per record (binder). Nothing is built per row if it reads none.
	meta     planner.MetaRead
	jqBinder *JqBinder
	binder   awkBinder
	// aggregate finishes the shards' aggregate (planner.QueryPlan.
	// Aggregate) on the merged rows; feedErr is its failure.
	aggregate string
	// order and limit finish a count_distinct aggregate that a GroupSpec
	// ordered (see orderedAggregator).
	orderByCount bool
	// valueOrder orders the lines of an awk aggregate (see sortedAggregator).
	valueOrder *valueOrder
	groupLimit int
	feedErr    error

	// rowsScanned sums the lines the job's cold shards scanned (ReportScan).
	rowsScanned atomic.Int64

	ch        chan ComputeMessage // shards push rows + sentinels here
	streamCh  chan Line           // consume() pushes output here
	streamErr error               // set by consume before closing streamCh

	// shardErr is the first shard failure (Fail). The answer is missing
	// that shard's rows, so it fails the query whatever else was emitted.
	shardErrMu sync.Mutex
	shardErr   error

	consumeOnce sync.Once

	// drain cache — populated on first Result/MergedRows call.
	drainOnce    sync.Once
	drainedLines []Line
	drainedErr   error
}

func (j *Job) ensureStarted() {
	j.consumeOnce.Do(func() { go j.consume() })
}

func (j *Job) Send(msg ComputeMessage) {
	j.ensureStarted()
	j.ch <- msg
}

// SendRow implements shard.RowSink.
func (j *Job) SendRow(row *pb.ResultRow, shardID string) {
	if j.fieldSpec != nil && row.FieldCount != nil {
		row.Raw = RebuildRecord(j.fieldSpec, row)
	}
	j.Send(ComputeMessage{Row: row, ShardID: shardID})
}

// SendSentinel implements shard.RowSink.
func (j *Job) SendSentinel(shardID string) {
	j.Send(ComputeMessage{ShardID: shardID, Sentinel: true})
}

// Fail implements shard.RowSink. It is recorded before the shard's
// sentinel is sent, so once the merge has seen every sentinel it has seen
// every failure.
func (j *Job) Fail(shardID string, err error) {
	j.shardErrMu.Lock()
	defer j.shardErrMu.Unlock()
	if j.shardErr == nil {
		j.shardErr = &MissingRowsError{ShardID: shardID, Err: err}
	}
}

// shardFailure is the first shard failure Fail recorded, if any.
func (j *Job) shardFailure() error {
	j.shardErrMu.Lock()
	defer j.shardErrMu.Unlock()
	return j.shardErr
}

// Stream returns a forward-only line stream of the job's output. Lines
// are emitted as the merge advances (zero-stage jobs) or as the bash
// pipeline finishes and its stdout is split (staged jobs). Single-
// consumer: callers must NOT also call Result/MergedRows on the same
// job once Stream has been claimed.
func (j *Job) Stream() LineStream {
	j.ensureStarted()
	st := newChannelLineStream(j.streamCh, &j.streamErr)
	st.scans = j.rowsScanned.Load
	return st
}

// ReportScan implements shard.ScanReporter: a cold shard says how many lines
// it scanned. Every shard reports before its sentinel, so the total is
// complete once the merged stream ends.
func (j *Job) ReportScan(_ string, s shard.ScanStats) { j.rowsScanned.Add(s.RowsScanned) }

// drain reads streamCh to exhaustion into a cached slice. Used by
// Result and MergedRows; fires once via sync.Once so multiple batch-
// API callers see consistent data.
func (j *Job) drain() ([]Line, error) {
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
func (j *Job) Result() (string, error) {
	lines, err := j.drain()
	if err != nil {
		return "", err
	}
	return linesToText(lines), nil
}

// MergedRows drains the stream and reconstructs *pb.ResultRow entries
// for zero-stage jobs. Returns nil for staged jobs — the post-stage
// text path loses row metadata.
func (j *Job) MergedRows() ([]*pb.ResultRow, error) {
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
// Staged jobs route the merged stream through warp via a buffered pipe
// (chunkPipe): a writer goroutine pumps merged rows into the pipe (one
// row + "\n" per emit); warp's bash.RunStreaming reads from the pipe, runs
// the pipeline (with stream-safe stages emitting incrementally), and
// writes to a second pipe whose read side is scanned line-by-line
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
func (j *Job) retainLast() []*pb.ResultRow {
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
// a synthetic summary line followed by every row as a delimited
// "<ts_unix_ns>\t<namespace/service>\t<raw>" line — plain data, not
// pre-rendered ANSI. The client (shell.ExecNonInteractive) is the one
// that knows its own terminal width, so it's the one that turns this
// into the actual 3-column gap-histogram/badge/wrapped-message display
// via theme.RenderTraceColumns; the coordinator has no way to know
// that width and shouldn't guess. Buffering (rather than streaming as
// rows arrive) is required to know the final line/service counts for
// the summary before emitting it first — accepted as a v1 tradeoff
// because a trace ID scopes to one bounded request's log lines, unlike
// a general cat/tail over an app's full volume. j.limit still caps the
// buffer when set (0 is the common case: trace sets no default limit).
func (j *Job) emitTraceTagged() {
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
		ns, svc := row.MetaVars["namespace"], row.MetaVars["service"]
		label := ns + "/" + svc
		if ns == "" && svc == "" {
			label = "unknown"
		}
		raw := fmt.Sprintf("%d\t%s\t%s", row.TsUnixNs, label, row.Raw)
		j.streamCh <- Line{Raw: raw, LineNum: int64(i + 1)}
	}
}

// stageInput is the text a row feeds the first stage: its payload when
// the stage reads it, else its raw line. A jq first stage naming a
// meta-variable reads them as "_"-prefixed keys spliced into the JSON.
func (j *Job) stageInput(row *pb.ResultRow) string {
	in := row.Raw
	if j.stagesReadPayload {
		in = row.MetaVars[planner.PayloadColumn]
	}
	return in
}

// rowAwkVars is a row's meta-variables for awk, for each of the records
// (lines) awk reads from it.
type rowAwkVars struct {
	binds   []awk.Binding
	records int
}

// feed drives the merged rows, cut to the limit, through the aggregate
// or the row stages into sink, which returns false to stop. The limit
// counts rows before the row stages, as the head/tail it came from ran
// first. A plan never has both a limit and an aggregate, or row stages
// and an aggregate.
func (j *Job) feed(sink func(*pb.ResultRow) bool) {
	if j.aggregate != "" {
		a, err := newAggregator(j.aggregate, sink)
		if err == nil && j.orderByCount {
			a = newOrderedAggregator(a.(*aggregator), j.groupLimit, sink)
		}
		if err == nil && j.valueOrder != nil {
			a, err = newSortedAggregator(j.aggregate, j.valueOrder, j.groupLimit, sink)
		}
		if err != nil {
			j.feedErr = err
			return
		}
		defer func() {
			a.flush()
			j.feedErr = a.failure()
		}()
		sink = a.push
	}
	if j.rowStages != nil {
		r := j.rowStages.runner(sink)
		defer r.flush()
		sink = func(row *pb.ResultRow) bool { return r.push(0, row) }
	}
	if j.keepLast && j.limit > 0 {
		for _, r := range j.retainLast() {
			if !sink(r) {
				return
			}
		}
		return
	}
	seen := 0
	onlineMerge(j.ch, j.shardCount, j.mergeKey, func(row *pb.ResultRow) bool {
		if j.limit > 0 && seen >= j.limit {
			return false
		}
		seen++
		return sink(row)
	})
}

// streamErr is set BEFORE the channel is closed so consumers see the
// error after the channel signals exhaustion.
func (j *Job) consume() {
	defer close(j.streamCh)
	// Runs before the close: every path below returns only after the merge
	// has seen every shard's sentinel.
	defer func() {
		if err := j.shardFailure(); err != nil {
			j.streamErr = err
		}
	}()

	if len(j.stages) == 0 {
		if j.traceTag {
			j.emitTraceTagged()
			return
		}
		emitted := 0
		j.feed(func(row *pb.ResultRow) bool {
			emitted++
			j.streamCh <- Line{Raw: row.Raw, LineNum: int64(emitted)}
			return true
		})
		j.streamErr = j.feedErr
		return
	}

	// pipefail: warp reports a pipeline's status as its last stage's, so
	// without it `awk '<bad>' | wc -l` exits 0 from wc and the awk error
	// is dropped (the user sees 0 instead of the syntax error). Stages
	// that fail without writing to stderr (grep with no match) still
	// aren't errors: the exit check below also needs stderr output.
	pipeline := "set -o pipefail; " + strings.Join(j.stages, " | ")
	stdinR, stdinW := newChunkPipe(pipeLimit)
	stdoutR, stdoutW := newChunkPipe(pipeLimit)

	// awkVarsCh carries each row's meta-var values, in the exact order
	// rows are written to stdinW, to an AwkPerRecordVars hook the
	// Runner goroutine installs — awk has no per-row side channel like
	// jq's JSON-body splice (a bare reserved name is just an ordinary
	// awk global, unrelated to the record's own text), so this queue is
	// the correlation mechanism instead: one send per row written, with
	// the records awk reads from it (a record per line), in the same
	// order.
	// Only allocated when the first stage is awk and names a meta-variable:
	// the common case skips it, and the per-row work of building the values,
	// entirely, since nothing would ever drain it (a full buffer would then
	// wedge the writer goroutine).
	var awkVarsCh chan rowAwkVars
	// awkFree returns a finished row's binding slice to the writer, so a row
	// allocates none once the queue has filled.
	var awkFree chan []awk.Binding
	if firstStageIsAwk(j.stages) && j.meta.Reads() {
		awkVarsCh = make(chan rowAwkVars, 1024)
		awkFree = make(chan []awk.Binding, 1024)
	}
	// The same for jq: its variables for each input, in the order its inputs
	// are written.
	var jqVarsCh chan map[string]jq.Value
	if j.jqBinder != nil {
		jqVarsCh = make(chan map[string]jq.Value, 1024)
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
	// stopped is closed once the pipeline has finished reading, so a writer
	// waiting to queue a row's variables for a stage that will never read them
	// (a BEGIN-only awk, jq -n) lets go.
	stopped := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer stdinW.Close()
		if awkVarsCh != nil {
			defer close(awkVarsCh)
		}
		if jqVarsCh != nil {
			defer close(jqVarsCh)
		}
		j.feed(func(row *pb.ResultRow) bool {
			in := j.stageInput(row)
			switch {
			case awkVarsCh != nil:
				// Queued before awk can read the row: a record waiting
				// on its set while the writer waits on awk would
				// deadlock.
				var buf []awk.Binding
				select {
				case buf = <-awkFree:
				default:
				}
				v := rowAwkVars{binds: j.binder.bind(row, buf), records: strings.Count(in, "\n") + 1}
				select {
				case awkVarsCh <- v:
				case <-stopped:
					return false
				}
			case jqVarsCh != nil:
				switch {
				case strings.TrimSpace(in) == "":
					// Nothing for jq to read: no input, so no variables.
				case !gjson.Valid(in):
					// jq reads variables by counting inputs, so a payload
					// that is not exactly one JSON value (two, or none that
					// parse) can't be let through: jq would read it as
					// values of its own and every later row would take the
					// wrong variables. A byte JSON never starts with fails
					// the run there, as the invalid payload always has.
					in = "\x00" + in
				default:
					select {
					case jqVarsCh <- j.jqBinder.Vars(rowMetaVars(row, j.meta)):
					case <-stopped:
						return false
					}
				}
			}
			if _, err := io.WriteString(stdinW, in); err != nil {
				return false
			}
			if _, err := stdinW.Write(newline); err != nil {
				return false
			}
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
		// jq prints compact (one value per line) by default in weft:
		// its output feeds line-oriented stages, and it matches what a
		// projection SQL computes. See docs/plans/planner-ir.md, "jq".
		opts := bash.StreamingOptions{JqCompact: true}
		if jqVarsCh != nil {
			opts.JqInputVars = func() map[string]jq.Value { return <-jqVarsCh }
		}
		if awkVarsCh != nil {
			var cur rowAwkVars
			opts.AwkPerRecordBind = func(string) []awk.Binding {
				if cur.records == 0 {
					// The previous row's records have all been bound, and awk
					// copied the values, so its slice can serve a later row.
					if cur.binds != nil {
						select {
						case awkFree <- cur.binds:
						default:
						}
					}
					cur = <-awkVarsCh
				}
				cur.records--
				return cur.binds
			}
		}
		exitCode, err = bash.RunStreamingWithOptions(pipeline, stdinR, stdoutW, &stderrBuf, opts)
		// A pipeline that stopped reading early (head) must not strand the
		// writer: its writes now fail, and it drains the remaining shards to
		// their sentinels without emitting.
		stdinR.Close()
		close(stopped)
		if err != nil {
			runErrCh <- fmt.Errorf("compute: %w", err)
			return
		}
		// jq's 5 means some inputs failed to evaluate and the rest ran:
		// those rows are skipped, as the shard skips them. Any other
		// failure with a message fails the query.
		if exitCode != 0 && exitCode != bash.ExitJqInputErrors && stderrBuf.Len() > 0 {
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
		// Drain runner and writer before returning so we don't leak them.
		<-runErrCh
		<-writerDone
		return
	}
	if err := <-runErrCh; err != nil {
		j.streamErr = err
	}
	<-writerDone
	if j.feedErr != nil {
		j.streamErr = j.feedErr
	}
}
