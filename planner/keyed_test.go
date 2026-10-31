package planner

import (
	"strings"
	"testing"
	"time"
)

func planKeyed(t *testing.T, program string) *QueryPlan {
	t.Helper()
	plan, err := Plan("cat /log/ns/svc/main.log | awk '"+program+"'", 1, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

const (
	keyedSum = `{ s[$1] += $2 } END { for (k in s) print k, s[k] }`
	keyedMax = `{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`
	keyedMin = `{ if ($2 < m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`
)

// A sum, max or min stays a compute stage in the plan that holds anywhere, and
// becomes the shard's GROUP BY in the plan for shards whose facts say the field
// is what the aggregate is exact over: whole numbers for a sum, numbers for a
// max or min.
func TestKeyedAggregate_LowersOnlyUnderItsFacts(t *testing.T) {
	for _, c := range []struct {
		name, program string
		requires      []string
		render        GroupRender
	}{
		{"sum", keyedSum, []string{"$2", "$2#int"}, RenderKeySum},
		{"max", keyedMax, []string{"$2"}, RenderKeyMax},
		{"min", keyedMin, []string{"$2"}, RenderKeyMin},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := planKeyed(t, c.program)
			if plan.Group != nil || len(plan.ComputeStages) != 1 {
				t.Errorf("base plan: group %+v, compute %v; want the stage in compute", plan.Group, plan.ComputeStages)
			}
			if plan.Typed == nil || strings.Join(plan.Typed.Requires, ",") != strings.Join(c.requires, ",") {
				t.Fatalf("typed %+v, want it to require %v", plan.Typed, c.requires)
			}
			exact := plan.Typed.Exact
			if exact == nil || exact.Group == nil || exact.Group.Render != c.render || len(exact.ComputeStages) != 0 {
				t.Errorf("exact plan %+v, want group %s and no compute stages", exact, c.render)
			}
		})
	}
}

// A count is exact for any key and needs no facts.
func TestKeyedAggregate_ACountNeedsNoFacts(t *testing.T) {
	plan := planKeyed(t, `{ c[$1]++ } END { for (k in c) print k, c[k] }`)
	if plan.Group == nil || plan.Group.Render != RenderKeyCount || plan.Typed != nil || len(plan.ComputeStages) != 0 {
		t.Errorf("count plan: group %+v typed %+v compute %v", plan.Group, plan.Typed, plan.ComputeStages)
	}
}

// The shard SQL for each, pinned: the value columns beside each line, and what
// the group reports of them. A sum is an exact integer (a BIGINT added in 128
// bits, returned as text), so there is no bound to check, and nothing is counted
// bad: the plan only goes to shards whose facts say the values fit.
func TestKeyedAggregate_ShardSQL(t *testing.T) {
	const from = ` FROM (SELECT unnest(string_split(COALESCE(list_extract(fields, 1), ''), chr(10))) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, `
	const where = ` FROM logs WHERE customer_id = CAST(? AS BIGINT) AND namespace = ? AND service = ? AND kind = ?) AS t GROUP BY raw ORDER BY raw`
	for name, want := range map[string]string{
		keyedSum: "SELECT raw, min(ts) AS ts, count(*) AS count, COALESCE(CAST(sum(agg_val) AS VARCHAR), '0') AS agg_sum" +
			from + "TRY_CAST(list_extract(fields, 2) AS BIGINT) AS agg_val" + where,
		keyedMax: "SELECT raw, min(ts) AS ts, count(*) AS count, max(agg_val) AS agg_best, " +
			"first(agg_txt ORDER BY agg_val DESC, t.ts, t.event_id NULLS FIRST, t.line_no) AS agg_best_txt, " +
			"CAST(epoch_ns(first(t.ts ORDER BY agg_val DESC, t.ts, t.event_id NULLS FIRST, t.line_no)) AS BIGINT) AS agg_best_ns" +
			from + "TRY_CAST(list_extract(fields, 2) AS DOUBLE) AS agg_val, list_extract(fields, 2) AS agg_txt" + where,
		keyedMin: "SELECT raw, min(ts) AS ts, count(*) AS count, min(agg_val) AS agg_best, " +
			"first(agg_txt ORDER BY agg_val ASC, t.ts, t.event_id NULLS FIRST, t.line_no) AS agg_best_txt, " +
			"CAST(epoch_ns(first(t.ts ORDER BY agg_val ASC, t.ts, t.event_id NULLS FIRST, t.line_no)) AS BIGINT) AS agg_best_ns" +
			from + "TRY_CAST(list_extract(fields, 2) AS DOUBLE) AS agg_val, list_extract(fields, 2) AS agg_txt" + where,
	} {
		if got := planKeyed(t, name).Typed.Exact.DirectShardSQL; got != want {
			t.Errorf("%s\n got: %s\nwant: %s", name, got, want)
		}
	}
}

