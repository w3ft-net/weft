//go:build test

package coordtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/shard"
)

// resultText is the text of a drained result, falling back to its rows' raw
// lines when the pipeline left no text.
func resultText(d *coordinator.DrainedResult) string {
	if d.Text != "" {
		return d.Text
	}
	var sb strings.Builder
	for _, r := range d.Rows {
		sb.WriteString(r.Raw)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// Projections runs the pipelines that narrow a row to some of its fields (cut,
// awk print, jq construction) and checks the output is what the narrowed row
// would have been, and that a query with no projection is unchanged.
func Projections(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"CutSingleField", func(t *testing.T, f *queryFixture) {
			lines := nonEmptyLines(resultText(f.exec(t, `cat vimgym.log | cut -d' ' -f3`, f.full())))
			if len(lines) == 0 {
				t.Fatal("expected output lines")
			}
			for i, l := range lines {
				if l != "vimgym" {
					t.Errorf("line %d: expected 'vimgym', got %q", i, l)
				}
			}
		}},
		{"CutMultipleFields", func(t *testing.T, f *queryFixture) {
			lines := nonEmptyLines(resultText(f.exec(t, `cat vimgym.log | grep ERROR | cut -d' ' -f1,2`, f.full())))
			for _, l := range lines {
				fields := strings.Fields(l)
				if len(fields) != 2 || fields[1] != "ERROR" {
					t.Errorf("expected 'TIMESTAMP ERROR', got %q", l)
				}
			}
			if len(lines) != 5 {
				t.Errorf("expected 5 lines, got %d", len(lines))
			}
		}},
		{"CutOpenEnded", func(t *testing.T, f *queryFixture) {
			lines := nonEmptyLines(resultText(f.exec(t, `cat vimgym.log | grep ERROR | cut -d' ' -f5-`, f.full())))
			if len(lines) != 5 {
				t.Errorf("expected 5 lines, got %d", len(lines))
			}
			for _, l := range lines {
				if strings.HasPrefix(l, "2026") {
					t.Errorf("line should not start with timestamp: %q", l)
				}
			}
		}},
		{"CutThenSort", func(t *testing.T, f *queryFixture) {
			lines := nonEmptyLines(resultText(f.exec(t, `cat vimgym.log | cut -d' ' -f2 | sort | uniq -c`, f.full())))
			if len(lines) == 0 {
				t.Fatal("expected output")
			}
			for _, l := range lines {
				if len(strings.Fields(l)) != 2 {
					t.Errorf("expected 'COUNT LEVEL', got %q", l)
				}
			}
		}},
		{"AwkPrintField", func(t *testing.T, f *queryFixture) {
			for _, l := range nonEmptyLines(resultText(f.exec(t, `cat vimgym.log | awk '{ print $3 }'`, f.full()))) {
				if l != "vimgym" {
					t.Errorf("expected 'vimgym', got %q", l)
				}
			}
		}},
		{"JqObjectConstruction", func(t *testing.T, f *queryFixture) {
			lines := nonEmptyLines(resultText(f.exec(t, `cat vimgym.log | awk 'LEVEL == "ERROR"' | jq '{action: .action}'`, f.full())))
			if len(lines) == 0 {
				t.Fatal("expected output")
			}
			for _, l := range lines {
				if !strings.Contains(l, "action") {
					t.Errorf("expected 'action' in output, got %q", l)
				}
			}
		}},
		{"NoProjectionRegression", func(t *testing.T, f *queryFixture) {
			if d := f.exec(t, `cat vimgym.log | awk 'LEVEL == "ERROR"'`, f.full()); len(d.Rows) != 5 {
				t.Errorf("expected 5 rows, got %d", len(d.Rows))
			}
			if d := f.exec(t, `cat warp.log | grep timeout | wc -l`, f.full()); strings.TrimSpace(d.Text) != "5" {
				t.Errorf("expected 5, got %q", d.Text)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newQueryFixture(t, newSubject(t))) })
	}
}

