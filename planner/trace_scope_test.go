package planner

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A trace spans services, so a user who can read only some namespaces still
// runs `trace`, but sees only the lines from namespaces they can read. The
// restriction is in the SQL, not applied to the output afterwards: counts,
// durations and the per-trace service list are computed over rows the user may
// read, so nothing about the others leaks through an aggregate.

func scopedPlan(t *testing.T, pipeline string, readable []string) *QueryPlan {
	t.Helper()
	end := time.Now()
	p, err := PlanScoped(pipeline, 1, "/", end.Add(-time.Hour), end, readable)
	if err != nil {
		t.Fatalf("PlanScoped(%q): %v", pipeline, err)
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
			if !slices.Equal(p.SourceNamespaces, []string{"alpha", "gamma"}) {
				t.Errorf("SourceNamespaces = %v, want [alpha gamma]", p.SourceNamespaces)
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

// Scoping is for trace only; every other command keeps its explicit namespaces
// and is refused by the coordinator if the user can't read them.
func TestPlanScoped_OtherCommandsAreUnchanged(t *testing.T) {
	p := scopedPlan(t, "cat /log/beta/api/main.log", []string{"alpha"})

	if !slices.Equal(p.SourceNamespaces, []string{"beta"}) {
		t.Errorf("SourceNamespaces = %v, want [beta] so the coordinator can refuse it", p.SourceNamespaces)
	}
}

// A trace that names its own namespace keeps it; the coordinator refuses it if
// the user can't read that one.
func TestPlanScoped_TraceNamingANamespaceKeepsIt(t *testing.T) {
	p := scopedPlan(t, "trace beta", []string{"alpha"})

	if !slices.Equal(p.SourceNamespaces, []string{"beta"}) {
		t.Errorf("SourceNamespaces = %v, want [beta]", p.SourceNamespaces)
	}
}

// An empty readable list means the user may read nothing, not everything (an
// empty namespace list in a plan means "all"). A trace for them selects no
// rows, and an ordinary query is unchanged.
func TestPlanScoped_NothingReadableSelectsNothing(t *testing.T) {
	p := scopedPlan(t, "trace", []string{})
	if p.ShardSQL != "" {
		t.Errorf("a user who can read nothing got a trace query:\n%s", p.ShardSQL)
	}

	q := scopedPlan(t, "cat /log/beta/api/main.log", []string{})
	if q.ShardSQL == "" || !slices.Equal(q.SourceNamespaces, []string{"beta"}) {
		t.Errorf("an ordinary query was changed: sql=%q namespaces=%v", q.ShardSQL, q.SourceNamespaces)
	}
}
