package difftest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// awkNumbers are spellings awk compares as numbers: every one is what a shard
// would call typed. (1e999 is not one: awk's number parse rejects a value out
// of range, so a shard would mark it dirty.) Several sort above "500" as text, which is what makes the
// base prefilter wide, and the odd forms (hex floats, inf, nan, a leading +,
// a trailing dot, an integer past 2^53) are where DuckDB and Go could disagree.
var awkNumbers = []string{
	"0", "-0", "7", "12", "49", "60", "99", "500", "500.0", "5e2", "+500", "501", "1000", "5000",
	"-3", "-500", ".5", "5.", "1e3", "1E2", "007", "0x1p3", "0X1P-2",
	"inf", "-inf", "Inf", "+Inf", "infinity", "NaN", "nan", "-nan",
	"9007199254740993", "1e308", "4.9e-324", "1e-999",
}

func awkRows() []*pb.LogRow {
	var rows []*pb.LogRow
	for i, v := range awkNumbers {
		for rep := 0; rep < 3; rep++ {
			rows = append(rows, &pb.LogRow{
				Raw:       fmt.Sprintf("id%d %s tail", i, v),
				TsUnixNs:  time.Date(2026, 10, 30, 12, 0, i*3+rep, 0, time.UTC).UnixNano(),
				Namespace: "typed", Service: "api", Kind: "main",
			})
		}
	}
	return rows
}

// runTyped dispatches the plan with the typed variant's shard SQL, the way the
// coordinator does for a shard whose facts allow it.
func (h *harness) runTyped(plan *planner.QueryPlan) (string, error) {
	return h.dispatch(&pb.ExecuteQueryRequest{
		Sql:               plan.Typed.DirectShardSQL,
		Args:              argStrings(plan.Args),
		Stages:            plan.ComputeStages,
		RowStages:         plan.RowStages,
		StagesReadPayload: plan.StagesReadPayload,
		MergeKey:          plan.ShardOrderBy,
		Aggregate:         plan.Aggregate,
		Group:             compute.GroupToProto(plan.Group),
		Limit:             int32(plan.Limit),
		KeepLast:          plan.TailMode,
	})
}

// rowsShipped is how many rows the SQL returns across the harness's shards.
func (h *harness) rowsShipped(t *testing.T, sql string, args []any) int {
	t.Helper()
	n := 0
	for _, sh := range h.shards {
		rows, err := sh.Query(context.Background(), sql, args...)
		if err != nil {
			t.Fatalf("shard SQL %s: %v", sql, err)
		}
		n += len(rows)
	}
	return n
}

func checkTyped(t *testing.T, h *harness, pipeline, locator string) (base, typed int) {
	t.Helper()
	plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	if plan.Typed == nil || len(plan.Typed.Requires) != 1 || plan.Typed.Requires[0] != locator {
		t.Fatalf("%s: typed variant = %+v, want one requiring %s", pipeline, plan.Typed, locator)
	}
	source, stages := splitSource(t, pipeline)
	want, wantErr := h.runReference(source, stages, readsMetaVars(stages))
	for name, run := range map[string]func(*planner.QueryPlan) (string, error){"base": h.runPlan, "typed": h.runTyped} {
		got, gotErr := run(plan)
		if (gotErr != nil) != (wantErr != nil) {
			t.Fatalf("%s %s: error %v, reference error %v", pipeline, name, gotErr, wantErr)
		}
		if plan.ShardOrderBy == "" {
			got, want = sortedLines(got), sortedLines(want)
		}
		if got != want {
			t.Errorf("%s: %s plan differs from the reference: %s", pipeline, name, firstDiff(got, want))
		}
	}
	args := toAny(argStrings(plan.Args))
	return h.rowsShipped(t, plan.DirectShardSQL, args), h.rowsShipped(t, plan.Typed.DirectShardSQL, args)
}