// Streaming checks how a result reaches its consumer: through the log-query
// rewriter, live rather than buffered to EOF, with the alert and staged paths
// post-processing, and a shard that cannot answer failing the query.
func Streaming(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"LogQueryFlowsThroughRewriter", func(t *testing.T, f *queryFixture) {
			if d := f.exec(t, `cat vimgym.log`, f.full()); len(d.Rows) == 0 && d.Text == "" {
				t.Fatal("expected Execute to produce vimgym rows via the rewriter callback")
			}
		}},
		// The first line reaches the consumer before the script has finished: a
		// pipeline that trickles must not buffer to EOF.
		{"StreamsLiveThroughRunLocal", func(t *testing.T, f *queryFixture) {
			res, err := f.c.Execute(context.Background(), `cat vimgym.log`, f.full(), f.s.ActorOf(""))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Stream.Close()
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
		}},
		// Log lines are non-numeric to alert, so stderr is the signal that the
		// alert stage ran at all.
		{"AlertPathPostProcesses", func(t *testing.T, f *queryFixture) {
			if d := f.exec(t, `cat vimgym.log | alert above 100`, f.full()); d.Stderr == "" {
				t.Fatal("alert path should produce stderr output from the alert stage")
			}
		}},
		{"StagedPipelinePopulatesText", func(t *testing.T, f *queryFixture) {
			if d := f.exec(t, `cat vimgym.log | sort | wc -l`, f.full()); d.Text == "" {
				t.Fatal("staged path should populate Text from the pipeline's stdout")
			}
		}},
		// A query whose answer is missing a shard's rows has no answer: it fails,
		// naming the shard, rather than returning what the others had.
		{"ShardErrorFailsTheQuery", func(t *testing.T, f *queryFixture) {
			for _, pipeline := range []string{`cat vimgym.log`, `cat vimgym.log | sort`, `cat vimgym.log | head -n 1`} {
				broken, err := shard.NewDuckDBShard("shard-1", "")
				if err != nil {
					t.Fatal(err)
				}
				broken.Close()
				f.register(t, &logdb.Shard{
					ShardRow: logdb.ShardRow{ID: "shard-1", WindowStart: f.t1, WindowEnd: f.t2, State: logdb.StateIngesting, BucketID: f.bucketID, CreatedAt: time.Now()},
					Live:     broken,
				})
				f.requireFails(t, pipeline, "shard-1")
			}
		}},
		// A shard that holds rows for the range but can't be queried (its instance
		// died, or it is upgrading) is not left out of the answer.
		{"UnavailableShardFailsTheQuery", func(t *testing.T, f *queryFixture) {
			for _, st := range []logdb.ShardState{logdb.StateSuspended, logdb.StateUpgrading} {
				f.register(t, &logdb.Shard{
					ShardRow: logdb.ShardRow{ID: "shard-1", WindowStart: f.t1, WindowEnd: f.t2, State: st, Addr: "10.0.0.9:9000", RowCount: 25, BucketID: f.bucketID, CreatedAt: time.Now()},
				})
				f.requireFails(t, `cat vimgym.log`, "shard-1")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newQueryFixture(t, newSubject(t))) })
	}
}

// requireFails runs pipeline over the full range and fails the test unless the
// query errors with a message naming shardID.
func (f *queryFixture) requireFails(t *testing.T, pipeline, shardID string) {
	t.Helper()
	res, err := f.c.Execute(context.Background(), pipeline, f.full(), f.s.ActorOf(""))
	if err == nil {
		d, drainErr := res.Drain(context.Background())
		if drainErr == nil {
			t.Fatalf("%s succeeded with %d lines although %s's rows are missing (stderr %q)",
				pipeline, len(d.Rows)+len(nonEmptyLines(d.Text)), shardID, d.Stderr)
		}
		err = drainErr
	}
	if !strings.Contains(err.Error(), shardID) {
		t.Fatalf("%s failed with %q, which doesn't name %s", pipeline, err, shardID)
	}
}
