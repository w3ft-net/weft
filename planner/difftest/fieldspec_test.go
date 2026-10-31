package difftest

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// fieldProgram are awk stages that stay in compute and read only some fields:
// positions, $NF, NF, none at all.
var fieldPrograms = []string{
	`awk '{ n++ } END { print n }'`,
	`awk '{ s += $2 } END { print s }'`,
	`awk '{ s += $2; c[$3]++ } END { print s; for (k in c) print k, c[k] }'`,
	`awk '{ printf "[%s][%s][%d]\n", $1, $NF, NF }'`,
	`awk '{ printf "%s|%s|%s\n", $3, $2, NF }'`,
	`awk '{ n += NF } END { print n }'`,
	`awk '{ a[$NF] = a[$NF] + 1 } END { for (k in a) print k "=" a[k] }'`,
	`awk '{ if (length($1) > m) m = length($1) } END { print m }'`,
	`awk '{ s = s "," $2 } END { print s }'`,
	`awk 'NR % 2 == 0 { printf "%d:%s\n", NR, $1 }'`,
}

// fieldLines are stored lines (a row is a line) of no fields, one, and many.
func fieldLines() []*pb.LogRow {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for i, raw := range []string{
		"", "   ", "a", "a 1", "b 2 x", "c 3 y z", "  lead  7  trail ", "h\ti\tj", "naïve 12 café", "日本語 5 テスト",
		"d 4 e 5 f 6 g", "k 9 l", "m 10", "12 5", "x y z w v u t s",
	} {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "spec", Service: "api", Kind: "main", CustomerId: 1,
		})
	}
	return ingest.SplitLines(rows)
}

// runFieldForm sends the request a coordinator sends for a stage that stays in
// compute: the text-record sql every component understands, plus the field
// SQL and spec for those that do.
func (h *harness) runFieldForm(plan *planner.QueryPlan) (string, error) {
	return h.dispatch(&pb.ExecuteQueryRequest{
		Sql:               plan.DirectShardSQL,
		FieldSql:          plan.FieldSQL,
		FieldSpec:         compute.FieldSpecToProto(plan.FieldSpec),
		Args:              argStrings(plan.Args),
		Stages:            plan.ComputeStages,
		RowStages:         plan.RowStages,
		StagesReadPayload: plan.StagesReadPayload,
		MergeKey:          plan.ShardOrderBy,
		Limit:             int32(plan.Limit),
		KeepLast:          plan.TailMode,
	})
}

// A stage that stays in compute and reads some fields gets, from a shard that
// understands field SQL, the values of those fields and nothing else, and
// compute rebuilds the record the stage reads. The answer is the reference's
// whichever shards understand it: all of them, none, or a mix as during a roll.
func TestDifferentialFieldRowSpec(t *testing.T) {
	for _, mode := range []struct {
		name  string
		old   func(i int) bool
		asked int32 // shards that should be asked in the field form
	}{
		{"all shards new", func(int) bool { return false }, 2},
		{"one old shard", func(i int) bool { return i == 1 }, 1},
		{"all shards old", func(int) bool { return true }, 0},
	} {
		h := newHarnessWith(t, fieldLines(), 2, mode.old)
		for _, prog := range fieldPrograms {
			pipeline := "cat /log/spec/api/main.log | " + prog
			t.Run(mode.name+"/"+prog, func(t *testing.T) {
				plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				if plan.FieldSQL == "" || plan.FieldSpec == nil {
					t.Fatalf("no field form planned:\n%s", planner.Explain(pipeline))
				}
				source, stages := splitSource(t, pipeline)
				want, wantErr := h.runReference(source, stages, readsMetaVars(stages))
				h.fieldQueries.Store(0)
				got, gotErr := h.runFieldForm(plan)
				if n := h.fieldQueries.Load(); n != mode.asked {
					t.Errorf("%d shards were asked in the field form, want %d", n, mode.asked)
				}
				if (gotErr != nil) != (wantErr != nil) {
					t.Fatalf("error %v, reference error %v", gotErr, wantErr)
				}
				if plan.ShardOrderBy == "" {
					got, want = sortedLines(got), sortedLines(want)
				}
				if got != want {
					t.Errorf("differs from the reference: %s", firstDiff(got, want))
				}
			})
		}
	}
}

// A flag changes how awk splits a line or what its variables hold, so a stage
// that has one is not cut down to the whitespace-split fields it names.
func TestDifferentialFieldProjectionRespectsAwkFlags(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for i, raw := range []string{"a,1 x", "b,2,y z", "c,3", "d e,4,5", "f,6,7,8 9"} {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "spec", Service: "api", Kind: "main", CustomerId: 1,
		})
	}
	h := newHarness(t, rows, 2)
	for _, prog := range []string{
		`awk -F, '{ s += $2 } END { print s }'`,
		`awk -F, '{ printf "%s;%d\n", $1, NF }'`,
		`awk -F, '{ n += NF } END { print n }'`,
		`awk -v k=2 '{ s += $2 * k } END { print s }'`,
		`awk -v FS=, '{ s += $2 } END { print s }'`,
		`awk -v k=1 -v FS=, '{ printf "%s;%d\n", $1, NF }'`,
	} {
		t.Run(prog, func(t *testing.T) { h.check(t, "cat /log/spec/api/main.log | "+prog) })
	}
}

// -v sets a variable and leaves the split alone, so the stage is still cut down
// to its fields and gets the field form.
func TestDifferentialFieldFormOfAStageWithVars(t *testing.T) {
	h := newHarness(t, fieldLines(), 2)
	pipeline := `cat /log/spec/api/main.log | awk -v k=2 -v name=x '{ s += $2 * k } END { print name, s }'`
	plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil || plan.FieldSQL == "" || plan.FieldSpec == nil {
		t.Fatalf("no field form: %v\n%s", err, planner.Explain(pipeline))
	}
	source, stages := splitSource(t, pipeline)
	want, wantErr := h.runReference(source, stages, readsMetaVars(stages))
	if wantErr != nil {
		t.Fatal(wantErr)
	}
	h.fieldQueries.Store(0)
	got, err := h.runFieldForm(plan)
	if err != nil {
		t.Fatal(err)
	}
	if h.fieldQueries.Load() != 2 {
		t.Errorf("%d shards asked in the field form, want 2", h.fieldQueries.Load())
	}
	if got != want {
		t.Errorf("differs from the reference: %s", firstDiff(got, want))
	}
}
