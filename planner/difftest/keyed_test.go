package difftest

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape/shapetest"
)

// keyedRows are lines `key value tail`, one second apart so every row has its
// own ts and the merge order (ts, then shard) is the order awk meets them. Each
// key draws from its own pool of values, since the aggregates differ most at
// the edges: a key whose values are all above zero, all below, all zero, a mix
// of everything, and one seen only once.
func keyedRows(pools map[string][]string) []*pb.LogRow {
	var rows []*pb.LogRow
	for i := 0; i < 200; i++ {
		key := keyedNames[i%len(keyedNames)]
		pool := pools[key]
		if key == "sparse" && i >= len(keyedNames) {
			key = "pos" // "sparse" appears once
			pool = pools[key]
		}
		rows = append(rows, &pb.LogRow{
			Raw:       fmt.Sprintf("%s %s tail%d", key, pool[(i*7+i/len(keyedNames))%len(pool)], i%3),
			TsUnixNs:  time.Date(2026, 10, 31, 12, 0, i, 0, time.UTC).UnixNano(),
			Namespace: "keyed", Service: "api", Kind: "main", CustomerId: 1,
		})
	}
	return rows
}

// pools splits values into the keys' pools: by sign, so a key can hold only
// values the aggregates treat alike.
func pools(values []string) map[string][]string {
	p := map[string][]string{"mix": values, "sparse": values}
	for _, v := range values {
		switch {
		case strings.HasPrefix(v, "-") && strings.Trim(v, "-.0eE") != "":
			p["neg"] = append(p["neg"], v)
		case strings.Trim(v, "+-.0eE") == "":
			p["zero"] = append(p["zero"], v)
		default:
			p["pos"] = append(p["pos"], v)
		}
	}
	return p
}

var keyedNames = []string{"pos", "neg", "zero", "mix", "sparse"}

// extremeValues are finite decimals in several spellings of the same number
// (100, 100.0, 1e2, +100): awk assigns the field, so the text a max prints is
// the first row's spelling, which only a faithful tie-break reproduces.
var extremeValues = []string{
	"3", "3.0", "+3", "003", "3e0", "100", "100.0", "1e2", "+100", "1E2", "7", "-3", "-3.0", "-100", "0", "-0", "0.0",
	".5", "5.", "-.5", "1e3", "250", "9007199254740993", "1e308", "-1e308", "4.9e-324", "1e-999", "42", "42.5",
}

// sumValues are integers, in the spellings the integer fact allows.
var sumValues = []string{"0", "-0", "7", "-3", "12", "007", "+5", "1000000", "-1000000", "42", "-42", "999", "13", "-8"}

