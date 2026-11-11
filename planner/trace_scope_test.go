package planner

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	coords "github.com/graybeardcorp/weft/shape"
)

// A trace spans services, so a user who can read only some namespaces still
// runs `trace`, but sees only the lines from namespaces they can read. The
// restriction is in the SQL, not applied to the output afterwards: counts,
// durations and the per-trace service list are computed over rows the user may
// read, so nothing about the others leaks through an aggregate.

// namespaceScope is a scope that reads exactly the named namespaces of customer 1.
func namespaceScope(readable []string) coords.Set {
	set := coords.Set{}
	for _, n := range readable {
		set = append(set, coords.Coord{Customer: 1, Namespace: coords.Value(n)})
	}
	return set
}

func scopedPlanErr(pipeline string, readable []string) (*QueryPlan, error) {
	end := time.Now()
	return PlanIn(pipeline, 1, namespaceScope(readable), "/", end.Add(-time.Hour), end)
}

func scopedPlan(t *testing.T, pipeline string, readable []string) *QueryPlan {
	t.Helper()
	p, err := scopedPlanErr(pipeline, readable)
	if err != nil {
		t.Fatalf("PlanIn(%q): %v", pipeline, err)
	}
	return p
}

func hasArg(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

func TestPlanScoped_TraceIsRestrictedToReadableNamespaces(t *testing.T) {
	for _, pipeline := range []string{
		"trace",                     // recent traces
		"trace --id abc123",         // one request's lines
		"trace --id abc123 --spans", // one request's span breakdown
		"trace --spans",             // operations across requests
		"trace --spans --name checkout --samples", // one operation's slowest occurrences
	} {
		t.Run(pipeline, func(t *testing.T) {
			p := scopedPlan(t, pipeline, []string{"alpha", "gamma"})

			if !strings.Contains(p.ShardSQL, "namespace IN (?, ?)") || !hasArg(p.Args, "alpha") || !hasArg(p.Args, "gamma") {
				t.Errorf("shard SQL is not restricted to the readable namespaces:\n%s\nargs=%v", p.ShardSQL, p.Args)
			}
			if got, _, _ := p.Coords.Namespaces(1); !slices.Equal(got, []string{"alpha", "gamma"}) {
				t.Errorf("Coords namespaces = %v, want [alpha gamma]", got)
			}
			// Routing stays on every shard: SourceNamespace routes by the
			// first namespace only, which would skip the others' shards.
			if p.SourceNamespace != "" {
				t.Errorf("SourceNamespace = %q, want it empty so every shard is searched", p.SourceNamespace)
			}
		})
	}
}

// The trace list asks a second question about the traces it found, and that
// answer — services touched, line and error counts — must be restricted too.
func TestPlanScoped_TraceStatsAreRestrictedToo(t *testing.T) {
	p := scopedPlan(t, "trace", []string{"alpha"})

	if p.TraceStats == nil {
		t.Fatal("trace list has no stats query")
	}
	if !strings.Contains(p.TraceStats.SQL, "namespace = ?") || !hasArg(p.TraceStats.Args, "alpha") {
		t.Errorf("stats SQL is not restricted:\n%s\nargs=%v", p.TraceStats.SQL, p.TraceStats.Args)
	}
}

func TestPlan_UnscopedTraceReadsEveryNamespace(t *testing.T) {
	end := time.Now()
	p, err := Plan("trace", 1, "/", end.Add(-time.Hour), end)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.ShardSQL, "namespace IN") || strings.Contains(p.TraceStats.SQL, "namespace IN") {
		t.Errorf("an unscoped trace is restricted to namespaces:\n%s\n%s", p.ShardSQL, p.TraceStats.SQL)
	}
}

// A source the scope does not cover is refused by the planner itself, whether
// or not it exists, and the refusal never says which.
func TestPlanIn_SourceOutsideTheScopeIsRefused(t *testing.T) {
	for _, pipeline := range []string{"cat /log/beta/api/main.log", "trace beta"} {
		if _, err := scopedPlanErr(pipeline, []string{"alpha"}); !errors.Is(err, ErrNoSource) {
			t.Errorf("%q: err = %v, want ErrNoSource", pipeline, err)
		}
	}
}

// A wildcard source means every tuple in scope, never "no predicate".
func TestPlanIn_WildcardResolvesAgainstTheScope(t *testing.T) {
	p := scopedPlan(t, "cat /log/warp.log", []string{"alpha", "gamma"})
	if !strings.Contains(p.ShardSQL, "namespace IN (?, ?)") || !hasArg(p.Args, "alpha") || !hasArg(p.Args, "gamma") {
		t.Errorf("warp.log was not held to the scope:\n%s\nargs=%v", p.ShardSQL, p.Args)
	}
}

// A source that names one namespace the scope reads and one it does not is
// refused whole. Running the readable half would return a partial answer.
func TestPlanIn_MixedSourceIsRefusedWhole(t *testing.T) {
	_, err := scopedPlanErr("cat /log/alpha/api/main.log /log/beta/api/main.log", []string{"alpha"})
	if !errors.Is(err, ErrNoSource) || !strings.Contains(err.Error(), "beta") {
		t.Errorf("err = %v, want ErrNoSource naming beta", err)
	}
}

// An empty scope means the user may read nothing. A trace for them selects no
// rows, and an ordinary query is refused.
func TestPlanIn_NothingReadableSelectsNothing(t *testing.T) {
	p := scopedPlan(t, "trace", []string{})
	if p.ShardSQL != "" {
		t.Errorf("a user who can read nothing got a trace query:\n%s", p.ShardSQL)
	}
	if _, err := scopedPlanErr("cat /log/beta/api/main.log", []string{}); !errors.Is(err, ErrNoSource) {
		t.Errorf("err = %v, want ErrNoSource", err)
	}
}

func TestPlanIn_ScopeOfAnotherCustomerIsRefused(t *testing.T) {
	if _, err := PlanIn("cat /log/a/api/main.log", 1, coords.Set{{Customer: 2}}, "/", time.Time{}, time.Time{}); err == nil {
		t.Error("a scope reaching customer 2 planned for customer 1")
	}
}
