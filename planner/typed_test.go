package planner

import (
	"strings"
	"testing"
	"time"
)

func planTyped(t *testing.T, pipeline string) *QueryPlan {
	t.Helper()
	p, err := Plan(pipeline, 7, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	return p
}

func TestTypedPlan_AwkFieldComparison(t *testing.T) {
	p := planTyped(t, `cat /log/acme/api/main.log | awk '$5 > 500' | wc -l`)
	if p.Typed == nil {
		t.Fatal("no typed variant for a numeric comparison on $5")
	}
	if got := strings.Join(p.Typed.Requires, ","); got != "$5" {
		t.Errorf("requires %q, want $5", got)
	}
	tp := p.Typed
	if tp.CustomerID != 7 || tp.Namespace != "acme" || tp.Service != "api" || tp.Kind != "main" || tp.Env != "" {
		t.Errorf("source = %+v", tp)
	}
	// The base keeps the string alternative that makes it a wide prefilter;
	// the typed form drops it and keeps the numeric test and the escape.
	if !strings.Contains(p.ShardSQL, "list_extract(fields, 5) > '500'") {
		t.Errorf("base SQL lost its prefilter: %s", p.ShardSQL)
	}
	if strings.Contains(tp.ShardSQL, "> '500'") || strings.Contains(tp.DirectShardSQL, "> '500'") {
		t.Errorf("typed SQL still compares as a string: %s", tp.ShardSQL)
	}
	if !strings.Contains(tp.ShardSQL, "TRY_CAST(list_extract(fields, 5) AS DOUBLE) > 500") {
		t.Errorf("typed SQL has no numeric test: %s", tp.ShardSQL)
	}
	if strings.Contains(tp.ShardSQL, "IS NULL") {
		t.Errorf("typed SQL kept the missing-field case: %s", tp.ShardSQL)
	}
}

func TestTypedPlan_EnvComesFromThePath(t *testing.T) {
	p := planTyped(t, `cat /log/acme/prod/api/main.log | awk '$2 >= 3'`)
	if p.Typed == nil || p.Typed.Env != "prod" || p.Typed.Namespace != "acme" || p.Typed.Service != "api" {
		t.Fatalf("typed = %+v", p.Typed)
	}
}

func TestTypedPlan_AndOfComparisonsNeedsBothFields(t *testing.T) {
	p := planTyped(t, `cat /log/acme/api/main.log | awk '$3 > 1 && $5 < 100'`)
	if p.Typed == nil {
		t.Fatal("no typed variant")
	}
	if got := strings.Join(p.Typed.Requires, ","); got != "$3,$5" {
		t.Errorf("requires %q, want $3,$5", got)
	}
}

func TestTypedPlan_JQKeyComparison(t *testing.T) {
	p := planTyped(t, `cat /log/weft/shard/stats.log | jq 'select(.q_max_ms > 10)' | wc -l`)
	if p.Typed == nil {
		t.Fatal("no typed variant for a select on a top-level key")
	}
	if got := strings.Join(p.Typed.Requires, ","); got != ".q_max_ms" {
		t.Errorf("requires %q, want .q_max_ms", got)
	}
	if p.Typed.Namespace != "weft" || p.Typed.Service != "shard" || p.Typed.Kind != "stats" {
		t.Errorf("source = %+v", p.Typed)
	}
	if !strings.Contains(p.ShardSQL, "json_type") {
		t.Errorf("base SQL should branch on the JSON type: %s", p.ShardSQL)
	}
	if strings.Contains(p.Typed.ShardSQL, "json_type") ||
		!strings.Contains(p.Typed.ShardSQL, "TRY_CAST(json_extract(payload, '$.q_max_ms') AS DOUBLE) > 10") {
		t.Errorf("typed SQL = %s", p.Typed.ShardSQL)
	}
}

// Where a fact can't help, or can't be named, there is no variant.
func TestTypedPlan_AbsentWhereItCannotApply(t *testing.T) {
	for name, pipeline := range map[string]string{
		"string literal":           `cat /log/acme/api/main.log | awk '$5 == "abc"'`,
		"last field":               `cat /log/acme/api/main.log | awk '$NF > 5'`,
		"whole line":               `cat /log/acme/api/main.log | awk '$0 > 5'`,
		"no comparison":            `cat /log/acme/api/main.log | grep error | wc -l`,
		"kind glob":                `cat /log/acme/api/*.log | awk '$5 > 500'`,
		"two services":             `cat /log/acme/api/main.log /log/acme/web/main.log | awk '$5 > 500'`,
		"a whole namespace":        `cat /log/acme.log | awk '$5 > 500'`,
		"jq against a string":      `cat /log/acme/api/main.log | jq 'select(.s == "x")'`,
		"jq nested key":            `cat /log/acme/api/main.log | jq 'select(.a.b > 1)'`,
		"jq key after a step":      `cat /log/acme/api/main.log | jq '.a | select(.b > 1)'`,
		"jq against another field": `cat /log/acme/api/main.log | jq 'select(.a > .b)'`,
		"follow":                   `tail -f /log/acme/api/main.log | awk '$5 > 500'`,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Plan(pipeline, 7, "", time.Time{}, time.Time{})
			if err != nil {
				t.Skipf("not plannable: %v", err)
			}
			if p.Typed != nil {
				t.Errorf("unexpected typed variant requiring %v: %s", p.Typed.Requires, p.Typed.ShardSQL)
			}
		})
	}
}

// The base plan is what it was: a variant is beside it, never instead.
func TestTypedPlan_DoesNotChangeTheBasePlan(t *testing.T) {
	p := planTyped(t, `cat /log/acme/api/main.log | awk '$5 > 500' | wc -l`)
	if p.Typed == nil || p.Typed.ShardSQL == p.ShardSQL {
		t.Fatalf("typed = %+v", p.Typed)
	}
	if len(p.ComputeStages) == 0 || !strings.Contains(strings.Join(p.ComputeStages, "|"), "awk") {
		t.Errorf("the awk stage must still run in compute over what the shard returns: %v", p.ComputeStages)
	}
}