// keyedPlan plans a pipeline that should lower on shards with the facts, and
// returns it with the exact plan.
func keyedPlan(t *testing.T, program string, facts ...string) (*planner.QueryPlan, *planner.QueryPlan) {
	t.Helper()
	pipeline := fmt.Sprintf("cat /log/keyed/api/main.log | awk '%s'", program)
	plan, err := planner.Plan(pipeline, shapetest.Tenant(1), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	if len(plan.ComputeStages) != 1 || plan.Group != nil {
		t.Fatalf("%s: the base plan must keep the stage (compute %v, group %+v)", pipeline, plan.ComputeStages, plan.Group)
	}
	if plan.Typed == nil || fmt.Sprint(plan.Typed.Requires) != fmt.Sprint(facts) {
		t.Fatalf("%s: typed requires %+v, want %v", pipeline, plan.Typed, facts)
	}
	exact := plan.Typed.Exact
	if exact == nil || len(exact.ComputeStages) != 0 || exact.Group == nil {
		t.Fatalf("%s: the exact plan must be the shard's GROUP BY alone: %+v", pipeline, exact)
	}
	return plan, exact
}

// checkKeyed runs the base and exact plans against the reference awk.
func checkKeyed(t *testing.T, h *harness, program string, facts ...string) {
	t.Helper()
	plan, exact := keyedPlan(t, program, facts...)
	source, stages := splitSource(t, fmt.Sprintf("cat /log/keyed/api/main.log | awk '%s'", program))
	want, wantErr := h.runReference(source, stages, false)
	if wantErr != nil {
		t.Fatalf("%s: reference: %v", program, wantErr)
	}
	for name, run := range map[string]func() (string, error){
		"base":  func() (string, error) { return h.runPlan(plan) },
		"exact": func() (string, error) { return h.runPlan(exact) },
	} {
		got, err := run()
		if err != nil {
			t.Errorf("%s: %s plan: %v", program, name, err)
			continue
		}
		if got != want {
			t.Errorf("%s: %s plan differs from the reference: %s", program, name, firstDiff(got, want))
		}
	}
}

func TestDifferentialKeyedSum(t *testing.T) {
	h := newHarness(t, keyedRows(pools(sumValues)), 2)
	for _, program := range []string{
		`{ s[$1] += $2 } END { for (k in s) print k, s[k] }`,
		`{ total[$3] += $2 } END { for (g in total) print g, total[g] }`,
		`{ s[$1 "-" $3] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$99] += $2 } END { for (k in s) print k, s[k] }`,
	} {
		t.Run(program, func(t *testing.T) { checkKeyed(t, h, program, "$2", "$2#int") })
	}
}

func TestDifferentialKeyedMax(t *testing.T) {
	h := newHarness(t, keyedRows(pools(extremeValues)), 2)
	for _, program := range []string{
		`{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`,
		`{ if ($2 > best[$3]) { best[$3] = $2 } } END { for (g in best) print g, best[g] }`,
		`{ if ($2 > m[$1 "-" $3]) m[$1 "-" $3] = $2 } END { for (k in m) print k, m[k] }`,
	} {
		t.Run(program, func(t *testing.T) { checkKeyed(t, h, program, "$2") })
	}
}

func TestDifferentialKeyedMin(t *testing.T) {
	h := newHarness(t, keyedRows(pools(extremeValues)), 2)
	for _, program := range []string{
		`{ if ($2 < m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`,
		`{ if ($2 < low[$3]) { low[$3] = $2 } } END { for (g in low) print g, low[g] }`,
	} {
		t.Run(program, func(t *testing.T) { checkKeyed(t, h, program, "$2") })
	}
}

// The same rows on one shard, and three: the merge must not depend on how the
// rows were split.
func TestDifferentialKeyedAcrossShardCounts(t *testing.T) {
	for _, shards := range []int{1, 3, 5} {
		t.Run(fmt.Sprintf("%d shards", shards), func(t *testing.T) {
			sums := newHarness(t, keyedRows(pools(sumValues)), shards)
			checkKeyed(t, sums, `{ s[$1] += $2 } END { for (k in s) print k, s[k] }`, "$2", "$2#int")
			ext := newHarness(t, keyedRows(pools(extremeValues)), shards)
			checkKeyed(t, ext, `{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`, "$2")
			checkKeyed(t, ext, `{ if ($2 < m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`, "$2")
		})
	}
}

// lines builds rows from raw lines, one second apart.
func lines(raws ...string) []*pb.LogRow {
	var rows []*pb.LogRow
	for i, raw := range raws {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: time.Date(2026, 10, 31, 12, 0, i, 0, time.UTC).UnixNano(),
			Namespace: "keyed", Service: "api", Kind: "main", CustomerId: 1,
		})
	}
	return rows
}

