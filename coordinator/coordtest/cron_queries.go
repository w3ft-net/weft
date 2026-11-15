//go:build test

package coordtest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// CronQueries runs the suites of a cron whose pipeline reads logs: its run fails
// when a shard's rows are missing, and a plan lowered at save time answers as an
// inline plan does. Each test gets a fresh Subject over the query fixture.
func CronQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"MissingShardFailsTheRun", cronMissingShardFailsTheRun},
		{"LoweredHotPath", cronLoweredHotPath},
		{"LoweredVersionMismatchFallsBack", cronLoweredVersionMismatch},
		{"LoweredPlanSendsTheFieldForm", cronLoweredPlanSendsTheFieldForm},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newQueryFixture(t, newSubject(t))) })
	}
}

// A cron whose log query is missing a shard's rows fails its run: no value is
// stored, and a script doesn't carry on to print one from what it had. (The
// query's error was once a stderr line, so the run was recorded as an alert and
// the script's later output stored as its value.)
func cronMissingShardFailsTheRun(t *testing.T, f *queryFixture) {
	// shard-1 is suspended: its rows are unreachable.
	f.register(t, &logdb.Shard{ShardRow: logdb.ShardRow{
		ID: "shard-1", WindowStart: f.t1, WindowEnd: f.t2, State: logdb.StateSuspended,
		Addr: "10.0.0.9:9000", RowCount: 25, CreatedAt: time.Now(),
	}})
	var pages []string
	ce := f.s.executor(f.c, nil, 5*time.Second, &pages)

	for name, pipeline := range map[string]string{
		"count":  "cat vimgym.log | wc -l",
		"script": "cat vimgym.log | wc -l\necho 5",
	} {
		t.Run(name, func(t *testing.T) {
			f.s.saveCron(t, name, "every 5m", pipeline)
			w := f.s.watch(t, name)
			ce.ExecuteWatch(context.Background(), w, f.full())

			w = f.s.watch(t, name)
			if w.LastStatus != cron.StatusError {
				t.Fatalf("status = %q, want error", w.LastStatus)
			}
			rows, err := f.s.Crons.ListResults(w.ID, 10, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Value != nil || !strings.Contains(r.Stderr, "shard-1") {
					t.Fatalf("the failed run stored %+v; want no value and an error naming shard-1", r)
				}
			}
		})
	}
}

// loweredWatch is a watch of pipeline carrying the plan the save path would
// have stored for it.
func (f *queryFixture) loweredWatch(t *testing.T, name, pipeline string, mutate func(*planner.LoweredPlan)) cron.Watch {
	t.Helper()
	lp, err := planner.Lower(pipeline, shape.Set{f.s.Owner.Coord()}, "bolt", noBins{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if mutate != nil {
		mutate(lp)
	}
	lowered, err := json.Marshal(lp)
	if err != nil {
		t.Fatal(err)
	}
	return cron.Watch{
		Owner: f.s.Owner, Name: name,
		WatchSpec: cron.WatchSpec{Schedule: "every 1m", Pipeline: pipeline, Enabled: true, LoweredPlan: lowered},
	}
}

// noBins is the BinResolver of tests that don't inline /bin scripts.
type noBins struct{}

func (noBins) Source(string) (string, string, bool) { return "", "", false }

func (f *queryFixture) forWatch(t *testing.T, w cron.Watch) *coordinator.DrainedResult {
	t.Helper()
	res, err := f.c.ExecuteForWatch(context.Background(), w.Pipeline, f.full(), &w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A watch whose lowered plan is populated gives the same rows as the inline-plan
// path. This is the smoke test that the save-time-lowering shortcut produces
// correct output: if it ever ships SQL that diverges from a fresh plan, this
// catches the drift.
func cronLoweredHotPath(t *testing.T, f *queryFixture) {
	w := f.loweredWatch(t, "lowered-smoke", `cat /log/vimgym.log | grep ERROR`, nil)
	lowered := f.forWatch(t, w).Rows

	w.LoweredPlan = nil // forces the fallback path
	fresh := f.forWatch(t, w).Rows

	if len(lowered) == 0 {
		t.Fatal("lowered path returned no rows; expected ERROR rows from vimgym")
	}
	if len(lowered) != len(fresh) {
		t.Fatalf("lowered vs fresh row count mismatch: lowered=%d fresh=%d", len(lowered), len(fresh))
	}
	for i := range lowered {
		if lowered[i].Raw != fresh[i].Raw {
			t.Errorf("row[%d] differs:\n lowered: %s\n   fresh: %s", i, lowered[i].Raw, fresh[i].Raw)
		}
	}
	// Every row matched the filter: the SQL really applied the predicate.
	for _, r := range lowered {
		if !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected ERROR in row, got %q", r.Raw)
		}
	}
}

// A watch with a lowered plan stamped at an older planner version must not use
// the stored plan: it would be lying about a SQL shape the current planner no
// longer produces. The fallback path re-lowers. The version doesn't change the
// SQL today, so this exists to fail loudly if anyone removes the check.
func cronLoweredVersionMismatch(t *testing.T, f *queryFixture) {
	w := f.loweredWatch(t, "version-mismatch", `cat /log/vimgym.log | grep ERROR`, func(lp *planner.LoweredPlan) {
		lp.PlannerVersion = planner.PlannerVersion + 100
	})
	if rows := f.forWatch(t, w).Rows; len(rows) == 0 {
		t.Fatal("expected ERROR rows; got 0")
	}
}

// spyCompute remembers the last request that reached compute.
type spyCompute struct {
	inner   compute.Executor
	mu      sync.Mutex
	lastReq *pb.ExecuteQueryRequest
}

func (p *spyCompute) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	return p.inner.ExecuteQuery(ctx, req)
}

// A cron whose awk stage reads only some fields sends compute the field form from
// its lowered plan, as an interactive query does, and gets the same answer as a
// fire without the lowered plan.
func cronLoweredPlanSendsTheFieldForm(t *testing.T, f *queryFixture) {
	spy := &spyCompute{inner: f.c.Compute}
	f.c.Compute = spy

	w := f.loweredWatch(t, "lowered-fields", `cat /log/vimgym.log | awk '{ n += $2 } END { print n }'`, nil)
	lowered := f.forWatch(t, w)
	spy.mu.Lock()
	req := spy.lastReq
	spy.mu.Unlock()
	if req.FieldSql == "" || req.FieldSql == req.Sql || len(req.FieldSpec.GetPositions()) != 1 || req.FieldSpec.Positions[0] != 2 {
		t.Errorf("lowered fire sent field sql %q spec %v beside sql %q", req.FieldSql, req.FieldSpec, req.Sql)
	}

	w.LoweredPlan = nil
	fresh := f.forWatch(t, w)
	if lowered.Text != fresh.Text || len(lowered.Rows) != len(fresh.Rows) {
		t.Errorf("lowered fire %q differs from the fresh one %q", lowered.Text, fresh.Text)
	}
}
