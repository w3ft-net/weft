package planner

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	coords "github.com/graybeardcorp/weft/shape"
)

// goldenPipelines are planned for customer 7 and their SQL, args and typed
// variants pinned in testdata/tenant_sql.golden. The hosted shape must keep
// producing exactly these strings: the tenant predicate is generated from the
// shape, and for the hosted shape that has to be today's SQL, byte for byte.
var goldenPipelines = []string{
	`cat /log/acme/api/main.log`,
	`tail -n 20 /log/acme/api/main.log`,
	`head -n 5 /log/acme/prod/api/main.log`,
	`cat /log/acme/api/main.log /log/acme/web/main.log`,
	`cat /log/acme/api/main.log | grep ERROR`,
	`cat /log/acme/api/main.log | grep -v health | grep -i timeout`,
	`cat /log/acme/api/main.log | awk '$5 > 500'`,
	`cat /log/acme/prod/api/main.log | awk '$2 >= 3'`,
	`cat /log/weft/shard/stats.log | jq 'select(.q_max_ms > 10)'`,
	`cat /log/acme/api/main.log | jq -r '.level'`,
	`cat /log/acme/api/main.log | wc -l`,
	`cat /log/acme/api/main.log | sort | uniq -c`,
	`cat /log/ns/svc/main.log | awk '{ s[$1] += $2 } END { for (k in s) print k, s[k] }'`,
	`cat /log/ns/svc/main.log | awk '{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }'`,
	`tail -f /log/acme/api/main.log`,
	`tail -f /log/acme/api/main.log | grep ERROR`,
	`cat /log/acme/api/main.log /log/acme/api/audit.log`,
	`cat /log/acme/api/main.log | head -5`,
	`trace`,
	`trace acme/api`,
	`trace --id abc-123`,
	`trace --spans acme/api`,
	`cat /log/acme/api/main.log | awk '{ print $1 }'`,
}

func goldenText(t *testing.T) string {
	t.Helper()
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	end := time.Date(2026, 1, 2, 5, 4, 5, 0, time.UTC)
	var b strings.Builder
	for i, pipeline := range goldenPipelines {
		s, e := start, end
		if i%5 == 4 { // some with an open window
			s, e = time.Time{}, time.Time{}
		}
		fmt.Fprintf(&b, "== %s\n", pipeline)
		plan, err := goldenPlan(pipeline, s, e)
		if err != nil {
			fmt.Fprintf(&b, "error: %v\n\n", err)
			continue
		}
		goldenPlanText(&b, plan)
		b.WriteString("\n")
	}
	return b.String()
}

// kindName names an argument's kind without its number, so a renamed or
// renumbered tenant kind does not move the golden file.
func goldenKind(k ArgKind) string {
	switch k {
	case ArgLiteral:
		return "literal"
	case ArgTimeStart:
		return "start"
	case ArgTimeEnd:
		return "end"
	}
	return "tenant"
}

func goldenArgs(b *strings.Builder, label string, args []any, sources []ArgSource) {
	fmt.Fprintf(b, "%s args=%q\n", label, fmt.Sprintf("%v", args))
	var kinds []string
	for _, s := range sources {
		kinds = append(kinds, goldenKind(s.Kind)+":"+s.Value)
	}
	fmt.Fprintf(b, "%s sources=%s\n", label, strings.Join(kinds, " "))
}

func goldenPlanText(b *strings.Builder, p *QueryPlan) {
	fmt.Fprintf(b, "shard=%q\n", p.ShardSQL)
	fmt.Fprintf(b, "direct=%q\n", p.DirectShardSQL)
	fmt.Fprintf(b, "field=%q\n", p.FieldSQL)
	goldenArgs(b, "plan", p.Args, p.ArgSources)
	fmt.Fprintf(b, "table=%q cmd=%q ns=%q svc=%q kind=%q compute=%q agg=%q\n",
		p.Table, p.SourceCmd, p.SourceNamespace, p.SourceService, p.SourceKind, p.ComputeStages, p.Aggregate)
	if p.Typed != nil {
		fmt.Fprintf(b, "typed.requires=%q table=%q env=%q\n", p.Typed.Requires, p.Typed.Table, p.Typed.Env)
		fmt.Fprintf(b, "typed.shard=%q\n", p.Typed.ShardSQL)
		fmt.Fprintf(b, "typed.direct=%q\n", p.Typed.DirectShardSQL)
		fmt.Fprintf(b, "typed.field=%q\n", p.Typed.FieldSQL)
		if p.Typed.Exact != nil {
			fmt.Fprintf(b, "typed.exact.shard=%q\n", p.Typed.Exact.ShardSQL)
		}
	}
	if p.TraceStats != nil {
		fmt.Fprintf(b, "tracestats.sql=%q\n", p.TraceStats.SQL)
		fmt.Fprintf(b, "tracestats.args=%q\n", fmt.Sprintf("%v", p.TraceStats.Args))
	}
}

func TestTenantSQL_Golden(t *testing.T) {
	if coords.BuildLayout().Fingerprint != coords.Legacy().Fingerprint {
		t.Skip("the golden file is the hosted shape's SQL")
	}
	const path = "testdata/tenant_sql.golden"
	got := goldenText(t)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("the SQL planned for customer 7 changed; rerun with -update only if that is intended.\n--- got\n%s\n--- want\n%s", got, want)
	}
}
