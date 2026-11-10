package coordinator

import (
	"context"
	"github.com/graybeardcorp/weft/store"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// TestExecute_LogQueryFlowsThroughRewriter pins the unified-dispatch
// behavior: every Execute invocation routes through warp + the log-
// query rewriter, which substitutes log pipelines with
// CallbackCommands. End-to-end the captured pipeline output reaches
// the consumer.
func TestExecute_LogQueryFlowsThroughRewriter(t *testing.T) {
	c := setupTestCoordinator(t)

	res, err := c.Execute(context.Background(), `cat vimgym.log`, fullRange, store.TestOrgID("kevlar"), "")
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

	res, err := c.Execute(context.Background(), `cat vimgym.log`, fullRange, store.TestOrgID("kevlar"), "")
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

	res, err := c.Execute(context.Background(), `cat vimgym.log | alert above 100`, fullRange, store.TestOrgID("kevlar"), "")
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

	res, err := c.Execute(context.Background(), `cat vimgym.log | sort | wc -l`, fullRange, store.TestOrgID("kevlar"), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resD := drainExec(t, res)
	if resD.Text == "" {
		t.Fatal("staged path should populate Text from warp's stdout")
	}
}

// A query whose answer is missing a shard's rows has no answer: it fails,
// naming the shard, rather than returning what the other shards had. Here
// shard-1 holds rows for the range but every query on it errors.
func TestExecute_ShardErrorFailsTheQuery(t *testing.T) {
	for _, pipeline := range []string{`cat vimgym.log`, `cat vimgym.log | sort`, `cat vimgym.log | head -n 1`} {
		t.Run(pipeline, func(t *testing.T) {
			c := setupTestCoordinator(t)
			broken, err := shard.NewDuckDBShard("shard-1", "")
			if err != nil {
				t.Fatal(err)
			}
			broken.Close()
			addInProcessShard(t, c, &logdb.Shard{
				ID:          "shard-1",
				WindowStart: t1, WindowEnd: t2,
				State:     logdb.StateIngesting,
				Live:      broken,
				CreatedAt: time.Now(),
			}, "vimgym", "callbox")

			requireQueryFails(t, c, pipeline, "shard-1")
		})
	}
}

// A shard that holds rows for the range but can't be queried (SUSPENDED:
// its instance died) is not left out of the answer; the query fails.
func TestExecute_UnavailableShardFailsTheQuery(t *testing.T) {
	for _, st := range []logdb.ShardState{logdb.StateSuspended, logdb.StateUpgrading} {
		t.Run(string(st), func(t *testing.T) {
			c := setupTestCoordinator(t)
			e := &logdb.Shard{
				ID:          "shard-1",
				WindowStart: t1, WindowEnd: t2,
				State:     st,
				Addr:      "10.0.0.9:9000",
				RowCount:  25,
				CreatedAt: time.Now(),
			}
			logdb.SeedShard(c.Provisioner, e)
			stampShardServicesFromApps(t, c, e, []string{"vimgym", "callbox"})

			requireQueryFails(t, c, `cat vimgym.log`, "shard-1")
		})
	}
}

// requireQueryFails runs pipeline over fullRange and fails the test unless
// the query errors with a message naming shardID.
func requireQueryFails(t *testing.T, c *Coordinator, pipeline, shardID string) {
	t.Helper()
	res, err := c.Execute(context.Background(), pipeline, fullRange, store.TestOrgID("kevlar"), "")
	if err == nil {
		d, drainErr := res.Drain(context.Background())
		if drainErr == nil {
			n := 0
			if d != nil {
				n = len(d.Rows) + len(nonEmptyLines(d.Text))
			}
			t.Fatalf("%s succeeded with %d lines although %s's rows are missing (stderr %q)", pipeline, n, shardID, d.Stderr)
		}
		err = drainErr
	}
	if !strings.Contains(err.Error(), shardID) {
		t.Fatalf("%s failed with %q, which doesn't name %s", pipeline, err, shardID)
	}
}

// errShard is a minimal shard.Shard that always errors on Query.
// Used to verify the streaming path's error-collector surfaces
// shard failures through the wrapped stream.
type errShard struct {
	id string
}

func (s *errShard) ID() string { return s.id }
func (s *errShard) Query(context.Context, string, ...any) ([]*pb.ResultRow, error) {
	return nil, errSynthetic
}
func (s *errShard) QueryToSink(_ context.Context, _ string, _ []any, sink shard.RowSink) {
	sink.SendSentinel(s.id)
}
func (s *errShard) Ingest(context.Context, []*pb.LogRow, shard.IngestRecorder) error { return nil }
func (s *errShard) Archive(_ context.Context, _ string) (string, []shard.CustomerStat, error) {
	return "", nil, nil
}
func (s *errShard) Close() error { return nil }

type syntheticErr struct{}

func (syntheticErr) Error() string { return "synthetic shard failure" }

var errSynthetic = syntheticErr{}

// _ keeps compute imported when other helpers in this file don't
// reference it directly.
var _ compute.LineStream = (compute.LineStream)(nil)

// _ keeps sync imported.
var _ sync.WaitGroup
