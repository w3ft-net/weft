package planner

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

func TestRender_DuckDB(t *testing.T) {
	for _, c := range []struct {
		name string
		e    sqlExpr
		want string
	}{
		{"nil is no expression", nil, ""},
		{"regex", regexExpr{col: colRef("raw"), pattern: "it's"}, "regexp_matches(raw, 'it''s')"},
		{"case-insensitive regex", regexExpr{col: colRef("raw"), pattern: "a", ignoreCase: true}, "regexp_matches(raw, 'a', 'i')"},
		{"negated", notExpr{regexExpr{col: colRef("raw"), pattern: "a"}}, "NOT regexp_matches(raw, 'a')"},
		{"field compared with a string", cmpExpr{dialect.NE, coalesce{listIdx{colRef("fields"), 2}, strLit("")}, strLit("x")},
			"COALESCE(list_extract(fields, 2), '') != 'x'"},
		{"last field", coalesce{listIdx{colRef("fields"), -1}, colRef("raw")}, "COALESCE(list_extract(fields, -1), raw)"},
		{"typed comparison", typedComparison(listIdx{colRef("fields"), 3}, dialect.GT, 5), "TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 5"},
		{"and", andExpr{colRef("a"), colRef("b")}, "a AND b"},
		{"or is parenthesized", orExpr{terms: []sqlExpr{colRef("a"), colRef("b"), colRef("c")}}, "(a OR b OR c)"},
		{"wrapped or parenthesizes a compound term", orExpr{terms: []sqlExpr{andExpr{colRef("a"), colRef("b")}, colRef("c")}, wrap: true}, "((a AND b) OR c)"},
		// The pinned SQL records this: a term that is already an OR in parens is
		// found compound by its text and wrapped again.
		{"wrapped or wraps a parenthesized or again", orExpr{terms: []sqlExpr{orExpr{terms: []sqlExpr{colRef("a"), colRef("b")}}, colRef("c")}, wrap: true}, "(((a OR b)) OR c)"},
		{"length of a column", byteLen{colRef("raw")}, "strlen(raw)"},
		{"event id as text", castText{colRef("event_id")}, "CAST(event_id AS VARCHAR)"},
		{"is null", isNull{listIdx{colRef("fields"), 1}}, "list_extract(fields, 1) IS NULL"},
		{"print of two fields", concatWS{sep: strLit(" "), terms: []sqlExpr{colRef("raw"), strLit("x")}}, "concat_ws(' ', raw, 'x')"},
		{"concatenation", concatExpr{strLit("a"), colRef("raw")}, "concat('a', raw)"},
		{"cut of two ranges", coalesce{listJoin{listFlatten{
			listElement{splitExpr{colRef("raw"), strLit(",")}, 2},
			listFrom{splitExpr{colRef("raw"), strLit(",")}, 4},
		}, strLit(",")}, strLit("")},
			"COALESCE(array_to_string(flatten([[string_split(raw, ',')[2]], string_split(raw, ',')[4:]]), ','), '')"},
		{"cut range", listRange{splitExpr{colRef("raw"), strLit(",")}, 2, 3}, "string_split(raw, ',')[2:3]"},
	} {
		if got := render(c.e, dialect.DuckDB); got != c.want {
			t.Errorf("%s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}

func TestFeatures_FindsWhatAnExpressionNeeds(t *testing.T) {
	e := orExpr{terms: []sqlExpr{
		cmpExpr{dialect.EQ, tryCastDouble{listIdx{colRef("fields"), 1}}, numLit("1")},
		notExpr{regexExpr{col: coalesce{byteLen{colRef("raw")}, strLit("")}, pattern: "x"}},
	}}
	got := map[dialect.Feature]bool{}
	for _, f := range features(e) {
		got[f] = true
	}
	for _, f := range []dialect.Feature{dialect.TryCast, dialect.ListIndex, dialect.Regex, dialect.ByteLen} {
		if !got[f] {
			t.Errorf("feature %d not found", f)
		}
	}
	if len(features(colRef("a = 1"))) != 0 || len(features(nil)) != 0 {
		t.Error("an expression with no features reported some")
	}
}

// lacking is DuckDB without some features.
type lacking struct {
	dialect.Dialect
	no []dialect.Feature
}

func without(f ...dialect.Feature) dialect.Dialect { return lacking{dialect.DuckDB, f} }

func (l lacking) Supports(f dialect.Feature) bool { return !slices.Contains(l.no, f) }

func planFor(t *testing.T, d dialect.Dialect, pipeline string) (*QueryPlan, *compiled) {
	t.Helper()
	plan, c, err := compileBare(d, pipeline, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("%s: %v", pipeline, err)
	}
	return plan, c
}

// A stage the dialect can't express is not pushed: it runs in compute, and the
// shard's SQL holds none of what the dialect lacks. With the feature, the same
// pipeline pushes.
func TestDialectFeatures_AStageTheStoreCannotExpressRunsInCompute(t *testing.T) {
	for _, c := range []struct {
		name     string
		feature  dialect.Feature
		pipeline string
		sql      string // in the shard's SQL when the store can, never when it can't
		compute  int    // stages left to compute when it can't
	}{
		{"regex in grep", dialect.Regex, "cat vimgym.log | grep timeout", "regexp_matches", 1},
		{"regex in sed", dialect.Regex, "cat vimgym.log | sed -n '/timeout/p'", "regexp_matches", 1},
		{"regex in an awk pattern", dialect.Regex, "cat vimgym.log | awk '/timeout/'", "regexp_matches", 1},
		{"regex in an awk match", dialect.Regex, "cat vimgym.log | awk '$0 ~ /timeout/'", "regexp_matches", 1},
		{"splitting and lists in cut", dialect.Lists, "cat vimgym.log | cut -d, -f2", "string_split", 1},
		{"splitting in cut", dialect.Split, "cat vimgym.log | cut -d, -f2", "string_split", 1},
		{"jq", dialect.JSON, "cat vimgym.log | jq .msg", "json_", 1},
		{"hoisted filters", dialect.Lines, "cat vimgym.log | sort | grep timeout", "unnest", 2},
		{"grep -c", dialect.Lines, "cat vimgym.log | grep -c timeout", "unnest", 1},
		{"aggregates", dialect.Aggregates, "cat vimgym.log | sort | uniq -c", "GROUP BY", 2},
		{"field columns", dialect.ListIndex, "cat vimgym.log | awk '{ s += $3 } END { print s }'", "list_extract", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			can, _ := planFor(t, dialect.DuckDB, c.pipeline)
			if all := can.ShardSQL + " " + can.FieldSQL; !strings.Contains(all, c.sql) {
				t.Fatalf("DuckDB does not push %q here, so the case proves nothing:\n%s", c.sql, all)
			}
			cannot, _ := planFor(t, without(c.feature), c.pipeline)
			if all := cannot.ShardSQL + " " + cannot.FieldSQL; strings.Contains(all, c.sql) {
				t.Errorf("without the feature the SQL still holds %q:\n%s", c.sql, all)
			}
			if got := len(cannot.ComputeStages); got != c.compute {
				t.Errorf("compute stages = %v, want %d", cannot.ComputeStages, c.compute)
			}
		})
	}
}

func TestLowering_ARefusedStageSaysItIsTheStore(t *testing.T) {
	_, c := planFor(t, without(dialect.Regex), "cat vimgym.log | grep timeout")
	if op, ok := c.stages[0].nodes[0].(*opaqueNode); !ok || op.why.code != reasonDialect {
		t.Fatalf("want an opaque node because of the dialect, got %#v", c.stages[0].nodes[0])
	}
}

// A trace source has no compute to leave it to: the plan is refused.
func TestSpans_RefusedWhereTheStoreCannotRunThem(t *testing.T) {
	for _, pipeline := range []string{"trace --spans", "trace", "trace --id abc123 --spans"} {
		if _, _, err := compileBare(dialect.DuckDB, pipeline, "", time.Time{}, time.Time{}); err != nil {
			t.Fatalf("DuckDB: %s: %v", pipeline, err)
		}
		if _, _, err := compileBare(without(dialect.Spans), pipeline, "", time.Time{}, time.Time{}); !errors.Is(err, ErrNoSpans) {
			t.Errorf("%s: err = %v, want ErrNoSpans", pipeline, err)
		}
	}
	// Lines of one trace are rows like any other.
	if _, _, err := compileBare(without(dialect.Spans), "trace --id abc123", "", time.Time{}, time.Time{}); err != nil {
		t.Errorf("a trace's own lines: %v", err)
	}
}

func TestRender_ATypedChoiceIsItsBaseUnlessTyped(t *testing.T) {
	e := andExpr{
		typedChoice{locator: "$3", base: colRef("slow"), typed: colRef("fast")},
		colRef("other"),
	}
	if got := render(e, dialect.DuckDB); got != "slow AND other" {
		t.Errorf("base: %s", got)
	}
	if got := renderIn(e, dialect.DuckDB, true); got != "fast AND other" {
		t.Errorf("typed: %s", got)
	}
	if got := typedChoices(e); len(got) != 1 || got[0].locator != "$3" {
		t.Errorf("choices = %v", got)
	}
	// Features are what the base form needs: the typed form is only read
	// where the facts hold, and typedVariant checks it then.
	if len(features(typedChoice{base: colRef("a"), typed: tryCastDouble{colRef("a")}})) != 0 {
		t.Error("a choice reported the features of its typed form")
	}
}

func TestTypedVariant_SwapsEveryChoiceInEveryQuery(t *testing.T) {
	plan, _ := planFor(t, dialect.DuckDB, "cat /log/acme/api/main.log | awk '$3 > 5'")
	if plan.Typed == nil {
		t.Fatal("no typed variant")
	}
	if want := []string{"$3"}; !slices.Equal(plan.Typed.Requires, want) {
		t.Errorf("requires = %v, want %v", plan.Typed.Requires, want)
	}
	const typed = "TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 5"
	for name, sql := range map[string]string{"shard": plan.Typed.ShardSQL, "direct": plan.Typed.DirectShardSQL} {
		if !strings.Contains(sql, typed) || strings.Contains(sql, "regexp_matches") {
			t.Errorf("%s SQL is not the typed form:\n%s", name, sql)
		}
	}
	if !strings.Contains(plan.ShardSQL, "regexp_matches") {
		t.Errorf("the base plan lost its general form:\n%s", plan.ShardSQL)
	}
}

// The same comparison twice in one pattern is the same text twice in the SQL.
// Matching rendered text could not tell the two apart and declined; a choice is
// its own site, so both swap.
func TestTypedVariant_ARepeatedComparisonStillSwaps(t *testing.T) {
	plan, _ := planFor(t, dialect.DuckDB, "cat /log/acme/api/main.log | awk '$3 > 5 || $3 > 5'")
	if plan.Typed == nil {
		t.Fatal("a repeated comparison lost its typed variant")
	}
	if n := strings.Count(plan.Typed.ShardSQL, "TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 5"); n != 2 {
		t.Errorf("typed comparisons = %d, want 2:\n%s", n, plan.Typed.ShardSQL)
	}
	if strings.Contains(plan.Typed.ShardSQL, "regexp_matches") {
		t.Errorf("a base form is left:\n%s", plan.Typed.ShardSQL)
	}
	if n := strings.Count(plan.ShardSQL, "regexp_matches"); n != 2 {
		t.Errorf("the base plan has %d general forms, want 2:\n%s", n, plan.ShardSQL)
	}
}

// A typed form the dialect can't spell is not offered: the shard would be sent
// SQL it can't run.
func TestTypedVariant_DeclinedWhereTheTypedFormCannotBeSpelled(t *testing.T) {
	plan, _ := planFor(t, without(dialect.TryCast), "cat /log/acme/api/main.log | awk '$3 > 5'")
	if plan.Typed != nil {
		t.Errorf("typed variant offered without TryCast: %s", plan.Typed.ShardSQL)
	}
	if strings.Contains(plan.ShardSQL, "TRY_CAST") {
		t.Errorf("the base plan needs TryCast:\n%s", plan.ShardSQL)
	}
}

func TestTypedVariant_JqKeyComparison(t *testing.T) {
	plan, _ := planFor(t, dialect.DuckDB, "cat /log/acme/api/main.log | jq 'select(.n > 5)'")
	if plan.Typed == nil || !slices.Equal(plan.Typed.Requires, []string{".n"}) {
		t.Fatalf("typed = %+v", plan.Typed)
	}
	if want := "TRY_CAST(json_extract(payload, '$.n') AS DOUBLE) > 5"; !strings.Contains(plan.Typed.ShardSQL, want) {
		t.Errorf("typed SQL lacks %q:\n%s", want, plan.Typed.ShardSQL)
	}
}
