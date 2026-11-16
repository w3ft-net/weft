package dialect_test

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

func TestDuckDB_Spellings(t *testing.T) {
	d := dialect.DuckDB
	for _, c := range []struct{ got, want string }{
		{d.StringLit("it's"), "'it''s'"},
		{d.And("a", "b"), "a AND b"},
		{d.Or("a", "b"), "a OR b"},
		{d.Not("a"), "NOT a"},
		{d.Cmp(dialect.NE, "x", "1"), "x != 1"},
		{d.Cmp(dialect.EQ, "x", "'a'"), "x = 'a'"},
		{d.IsNull("x"), "x IS NULL"},
		{d.Coalesce("a", "''"), "COALESCE(a, '')"},
		{d.CastText("event_id"), "CAST(event_id AS VARCHAR)"},
		{d.Regex("raw", "timeout", false), "regexp_matches(raw, 'timeout')"},
		{d.Regex("raw", "timeout", true), "regexp_matches(raw, 'timeout', 'i')"},
		{d.Regex("raw", "it's", false), "regexp_matches(raw, 'it''s')"},
		{d.Regex("f", `^[+-]?(?i:inf|nan|0x)`, false), `regexp_matches(f, '^[+-]?(?i:inf|nan|0x)')`},
		{d.ListIndex("fields", 2), "list_extract(fields, 2)"},
		{d.ListIndex("fields", -1), "list_extract(fields, -1)"},
		{d.TryCastDouble("x"), "TRY_CAST(x AS DOUBLE)"},
		{d.ByteLen("raw"), "strlen(raw)"},
		{d.Concat("a", "b"), "concat(a, b)"},
		{d.ConcatWS("' '", "a", "b", "c"), "concat_ws(' ', a, b, c)"},
		{d.Split("raw", "','"), "string_split(raw, ',')"},
		{d.ListElement("p", 3), "[p[3]]"},
		{d.ListFrom("p", 3), "p[3:]"},
		{d.ListRange("p", 2, 4), "p[2:4]"},
		{d.ListFlatten("a", "b"), "flatten([a, b])"},
		{d.ListJoin("l", "','"), "array_to_string(l, ',')"},
		{d.ListLen("fields"), "len(fields)"},
		{d.SplitLines("raw"), "unnest(string_split(raw, chr(10)))"},
		{d.TimestampParam(), "CAST(? AS TIMESTAMP)"},
		{d.TypedParam(dialect.ParamInt64), "CAST(? AS BIGINT)"},
		{d.TypedParam(dialect.ParamText), "CAST(? AS VARCHAR)"},
		{d.CastInt64("x"), "CAST(x AS BIGINT)"},
		{d.TryCastInt64("x"), "TRY_CAST(x AS BIGINT)"},
		{d.EpochNs("ts"), "epoch_ns(ts)"},
		{d.FirstBy("txt", "ORDER BY v DESC"), "first(txt ORDER BY v DESC)"},
		{d.BoolLit(false), "false"},
		{d.Append("'a'", "x", "'b'"), "'a' || x || 'b'"},
		{d.Case([][2]string{{"a", "1"}, {"b", "2"}}, "3"), "CASE WHEN a THEN 1 WHEN b THEN 2 ELSE 3 END"},
		{d.JSONExtract("payload", "$.a.b"), "json_extract(payload, '$.a.b')"},
		{d.JSONAsNumber("payload", "$.a"), "CAST(json_extract(payload, '$.a') AS DOUBLE)"},
		{d.JSONAsBool("payload", "$.a"), "CAST(json_extract(payload, '$.a') AS BOOLEAN)"},
		{d.JSONAsString("payload", "$.a"), "json_extract_string(payload, '$.a')"},
		{d.JSONKindIs("payload", "$", dialect.KindNull, dialect.KindObject), "COALESCE(json_type(payload, '$'), 'NULL') IN ('NULL', 'OBJECT')"},
		{d.JSONKindIs("payload", "$.n", dialect.KindNumber), "COALESCE(json_type(payload, '$.n'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT')"},
		{d.JSONIsObject("payload"), "json_type(payload) = 'OBJECT'"},
		{d.JSONObject("'k'", "v"), "json_object('k', v)"},
		{d.JSONOf("x"), "to_json(x)"},
	} {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
}

func TestDuckDB_SupportsEveryFeature(t *testing.T) {
	for _, f := range []dialect.Feature{dialect.Regex, dialect.ListIndex, dialect.TryCast, dialect.ByteLen, dialect.Split, dialect.Lists, dialect.JSON, dialect.Lines, dialect.Aggregates, dialect.Spans} {
		if !dialect.DuckDB.Supports(f) {
			t.Errorf("DuckDB does not support feature %d", f)
		}
	}
}

func TestDuckDB_JSONTextWrapsTheLeafForRaw(t *testing.T) {
	d := dialect.DuckDB
	plain := d.JSONText("payload", "$.a", false)
	raw := d.JSONText("payload", "$.a", true)
	if !strings.HasPrefix(plain, "COALESCE(CASE WHEN json_type(payload, '$.a') IN ('BIGINT', 'UBIGINT')") || !strings.HasSuffix(plain, "END, 'null')") {
		t.Errorf("plain text: %s", plain)
	}
	want := "CASE WHEN json_type(payload, '$.a') = 'VARCHAR' THEN json_extract_string(payload, '$.a') ELSE " + plain + " END"
	if raw != want {
		t.Errorf("raw text:\n got: %s\nwant: %s", raw, want)
	}
}

func TestDuckDB_RenderSpans(t *testing.T) {
	base := dialect.SpanQuery{Table: "logs", Where: " WHERE a = ?", CountColumn: "n", TraceListLimit: 50, SamplesLimit: 20, LogGamma: 0.02}
	for _, c := range []struct {
		kind string
		want []string
	}{
		{dialect.AggSpanRows, []string{"FROM logs WHERE a = ? ORDER BY raw", "AS raw, ts, 1 AS n"}},
		{dialect.AggTraceIDs, []string{"GROUP BY trace_id ORDER BY max(ts) DESC LIMIT 50) AS s ORDER BY raw"}},
		{dialect.AggTraceList, []string{"arg_min(span_name, ts) FILTER (WHERE span_name IS NOT NULL", "FROM logs WHERE a = ? GROUP BY trace_id ORDER BY raw"}},
		{dialect.AggSpanSamples, []string{"FROM (SELECT * FROM logs WHERE a = ? ORDER BY duration_ns DESC LIMIT 20) AS s ORDER BY raw"}},
		{dialect.AggSpans, []string{"ceil(ln(duration_ns) / 0.02)", "FROM logs WHERE a = ?) AS s GROUP BY span_name, namespace, service, bucket ORDER BY raw"}},
	} {
		q := base
		q.Kind = c.kind
		got := dialect.DuckDB.RenderSpans(q)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in:\n%s", c.kind, w, got)
			}
		}
	}
}

