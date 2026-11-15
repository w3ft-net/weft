package difftest

import (
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// Analyze must describe what Plan does. Every stage Plan leaves to run
// after the shard (its row and compute stages) is a stage Analyze says is
// not entirely at the source, and no other stage is.
func TestAnalyzeAgreesWithPlan(t *testing.T) {
	checked := 0
	for _, p := range corpus(t) {
		plan, perr := planner.Plan(p, shapetest.Tenant(1), "", time.Time{}, time.Time{})
		a, aerr := planner.Analyze(p, "", nil)
		if (perr == nil) != (aerr == nil) {
			t.Errorf("%s: Plan error %v, Analyze error %v", p, perr, aerr)
			continue
		}
		if perr != nil {
			continue
		}
		checked++
		want := append(append([]string{}, plan.RowStages...), plan.ComputeStages...)
		var got []string
		for _, s := range a.Stages {
			if s.Where != planner.Optimized {
				got = append(got, s.Src)
			}
		}
		sort.Strings(want)
		sort.Strings(got)
		if len(want) != len(got) {
			t.Errorf("%s:\n  Plan runs after the shard: %q\n  Analyze says not at the source: %q", p, want, got)
			continue
		}
		for i := range want {
			if want[i] != got[i] {
				t.Errorf("%s:\n  Plan runs after the shard: %q\n  Analyze says not at the source: %q", p, want, got)
				break
			}
		}
		if (a.Barrier == 0) != (len(got) == 0) {
			t.Errorf("%s: barrier %d with %d stages after transfer", p, a.Barrier, len(got))
		}
	}
	if checked < 100 {
		t.Fatalf("only %d corpus pipelines planned; the corpus is broken", checked)
	}
}

// What a user reads never names how weft stores or executes a query.
var leaked = regexp.MustCompile(`(?i)\b(shards?|compute|sql|prefilters?|pushdown|push-down|pushed|opaque|warp|gpipe|duckdb|parquet|predicates?|planner|coordinator|IR|at the source|after transfer)\b`)

func TestAnalyzeNamesNoInternals(t *testing.T) {
	seen := 0
	for _, p := range corpus(t) {
		a, err := planner.Analyze(p, "", nil)
		if err != nil {
			continue
		}
		for _, s := range a.Stages {
			texts := []string{s.Where.String()}
			if s.Why != nil {
				texts = append(texts, s.Why.Because, s.Why.Try)
				seen++
			}
			for _, text := range texts {
				if m := leaked.FindString(text); m != "" {
					t.Errorf("%s: stage %d says %q, which names %q", p, s.Index, text, m)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no stage in the corpus has advice to check")
	}
}
