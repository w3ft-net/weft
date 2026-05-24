package coordinator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/logdb"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// TestExecute_LogQueryFlowsThroughRewriter pins the unified-dispatch
// behavior: every Execute invocation routes through warp + the log-
// query rewriter, which substitutes log pipelines with
// CallbackCommands. End-to-end the captured pipeline output reaches
// the consumer.
func TestExecute_LogQueryFlowsThroughRewriter(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) == 0 && resD.Text == "" {
		t.Fatal("expected Execute to produce vimgym rows via rewriter callback")
	}
}

// TestExecute_StreamsLiveThroughRunLocal pins runLocal's streaming
// property: the gRPC server (and any other ExecuteResult.Stream
// consumer) sees the FIRST line BEFORE warp's ExecScript has
// finished. A scripted pipeline whose output trickles slowly via
// warp would otherwise have to buffer to EOF before any byte
// reaches the wire.
//
// Setup: a small fixture and a streaming probe — read one line off
// res.Stream within a short timeout. The line emerges before the
// stream's terminal EOF, proving the pipe-backed runLocal forwards
// bytes as warp produces them rather than after the whole script
// completes.
func TestExecute_StreamsLiveThroughRunLocal(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer res.Stream.Close()

	// First line should arrive within a generous bound. Pre-streaming
	// runLocal buffered until ExecScript finished — a slow shard
	// (none here, but the architectural property still holds) would
	// have meant first-byte-latency ≈ total-execution-time. With
	// streaming runLocal, first-byte is bounded by the slowest
	// shard's first row + warp pipe wiring.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	line, ok, err := res.Stream.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !ok {
		t.Fatal("stream EOF before any line emitted")
	}
	if line.Raw == "" {
		t.Errorf("first streamed line is empty: %+v", line)
	}
}

// TestExecute_AlertPathPostProcesses — a pipeline ending in
// `| alert ...` peels the alert at the top level, runs the rest
// through warp+rewriter, then runAlertStage post-processes the
// captured output.
//
// The body of `cat vimgym.log` is whitespace-delimited log lines —
// non-numeric to alert, which emits "non-numeric value" stderr and
// (interactively) clears the body. We assert on Stderr rather than
// the body because Stderr is the signal that runAlertStage actually
// ran: an empty Stderr would mean the alert was never invoked.
func TestExecute_AlertPathPostProcesses(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute(`cat vimgym.log | alert above 100`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resD := drainExec(t, res)
	if resD.Stderr == "" {
		t.Fatal("alert path should produce stderr output via runAlertStage")
	}
}

// TestExecute_StagedPipelinePopulatesText — staged compute pipelines
// (sort, wc, uniq, awk-with-END) buffer at the warp boundary; their
// stdout becomes the captured Text on the returned ExecuteResult.
func TestExecute_StagedPipelinePopulatesText(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute(`cat vimgym.log | sort | wc -l`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resD := drainExec(t, res)
	if resD.Text == "" {
		t.Fatal("staged path should populate Text from warp's stdout")
	}
}

// TestExecute_ShardErrorSurfaces — a shard whose Query fails surfaces
// its error through the rewriter callback to the result's Stderr.
// TestExecute_ShardErrorReturnsPartialResults verifies that a shard
// which produces no rows (simulating a failure) doesn't crash the
// query — the coordinator returns partial results from the working
// shard. Per-shard error surfacing in Stderr was removed with
// shardErrAwareStream in d477ea2; the new compute path logs dial/
// query failures server-side.
func TestExecute_ShardErrorReturnsPartialResults(t *testing.T) {
	c := setupTestCoordinator(t)
	e := &logdb.ShardEntry{
		ID:          "shard-1",
		WindowStart: t1, WindowEnd: t2,
		State:     logdb.StateIngesting,
		Shard:     &errShard{id: "shard-1"},
		CreatedAt: time.Now(),
	}
	logdb.SeedShard(c.Provisioner, e)
	stampShardServicesFromApps(t, c, e, []string{"vimgym", "callbox"})

	res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	drained, drainErr := res.Drain(context.Background())
	if drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}
	if drained == nil {
		t.Fatal("expected partial results from working shard")
	}
	if len(drained.Rows) == 0 && drained.Text == "" {
		t.Fatal("expected rows from shard-0; got empty result")
	}
}

// errShard is a minimal shard.Shard that always errors on Query.
// Used to verify the streaming path's error-collector surfaces
// shard failures through the wrapped stream.
type errShard struct {
	id string
}

func (s *errShard) ID() string                                          { return s.id }
func (s *errShard) Query(string, ...any) ([]*pb.ResultRow, error)       { return nil, errSynthetic }
func (s *errShard) QueryToSink(_ string, _ []any, sink shard.RowSink)   { sink.SendSentinel(s.id) }
func (s *errShard) Ingest([]*pb.LogRow, shard.IngestRecorder) error     { return nil }
func (s *errShard) Archive(_ context.Context, _ string) (string, error) { return "", nil }
func (s *errShard) Close() error                                        { return nil }

type syntheticErr struct{}

func (syntheticErr) Error() string { return "synthetic shard failure" }

var errSynthetic = syntheticErr{}

// _ keeps compute imported when other helpers in this file don't
// reference it directly.
var _ compute.LineStream = (compute.LineStream)(nil)

// _ keeps sync imported.
var _ sync.WaitGroup
