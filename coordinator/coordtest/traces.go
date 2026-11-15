//go:build test

package coordtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// TraceQueries runs the trace command: the list of traces is exact across
// shards, a bare `trace --id` is tagged for display when typed and never when a
// cron fires it, and the span queries are aggregates.
func TraceQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	seeded := func(run func(*testing.T, *queryFixture)) func(*testing.T) {
		return func(t *testing.T) { run(t, newQueryFixture(t, newSubject(t))) }
	}
	listed := func(run func(*testing.T, *queryFixture)) func(*testing.T) {
		return func(t *testing.T) { run(t, newTraceListFixture(t, newSubject(t))) }
	}

	t.Run("TagsInteractiveOutput", seeded(func(t *testing.T, f *queryFixture) {
		log := f.logRequests()
		f.exec(t, `trace --id abc-123`, f.full())
		if req := log.last(t); !req.TraceTag {
			t.Error("interactive `trace --id abc-123` should set TraceTag, so compute shows the summary line and badges")
		}
	}))

	// A cron saved as `trace --id` has no lowered plan and runs through the cold
	// path: a summary row and ANSI codes in its output would corrupt what alert
	// and dashboards read.
	for _, pipeline := range []string{"trace --id abc-123", "trace --spans"} {
		t.Run("CronDoesNotTagOutput/"+pipeline, seeded(func(t *testing.T, f *queryFixture) {
			log := f.logRequests()
			f.s.saveCron(t, "trace-watch", "every 1m", pipeline)
			w := f.s.watch(t, "trace-watch")
			res, err := f.c.ExecuteForWatch(context.Background(), pipeline, f.full(), &w)
			if err != nil {
				t.Fatalf("ExecuteForWatch: %v", err)
			}
			if _, err := res.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if req := log.last(t); req.TraceTag {
				t.Errorf("a cron's %q must not set TraceTag", pipeline)
			}
		}))
	}

	// The span queries and the list return JSON rows, so none is tagged.
	for _, pipeline := range []string{"trace --spans", "trace --spans --name shard.Query", "trace --spans --id abc-123", "trace"} {
		t.Run("AggregatesAreNotTagged/"+pipeline, seeded(func(t *testing.T, f *queryFixture) {
			log := f.logRequests()
			f.exec(t, pipeline, f.full())
			req := log.last(t)
			if req.TraceTag {
				t.Errorf("%q must not set TraceTag", pipeline)
			}
			if req.Aggregate == "" {
				t.Errorf("%q should run as a shard-side aggregate", pipeline)
			}
		}))
	}

	t.Run("ListIsExactAcrossShards", listed(func(t *testing.T, f *queryFixture) {
		log := f.logRequests()
		got := parseListedTraces(t, f.exec(t, "trace", f.full()).Text)
		if len(got) != 50 {
			t.Fatalf("want the newest 50 traces, got %d", len(got))
		}
		if got[0].TraceID != "x" || got[0].Lines != 2 {
			t.Errorf("x has a line on each shard and the newest of all, got %+v", got[0])
		}
		if strings.Join(got[0].Services, ",") != "vimgym/api,vimgym/worker" {
			t.Errorf("x services = %v", got[0].Services)
		}
		if len(log.reqs) != 2 {
			t.Fatalf("want two compute calls (ids, then stats), got %d", len(log.reqs))
		}
		if req := log.last(t); req.Aggregate != "trace_list" || req.TraceTag {
			t.Errorf("second call = aggregate %q tag %v", req.Aggregate, req.TraceTag)
		}
	}))

	t.Run("ListPipesIntoStages", listed(func(t *testing.T, f *queryFixture) {
		lines := nonEmptyLines(f.exec(t, `trace | jq -r '.trace_id' | head -n 3`, f.full()).Text)
		if len(lines) != 3 || lines[0] != "x" {
			t.Fatalf("got %v", lines)
		}
	}))

	t.Run("ListScopedToANamespace", listed(func(t *testing.T, f *queryFixture) {
		got := parseListedTraces(t, f.exec(t, "trace vimgym/worker", f.full()).Text)
		if len(got) != 1 || got[0].TraceID != "x" || got[0].Lines != 2 {
			t.Fatalf("only x touched vimgym/worker, and its summary covers both shards: %+v", got)
		}
	}))

	// A saved cron can't be lowered for the list; it runs the ordinary two-step
	// path and is never tagged.
	t.Run("CronListRuns", listed(func(t *testing.T, f *queryFixture) {
		f.s.saveCron(t, "recent-traces", "every 1m", "trace")
		w := f.s.watch(t, "recent-traces")
		res, err := f.c.ExecuteForWatch(context.Background(), "trace", f.full(), &w)
		if err != nil {
			t.Fatalf("ExecuteForWatch: %v", err)
		}
		d, err := res.Drain(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := parseListedTraces(t, d.Text); len(got) != 50 {
			t.Fatalf("want 50 traces, got %d", len(got))
		}
	}))

	t.Run("ListEmptyRunsOneStep", seeded(func(t *testing.T, f *queryFixture) { // its rows carry no trace IDs
		log := f.logRequests()
		if d := f.exec(t, "trace", f.full()); d.Text != "" || len(d.Rows) != 0 {
			t.Errorf("want no output, got %+v", d)
		}
		if len(log.reqs) != 1 {
			t.Errorf("with no trace IDs there is nothing to summarize: want 1 compute call, got %d", len(log.reqs))
		}
	}))

	// The old form `trace <id>` reads as a namespace, and no namespace has that name.
	t.Run("OldTraceFormIsAnUnknownNamespace", seeded(func(t *testing.T, f *queryFixture) {
		stderr := f.exec(t, "trace 4bf92f3577b34da6a3ce929d0e0e4736", f.full()).Stderr
		if !strings.Contains(stderr, "unknown source: 4bf92f") {
			t.Fatalf("want an unknown-source message naming the id, got %q", stderr)
		}
	}))
}