// The key can be any expression a print can compute; the value is always one
// field. What is not exactly that stays opaque, and asks for no facts.
func TestKeyedAggregate_WhichProgramsAreRecognized(t *testing.T) {
	for _, c := range []struct {
		program string
		lowers  bool
	}{
		{`{ s[$1 "-" $3] += $2 } END { for (k in s) print k, s[k] }`, true},
		{`{ s[HOST] += $2 } END { for (k in s) print k, s[k] }`, true},
		{`{ if ($4 > m[$1]) { m[$1] = $4 } } END { for (k in m) print k, m[k] }`, true},
		{`{ s[$1] += $2 * 2 } END { for (k in s) print k, s[k] }`, false},
		{`{ s[$1] += $0 } END { for (k in s) print k, s[k] }`, false},
		{`{ s[$1] += $NF } END { for (k in s) print k, s[k] }`, false},
		{`{ s[$1] += $(1+1) } END { for (k in s) print k, s[k] }`, false},
		{`{ s[$1] -= $2 } END { for (k in s) print k, s[k] }`, false},
		{`$2 > 0 { s[$1] += $2 } END { for (k in s) print k, s[k] }`, false},
		{`{ s[$1] += $2 } END { for (k in s) print s[k], k }`, false},
		{`{ s[$1] += $2; n[$1]++ } END { for (k in s) print k, s[k] }`, false},
		{`{ if ($2 >= m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`, false},
		{`{ if (m[$1] < $2) m[$1] = $2 } END { for (k in m) print k, m[k] }`, false},
		{`{ if ($2 > m[$1]) m[$1] = $3 } END { for (k in m) print k, m[k] }`, false},
		{`{ if ($2 > m[$1]) m[$2] = $2 } END { for (k in m) print k, m[k] }`, false},
		{`{ if ($2 > m[$1]) m[$1] = $2; else x++ } END { for (k in m) print k, m[k] }`, false},
		{`{ if ($2 > m) m = $2 } END { print m }`, false},
	} {
		plan := planKeyed(t, c.program)
		lowers := plan.Typed != nil && plan.Typed.Exact != nil && plan.Typed.Exact.Group != nil
		if lowers != c.lowers {
			t.Errorf("%s: lowers=%v, want %v", c.program, lowers, c.lowers)
		}
		if !c.lowers && (plan.Group != nil || len(plan.ComputeStages) != 1) {
			t.Errorf("%s: should stay one compute stage, got group %+v compute %v", c.program, plan.Group, plan.ComputeStages)
		}
	}
}

// A field in a pseudo-key position: each $N carries its own facts.
func TestKeyedAggregate_RequiresTheFieldItReads(t *testing.T) {
	plan := planKeyed(t, `{ s[$1] += $7 } END { for (k in s) print k, s[k] }`)
	if got := strings.Join(plan.Typed.Requires, ","); got != "$7,$7#int" {
		t.Errorf("requires %s, want $7,$7#int", got)
	}
}

// Explain says why the stage waits, in terms of the data and not the facts.
func TestKeyedAggregate_ExplainSaysWhatItWaitsFor(t *testing.T) {
	for program, want := range map[string]string{
		keyedSum: "$2 is not known to hold only integers",
		keyedMax: "$2 is not known to hold only numbers",
	} {
		out := Explain("cat /log/ns/svc/main.log | awk '" + program + "'")
		if !strings.Contains(out, want) {
			t.Errorf("%s: explain lacks %q:\n%s", program, want, out)
		}
		if strings.Contains(out, "fact") || strings.Contains(out, "#int") {
			t.Errorf("explain mentions the facts:\n%s", out)
		}
	}
}

// A saved cron or dashboard carries the base plan only: it never waits on facts
// a later run might not find.
func TestKeyedAggregate_ASavedPlanKeepsTheStage(t *testing.T) {
	lp, err := Lower("cat /log/ns/svc/main.log | awk '"+keyedSum+"'", 1, "bolt", noResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if lp.Group != nil || len(lp.ComputeStages) != 1 {
		t.Errorf("lowered plan: group %+v compute %v, want the stage", lp.Group, lp.ComputeStages)
	}
}
