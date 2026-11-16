package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// other is DuckDB, written differently where a test can see it.
type other struct {
	dialect.Dialect
	noRegex bool
}

func (other) Name() string { return "other" }

func (o other) Supports(f dialect.Feature) bool { return !(o.noRegex && f == dialect.Regex) }

func (other) Regex(col, pattern string, ignoreCase bool) string {
	return col + " ~ " + dialect.DuckDB.StringLit(pattern)
}

func useDialects(t *testing.T, d Dialects) {
	t.Helper()
	SetDialects(d)
	t.Cleanup(func() { SetDialects(Dialects{}) })
}

func planTiers(t *testing.T, pipeline string) *QueryPlan {
	t.Helper()
	plan, err := Plan(pipeline, shapetest.Group(1), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	return plan
}

const tierPipeline = "cat /log/acme/api/main.log | grep timeout"

func TestPlan_OneDialectNeedsNoArchiveSQL(t *testing.T) {
	plan := planTiers(t, tierPipeline)
	if plan.ArchiveDirectShardSQL != "" {
		t.Errorf("archive SQL = %q with both tiers on one dialect", plan.ArchiveDirectShardSQL)
	}
	if !strings.Contains(plan.DirectShardSQL, "regexp_matches") {
		t.Errorf("live SQL: %s", plan.DirectShardSQL)
	}
}

func TestPlan_TiersOnDifferentDialectsGetTheSamePlanSpelledTwice(t *testing.T) {
	useDialects(t, Dialects{Live: dialect.DuckDB, Archive: other{Dialect: dialect.DuckDB}})
	plan := planTiers(t, tierPipeline)
	if !strings.Contains(plan.DirectShardSQL, "regexp_matches(raw, 'timeout')") {
		t.Errorf("live SQL is not in the live dialect: %s", plan.DirectShardSQL)
	}
	if !strings.Contains(plan.ArchiveDirectShardSQL, "raw ~ 'timeout'") || strings.Contains(plan.ArchiveDirectShardSQL, "regexp_matches") {
		t.Errorf("archive SQL is not in the archive dialect: %s", plan.ArchiveDirectShardSQL)
	}
	// What differs is the spelling; the plan around it is the same.
	want := strings.Replace(plan.DirectShardSQL, "regexp_matches(raw, 'timeout')", "raw ~ 'timeout'", 1)
	if plan.ArchiveDirectShardSQL != want {
		t.Errorf("archive SQL differs by more than the regex:\n got: %s\nwant: %s", plan.ArchiveDirectShardSQL, want)
	}
	if len(plan.ComputeStages) != 0 {
		t.Errorf("compute stages = %v, want the grep pushed into both tiers", plan.ComputeStages)
	}
}

// A tier that can't spell a filter keeps it out of the other too: what one tier
// leaves to compute, the other must, or its rows would come back unfiltered.
func TestPlan_AFilterEitherTierCannotSpellRunsInComputeForBoth(t *testing.T) {
	for name, ds := range map[string]Dialects{
		"archive lacks it": {Live: dialect.DuckDB, Archive: other{Dialect: dialect.DuckDB, noRegex: true}},
		"live lacks it":    {Live: other{Dialect: dialect.DuckDB, noRegex: true}, Archive: dialect.DuckDB},
	} {
		t.Run(name, func(t *testing.T) {
			useDialects(t, ds)
			plan := planTiers(t, tierPipeline)
			if len(plan.ComputeStages) != 1 {
				t.Fatalf("compute stages = %v, want the grep", plan.ComputeStages)
			}
			for tier, sql := range map[string]string{"live": plan.DirectShardSQL, "archive": plan.ArchiveDirectShardSQL} {
				if strings.Contains(sql, "regexp_matches") || strings.Contains(sql, " ~ ") {
					t.Errorf("%s SQL still filters: %s", tier, sql)
				}
			}
		})
	}
}

func TestSetDialects_ATierLeftNilIsDuckDB(t *testing.T) {
	useDialects(t, Dialects{Archive: other{Dialect: dialect.DuckDB}})
	if LiveDialect() != "duckdb" || ArchiveDialect() != "other" {
		t.Errorf("tiers = %s, %s", LiveDialect(), ArchiveDialect())
	}
	useDialects(t, Dialects{})
	if LiveDialect() != "duckdb" || ArchiveDialect() != "duckdb" {
		t.Errorf("the zero Dialects is %s, %s", LiveDialect(), ArchiveDialect())
	}
}

func TestExplainAndAnalyze_PlanForTheConfiguredTiers(t *testing.T) {
	useDialects(t, Dialects{Live: dialect.DuckDB, Archive: other{Dialect: dialect.DuckDB, noRegex: true}})
	if out := Explain(tierPipeline); !strings.Contains(out, "grep timeout") || strings.Contains(out, "regexp_matches") {
		t.Errorf("Explain pushed a filter a tier can't run:\n%s", out)
	}
	a, err := Analyze(tierPipeline, "/", nil)
	if err != nil || a.Barrier == 0 {
		t.Errorf("Analyze: barrier = %v, err = %v; want the grep to be the barrier", a, err)
	}
}

func lowerTiers(t *testing.T) *LoweredPlan {
	t.Helper()
	lp, err := Lower(tierPipeline, shapetest.Group(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	return lp
}

func TestLower_RecordsTheDialectsItIsWrittenFor(t *testing.T) {
	// Both tiers DuckDB: nothing is recorded, so a plan stored before the names
	// existed reads the same, and fits.
	lp := lowerTiers(t)
	if lp.LiveDialect != "" || lp.ArchiveDialect != "" || lp.ArchiveDirectShardSQL != "" {
		t.Errorf("a default plan recorded %+v", lp)
	}
	if !lp.FitsDialects() {
		t.Error("a default plan does not fit a default deployment")
	}

	useDialects(t, Dialects{Live: dialect.DuckDB, Archive: other{Dialect: dialect.DuckDB}})
	if lp.FitsDialects() {
		t.Error("a plan written for DuckDB on both tiers fits a deployment with another archive")
	}
	split := lowerTiers(t)
	if split.LiveDialect != "" || split.ArchiveDialect != "other" {
		t.Errorf("recorded dialects = %q, %q", split.LiveDialect, split.ArchiveDialect)
	}
	if !strings.Contains(split.ArchiveDirectShardSQL, "raw ~ 'timeout'") {
		t.Errorf("archive SQL = %q", split.ArchiveDirectShardSQL)
	}
	if !split.FitsDialects() {
		t.Error("a plan does not fit the deployment it was lowered under")
	}
}