type noFeature struct {
	dialect.Dialect
	name string
	no   dialect.Feature
}

func (n noFeature) Name() string                    { return n.name }
func (n noFeature) Supports(f dialect.Feature) bool { return f != n.no }

// runs numbers the test's registrations: the registry is the process's, so a
// test run again in the same process (-count) must not register a name twice.
var runs atomic.Int64

func TestRegistry(t *testing.T) {
	if d, err := dialect.Lookup("duckdb"); err != nil || d.Name() != "duckdb" {
		t.Fatalf("duckdb: %v, %v", d, err)
	}
	if _, err := dialect.Lookup("nope"); err == nil || !strings.Contains(err.Error(), "duckdb") {
		t.Errorf("an unknown name: %v", err)
	}
	if dialect.Register(dialect.DuckDB) == nil {
		t.Error("a name registered twice")
	}
	name := fmt.Sprintf("no-regex-%d", runs.Add(1))
	if err := dialect.Register(noFeature{dialect.DuckDB, name, dialect.Regex}); err != nil {
		t.Fatal(err)
	}
	if d, err := dialect.Lookup(name); err != nil || d.Supports(dialect.Regex) {
		t.Errorf("%s: %v, %v", name, d, err)
	}
}

func TestMeet_HasOnlyWhatBothHave_AndSpellsAsTheFirst(t *testing.T) {
	a := dialect.DuckDB
	b := noFeature{dialect.DuckDB, "b", dialect.Regex}
	m := dialect.Meet(a, b)
	if m.Supports(dialect.Regex) {
		t.Error("the meet has what only one dialect has")
	}
	if !m.Supports(dialect.JSON) {
		t.Error("the meet lost what both have")
	}
	if m.Name() != "duckdb" || m.StringLit("x") != "'x'" {
		t.Errorf("the meet is not spelled by the first: %s", m.Name())
	}
	if dialect.Meet(a, a) != a {
		t.Error("the meet of a dialect with itself is not itself")
	}
}