func TestDifferentialTypedAwkComparison(t *testing.T) {
	h := newHarness(t, awkRows(), 2)
	var baseTotal, typedTotal int
	for _, cond := range []string{
		"$2 > 500", "$2 >= 500", "$2 < 50", "$2 <= 7", "$2 == 500", "$2 != 7",
		"$2 == 0", "$2 < 1e3", "$2 > 5e2", "$2 >= 0", "$2 != 500",
		"$2 > 100 && $2 < 600",
	} {
		pipeline := fmt.Sprintf("cat /log/typed/api/main.log | awk '%s'", cond)
		locator := "$2"
		base, typed := checkTyped(t, h, pipeline, locator)
		if typed > base {
			t.Errorf("%s: typed SQL ships %d rows, more than the base's %d", cond, typed, base)
		}
		baseTotal += base
		typedTotal += typed
	}
	if typedTotal >= baseTotal {
		t.Errorf("typed SQL ships %d rows in all, the base %d: no saving", typedTotal, baseTotal)
	}
	t.Logf("rows shipped across the corpus: base %d, typed %d", baseTotal, typedTotal)
}

func jqRows() []*pb.LogRow {
	nums := []string{"0", "-0", "1", "10", "10.0", "10.5", "11", "100", "-5", "1e3", "2.50", "9007199254740993", "1e308", "0.1", "1E2", "-1e-7"}
	var rows []*pb.LogRow
	for i, v := range nums {
		for rep := 0; rep < 3; rep++ {
			rows = append(rows, &pb.LogRow{
				Raw:         "x",
				PayloadJson: []byte(`{"q":` + v + `,"name":"n` + fmt.Sprint(i) + `"}`),
				TsUnixNs:    time.Date(2026, 10, 30, 12, 0, i*3+rep, 0, time.UTC).UnixNano(),
				Namespace:   "typed", Service: "stats", Kind: "main",
			})
		}
	}
	return rows
}

func TestDifferentialTypedJQComparison(t *testing.T) {
	h := newHarness(t, jqRows(), 2)
	for _, cond := range []string{".q > 10", ".q >= 10", ".q < 10", ".q <= 10.5", ".q == 10", ".q != 10", ".q == 0", ".q < 1e3"} {
		pipeline := fmt.Sprintf("cat /log/typed/stats/main.log | jq 'select(%s)'", cond)
		checkTyped(t, h, pipeline, ".q")
	}
}

// A query over an old window and a new one runs the base SQL on some shards
// and the typed SQL on others, in one request. The merged answer is the
// reference's.
func TestDifferentialTypedMixedShards(t *testing.T) {
	h := newHarness(t, awkRows(), 2)
	for _, cond := range []string{"$2 > 500", "$2 <= 7", "$2 == 500", "$2 != 7"} {
		pipeline := fmt.Sprintf("cat /log/typed/api/main.log | awk '%s'", cond)
		plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
		if err != nil || plan.Typed == nil {
			t.Fatalf("%s: %v, typed %+v", pipeline, err, plan.Typed)
		}
		args := argStrings(plan.Args)
		req := &pb.ExecuteQueryRequest{
			Sql: plan.DirectShardSQL, Args: args, Stages: plan.ComputeStages, RowStages: plan.RowStages,
			StagesReadPayload: plan.StagesReadPayload, MergeKey: plan.ShardOrderBy, Aggregate: plan.Aggregate,
			Group: compute.GroupToProto(plan.Group), Limit: int32(plan.Limit), KeepLast: plan.TailMode,
		}
		for i, sh := range h.shards {
			d := &pb.ShardDescriptor{ShardId: sh.ID()}
			if i == 0 { // only the first shard has the facts
				d.Sql, d.Args = plan.Typed.DirectShardSQL, args
			}
			req.Shards = append(req.Shards, d)
		}
		stream, err := h.engine.ExecuteQuery(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := drain(stream)
		if err != nil {
			t.Fatal(err)
		}
		source, stages := splitSource(t, pipeline)
		want, err := h.runReference(source, stages, readsMetaVars(stages))
		if err != nil {
			t.Fatal(err)
		}
		if got, want = sortedLines(got), sortedLines(want); got != want {
			t.Errorf("%s: mixed shards differ from the reference: %s", pipeline, firstDiff(got, want))
		}
	}
}