// A sum is exact however large, which awk's float sum is not: the shards add in 128
// bits and compute in arbitrary precision. Below 2^53 the two print the same
// (the tests above); past it, awk rounds, so the expected values here are
// computed independently, not by awk.
func TestDifferentialKeyedSumIsExactPastFloatRange(t *testing.T) {
	const big = "999999999999999" // the most the integer fact allows a value: 15 digits
	var raws []string
	want := map[string]*bigInt{}
	for i := 0; i < 40; i++ {
		key := []string{"a", "b", "c"}[i%3]
		v := big
		if i%5 == 0 {
			v = "-" + big
		}
		raws = append(raws, key+" "+v)
		if want[key] == nil {
			want[key] = newBigInt()
		}
		want[key].add(v)
	}
	plan, err := planner.Plan("cat /log/keyed/api/main.log | awk '"+sumProg+"'", shapetest.Tenant(1), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, shards := range []int{1, 2, 3} {
		h := newHarness(t, lines(raws...), shards)
		got, err := h.runPlan(plan.Typed.Exact)
		if err != nil {
			t.Fatal(err)
		}
		var wantLines []string
		for _, k := range []string{"a", "b", "c"} {
			wantLines = append(wantLines, k+" "+want[k].String())
		}
		if g := strings.TrimSuffix(got, "\n"); g != strings.Join(wantLines, "\n") {
			t.Errorf("%d shards: %q, want %q", shards, g, strings.Join(wantLines, "\n"))
		}
	}
}

// Shapes that look like the aggregates but are not stay in compute with no
// facts to wait for.
func TestKeyedAggregateShapesThatDoNotLower(t *testing.T) {
	for _, program := range []string{
		`{ s[$1] += $2 * 2 } END { for (k in s) print k, s[k] }`,
		`{ s[$1] += $2 + 1 } END { for (k in s) print k, s[k] }`,
		`{ s[$1] -= $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$1] += $NF } END { for (k in s) print k, s[k] }`,
		`{ s[$1] += $(1+1) } END { for (k in s) print k, s[k] }`,
		`{ s[$1] += $0 } END { for (k in s) print k, s[k] }`,
		`$2 > 1 { s[$1] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$1] += $2 } END { for (k in s) print s[k], k }`,
		`{ s[$1] += $2 } END { for (k in s) print k, s[k] * 2 }`,
		`{ if ($2 >= m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`,
		`{ if (m[$1] < $2) m[$1] = $2 } END { for (k in m) print k, m[k] }`,
		`{ if ($2 > m[$1]) m[$1] = $3 } END { for (k in m) print k, m[k] }`,
		`{ if ($2 > m[$1]) m[$2] = $2 } END { for (k in m) print k, m[k] }`,
		`{ if ($2 > m[$1]) m[$1] = $2; else n[$1]++ } END { for (k in m) print k, m[k] }`,
		`{ if ($2 > m) m = $2 } END { print m }`,
	} {
		plan, err := planner.Plan("cat /log/keyed/api/main.log | awk '"+program+"'", shapetest.Tenant(1), "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Group != nil || plan.Typed != nil && plan.Typed.Exact != nil && plan.Typed.Exact.Group != nil {
			t.Errorf("%s lowered to a group", program)
		}
		if len(plan.ComputeStages) != 1 || !strings.Contains(plan.ComputeStages[0], "awk") {
			t.Errorf("%s: compute %v", program, plan.ComputeStages)
		}
	}
}

// Equal numbers in different spellings: awk assigns the field only for a strictly
// greater one, so the key keeps the first spelling it met, in ts order. Rows go
// round-robin over the shards, so the winner is sometimes on the shard that
// answers second, and the merge has to know which row was first.
func TestDifferentialKeyedTiesKeepTheFirstSpelling(t *testing.T) {
	for _, shards := range []int{2, 3} {
		for name, raws := range map[string][]string{
			"first of two":             {"t 5.0", "t 5"},
			"second shard first":       {"t 1", "t 5.0", "t 5", "t 5e0"},
			"third shard first":        {"t 1", "t 2", "t 5e0", "t 5.0", "t +5", "t 5"},
			"later and greater":        {"t 5", "t 5.0", "t 6.0", "t 6", "t 6e0"},
			"negative ties":            {"t -1", "t -5.0", "t -5", "t -5e0"},
			"zeros never assign":       {"t 0", "t 0.0", "t -0"},
			"keys tie independently":   {"a 3", "b 3.0", "a 3.0", "b 3", "a 3e0", "b 3e0"},
			"a tie then something big": {"t 9.0", "t 9", "t 9e0", "t 10", "t 10.0"},
		} {
			t.Run(fmt.Sprintf("%d shards/%s", shards, name), func(t *testing.T) {
				h := newHarness(t, lines(raws...), shards)
				checkKeyed(t, h, `{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`, "$2")
				checkKeyed(t, h, `{ if ($2 < m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`, "$2")
			})
		}
	}
}

// sortRows have keys that tie on their sums and maxima, a key with a blank in
// it, and spellings sort -n reads differently from awk (1e3 is 1 to sort).
func sortRows() []*pb.LogRow {
	return lines(
		"a 5", "b 5", "c 7", "d 5", "e -2", "f -2", "g 0", "h 12", "i 12", "j 1000", "k 3",
		"a 1", "b 1", "c -7", "d 4", "e 2", "f 2", "g 0", "h 0", "i 0", "j 5", "k 3",
	)
}

func sortRowsFloat() []*pb.LogRow {
	return lines(
		"a 5", "b 5.0", "c 7", "d 5e0", "e 1e3", "f 1000", "g 1e2", "h 100", "i .5", "j 0.5", "k 5.", "l +5",
		"a 1", "b 2", "c 3", "d 4", "e 1e3", "f 999", "g 100.0", "h 99", "i 1", "j 1e0", "k 5", "l 6",
	)
}

// checkKeyedPipeline runs `awk program | tail` against the reference. It
// returns the base and exact plans, and whether the stage after the aggregate
// is gone from the exact plan (folded into the group).
func checkKeyedPipeline(t *testing.T, h *harness, program, tail string) (base, exact *planner.QueryPlan) {
	t.Helper()
	pipeline := "cat /log/keyed/api/main.log | awk '" + program + "'"
	if tail != "" {
		pipeline += " | " + tail
	}
	base, err := planner.Plan(pipeline, shapetest.Tenant(1), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	exact = base
	if base.Typed != nil {
		exact = base.Typed.Exact
	}
	source, stages := splitSource(t, pipeline)
	want, wantErr := h.runReference(source, stages, false)
	for name, plan := range map[string]*planner.QueryPlan{"base": base, "exact": exact} {
		got, err := h.runPlan(plan)
		if (err != nil) != (wantErr != nil) {
			t.Errorf("%s: %s plan error %v, reference error %v", pipeline, name, err, wantErr)
			continue
		}
		if got != want {
			t.Errorf("%s: %s plan differs from the reference: %s", pipeline, name, firstDiff(got, want))
		}
	}
	return base, exact
}

const (
	sumProg   = `{ s[$1] += $2 } END { for (k in s) print k, s[k] }`
	countProg = `{ c[$2]++ } END { for (k in c) print k, c[k] }`
	maxProg   = `{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`
	minProg   = `{ if ($2 < m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`
)

// A sort by the value, and a head after it, are folded into the group and
// applied to the aggregate's lines in compute, as the stages would have.
func TestDifferentialKeyedSortAfter(t *testing.T) {
	sorts := []string{
		"sort -k2 -n", "sort -k2 -rn", "sort -k2,2 -n", "sort -k2,2nr", "sort -k2n", "sort -k2nr", "sort -k 2 -n",
		"sort -n -k2", "sort -r -n -k2", "sort -rn -k2", "sort -k2 -nr", "sort -k2,2n -r", "sort -k2,2nr -r", "sort -k2,2n",
		"sort -k2,2rn",
	}
	heads := []string{"", " | head -n 1", " | head -n 3", " | head -2"}
	for _, c := range []struct {
		name, prog string
		rows       func() []*pb.LogRow
	}{
		{"sum", sumProg, sortRows}, {"count", countProg, sortRows}, {"max", maxProg, sortRowsFloat}, {"min", minProg, sortRowsFloat},
	} {
		for shards := 1; shards <= 3; shards += 2 {
			h := newHarness(t, c.rows(), shards)
			for _, sortCmd := range sorts {
				for _, head := range heads {
					t.Run(fmt.Sprintf("%s/%d shards/%s%s", c.name, shards, sortCmd, head), func(t *testing.T) {
						base, exact := checkKeyedPipeline(t, h, c.prog, sortCmd+head)
						lowered := exact
						if c.name == "count" {
							lowered = base
						}
						if lowered.Group == nil || lowered.Group.Order == nil || len(lowered.ComputeStages) != 0 {
							t.Errorf("the sort was not folded into the group: group %+v compute %v", lowered.Group, lowered.ComputeStages)
						}
					})
				}
			}
		}
	}
}

// Sorts and heads that do not fold: they run in compute after the group, and
// the answer is the same.
func TestDifferentialKeyedSortAfterThatDoesNotFold(t *testing.T) {
	h := newHarness(t, sortRows(), 2)
	for _, tail := range []string{
		"sort -k2", "sort -n", "sort -rn", "sort -k1 -n", "sort -k2 -n -u", "sort -u -k2,2n", "sort -k2,3n", "sort -k2n -k1",
		"sort -k2 -n | tail -n 1", "sort -k2 -rn | head -n 0", "sort -k2 -n | head -n 2 | tail -n 1", "sort -t ' ' -k2 -n", "sort -k2.1 -n",
		"sort -k2 -n -", "sort -k2 -n -z",
	} {
		t.Run(tail, func(t *testing.T) {
			_, exact := checkKeyedPipeline(t, h, sumProg, tail)
			if exact.Group != nil && exact.Group.Order != nil && len(exact.ComputeStages) == 0 && tail != "sort -k2 -n | head -n 2 | tail -n 1" {
				t.Errorf("%q folded into the group", tail)
			}
		})
	}
}

// Keys that are not one word: with a blank in it the key takes fields of its own,
// and an empty one leaves the value as the first field, so the sort's key is
// something else. The order is computed on the printed lines, so it follows.
func TestDifferentialKeyedSortAfterOddKeys(t *testing.T) {
	// Some rows have a third field, so keying on it leaves an empty key beside
	// others: the line for it is ` value`, whose first field is the value.
	rows := lines(
		"a 5 x", "b 5", "c 7 y", "d 5", "e -2 x", "f -2 z", "g 0", "h 12 y", "i 12", "j 1000 z", "k 3",
		"a 1 x", "b 1", "c -7 y", "d 4 z", "e 2", "f 2 z", "g 0", "h 0 x", "i 0", "j 5", "k 3 q",
	)
	h := newHarness(t, rows, 2)
	for _, prog := range []string{
		`{ s[$3] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$3 $1] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$1 " x"] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$1 " " $1] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$99] += $2 } END { for (k in s) print k, s[k] }`,
		`{ s[$1 $99] += $2 } END { for (k in s) print k, s[k] }`,
	} {
		for _, tail := range []string{"sort -k2 -n", "sort -k2,2nr", "sort -k2 -rn | head -n 2", "sort -r -n -k2"} {
			t.Run(prog+" | "+tail, func(t *testing.T) { checkKeyedPipeline(t, h, prog, tail) })
		}
	}
}

// A sort ahead of a count or a sum does not change it, so it is dropped and the
// aggregate lowers; ahead of a max or min the first row of a tie decides, so it
// stays.
func TestDifferentialKeyedSortBefore(t *testing.T) {
	h := newHarness(t, sortRows(), 2)
	hf := newHarness(t, sortRowsFloat(), 2)
	for _, sortCmd := range []string{"sort", "sort -r", "sort -n", "sort -rn", "sort -k2 -n", "sort -k2,2nr", "sort -k 1", "sort -k1,1 -r", "sort -nr -k2"} {
		for _, c := range []struct {
			name, prog string
			dropped    bool
		}{{"sum", sumProg, true}, {"count", countProg, true}, {"max", maxProg, false}, {"min", minProg, false}} {
			t.Run(c.name+"/"+sortCmd, func(t *testing.T) {
				rows := h
				if !c.dropped {
					rows = hf
				}
				pipeline := "cat /log/keyed/api/main.log | " + sortCmd + " | awk '" + c.prog + "'"
				base, err := planner.Plan(pipeline, shapetest.Tenant(1), "", time.Time{}, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				exact := base
				if base.Typed != nil {
					exact = base.Typed.Exact
				}
				source, stages := splitSource(t, pipeline)
				want, wantErr := rows.runReference(source, stages, false)
				for name, plan := range map[string]*planner.QueryPlan{"base": base, "exact": exact} {
					if got, err := rows.runPlan(plan); err != nil || wantErr != nil || got != want {
						t.Errorf("%s plan = %q, %v; reference %q, %v", name, got, err, want, wantErr)
					}
				}
				lowered := exact
				if c.name == "count" {
					lowered = base
				}
				if got := lowered.Group != nil && len(lowered.ComputeStages) == 0; got != c.dropped {
					t.Errorf("lowered=%v, want %v (compute %v)", got, c.dropped, lowered.ComputeStages)
				}
			})
		}
	}
}

// Sorts that can change the answer or are not plain sorts stay, whatever follows.
func TestKeyedSortBeforeThatStays(t *testing.T) {
	for _, sortCmd := range []string{"sort -u", "sort -k2 -u", "sort -t ' '", "sort -z", "sort somefile", "sort -k", "sort -k 0", "sort -k2,1"} {
		pipeline := "cat /log/keyed/api/main.log | " + sortCmd + " | awk '" + sumProg + "'"
		plan, err := planner.Plan(pipeline, shapetest.Tenant(1), "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		exact := plan
		if plan.Typed != nil {
			exact = plan.Typed.Exact
		}
		// `sort -u` alone becomes a distinct-lines group of its own (the
		// existing lowering); what must not happen is the awk joining a group,
		// or the sort being dropped.
		var render planner.GroupRender
		if exact.Group != nil {
			render = exact.Group.Render
		}
		stages := strings.Join(exact.ComputeStages, " | ")
		sortStays := exact.Group != nil && render == planner.RenderLine || strings.Contains(stages, "sort")
		if !strings.Contains(stages, "awk") || !sortStays || render == planner.RenderKeySum {
			t.Errorf("%s: group %+v compute %v; the sort and the awk must both stay", sortCmd, exact.Group, exact.ComputeStages)
		}
	}
}

// bigInt is a running total of decimal integers, for expected values that no
// float can hold.
type bigInt struct{ n *big.Int }

func newBigInt() *bigInt { return &bigInt{n: new(big.Int)} }

func (b *bigInt) add(v string) {
	x, _ := new(big.Int).SetString(v, 10)
	b.n.Add(b.n, x)
}

func (b *bigInt) String() string { return b.n.String() }