// logRequests makes the fixture's compute record each request it is sent.
func (f *queryFixture) logRequests() *requestLog {
	log := &requestLog{inner: f.c.Compute}
	f.c.Compute = log
	return log
}

func (r *requestLog) last(t *testing.T) *pb.ExecuteQueryRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("ExecuteQuery was never called")
	}
	return r.reqs[len(r.reqs)-1]
}

// newTraceListFixture has trace "x" on both of two time-windowed shards, and 60
// busier traces on the first, so x is not among the first shard's newest 50
// although it is the newest overall.
func newTraceListFixture(t *testing.T, s Subject) *queryFixture {
	t.Helper()
	f := newBareFixture(t, s)
	row := func(at time.Time, service, traceID string) *pb.LogRow {
		r := &pb.LogRow{
			Raw: "line for " + traceID, TsUnixNs: at.UnixNano(), Level: "INFO",
			Namespace: "vimgym", Service: service, Kind: "main", TraceId: traceID,
		}
		if s.Stamp != nil {
			s.Stamp(r)
		}
		return r
	}
	busy := []*pb.LogRow{row(f.t0.Add(time.Second), "api", "x")}
	for i := 0; i < 60; i++ {
		busy = append(busy, row(f.t0.Add(time.Duration(2+i)*time.Second), "api", fmt.Sprintf("o-%02d", i)))
	}
	for _, sh := range []struct {
		id         string
		start, end time.Time
		rows       []*pb.LogRow
		serves     string
	}{
		{"tl-a", f.t0, f.t1, busy, "vimgym/api"},
		{"tl-b", f.t1, f.t2, []*pb.LogRow{row(f.t1.Add(time.Second), "worker", "x")}, "vimgym/worker"},
	} {
		live, err := shard.NewDuckDBShard(sh.id, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { live.Close() })
		if err := live.Ingest(context.Background(), sh.rows, nil); err != nil {
			t.Fatal(err)
		}
		f.registerServing(t, &logdb.Shard{
			ShardRow: logdb.ShardRow{ID: sh.id, WindowStart: sh.start, WindowEnd: sh.end, State: logdb.StateIngesting, CreatedAt: time.Now()},
			Live:     live,
		}, sh.serves)
	}
	return f
}

type listedTrace struct {
	TraceID  string   `json:"trace_id"`
	Lines    int64    `json:"lines"`
	Services []string `json:"services"`
}

func parseListedTraces(t *testing.T, text string) []listedTrace {
	t.Helper()
	var out []listedTrace
	for _, l := range nonEmptyLines(text) {
		var lt listedTrace
		if err := json.Unmarshal([]byte(l), &lt); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, lt)
	}
	return out
}
