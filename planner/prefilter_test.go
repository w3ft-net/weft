package planner

import (
	"slices"
	"testing"
	"time"
)

// TestPrefilter_StageStillRuns covers stages whose predicate pushes as a
// prefilter: SQL narrows the rows, and the stage still runs, verbatim, in
// compute: an awk pattern that's only partly lowerable (an AND keeps the
// side that lowers) or a pattern with a non-default action.
func TestPrefilter_StageStillRuns(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name:        "nested AND inside OR",
			pipeline:    `cat vimgym.log | awk '(LEVEL == "ERROR" && $3 > 100) || LEVEL == "FATAL"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, namespace, service, kind, level, instance, deployment, env, version, region, cluster, k8s_namespace, pod, container, node, trace_id, span_name, span_id, parent_span_id, duration_ns FROM logs WHERE namespace = ? AND ((level = 'ERROR' AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 100 OR list_extract(fields, 3) > '100' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))) OR level = 'FATAL')`,
			wantCompute: []string{`awk '(LEVEL == "ERROR" && $3 > 100) || LEVEL == "FATAL"'`},
		},
		{
			name:        "mixed AND with field and meta-var",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR" && $3 > 100'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, namespace, service, kind, level, instance, deployment, env, version, region, cluster, k8s_namespace, pod, container, node, trace_id, span_name, span_id, parent_span_id, duration_ns FROM logs WHERE namespace = ? AND level = 'ERROR' AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 100 OR list_extract(fields, 3) > '100' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk 'LEVEL == "ERROR" && $3 > 100'`},
		},
		{
			name:        "two awk filters",
			pipeline:    `cat vimgym.log | awk 'LEVEL == "ERROR"' | awk '$3 > 100'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR' AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 100 OR list_extract(fields, 3) > '100' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk '$3 > 100'`},
		},
	})
}

// TestJq_SelectChainIsExact covers weft's jq row filter: a program made
// only of select(...)s keeps or drops the row, which stays its raw line,
// so when every select lowers the stage is exactly its WHERE clause and
// later stages keep pushing. See docs/plans/planner-ir.md, "jq".
func TestJq_SelectChainIsExact(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name:        "select boolean true",
			pipeline:    `cat vimgym.log | jq 'select(.success == true)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.success'), 'NULL') IN ('BOOLEAN') AND CAST(json_extract(payload, '$.success') AS BOOLEAN) = true)`,
			wantCompute: nil,
		},
		{
			name:        "select less than",
			pipeline:    `cat vimgym.log | jq 'select(.duration_ms < 100)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.duration_ms'), 'NULL') IN ('NULL', 'BOOLEAN') OR (COALESCE(json_type(payload, '$.duration_ms'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.duration_ms') AS DOUBLE) < 100))`,
			wantCompute: nil,
		},
		{
			name:        "select gte",
			pipeline:    `cat vimgym.log | jq 'select(.retries >= 3)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.retries'), 'NULL') IN ('VARCHAR', 'ARRAY', 'OBJECT') OR (COALESCE(json_type(payload, '$.retries'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.retries') AS DOUBLE) >= 3))`,
			wantCompute: nil,
		},
		{
			name:        "three chained selects",
			pipeline:    `cat vimgym.log | jq 'select($level == "ERROR") | select(.status == 500) | select(.action == "login")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR' AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500) AND (COALESCE(json_type(payload, '$.action'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.action') = 'login')`,
			wantCompute: nil,
		},
		{
			name:        "select on json field equality",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500)`,
			wantCompute: nil,
		},
		{
			name:        "select on json field string",
			pipeline:    `cat vimgym.log | jq 'select(.action == "login")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.action'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.action') = 'login')`,
			wantCompute: nil,
		},
		{
			name:        "select on meta _app",
			pipeline:    `cat warp.log | jq 'select($namespace == "vimgym")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = 'vimgym'`,
			wantCompute: nil,
		},
		{
			name:        "select on meta _level",
			pipeline:    `cat vimgym.log | jq 'select($level == "ERROR")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: nil,
		},
		{
			name:        "select with numeric comparison",
			pipeline:    `cat vimgym.log | jq 'select(.duration_ms > 1000)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.duration_ms'), 'NULL') IN ('VARCHAR', 'ARRAY', 'OBJECT') OR (COALESCE(json_type(payload, '$.duration_ms'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.duration_ms') AS DOUBLE) > 1000))`,
			wantCompute: nil,
		},
		{
			name:        "select not equal",
			pipeline:    `cat vimgym.log | jq 'select(.status != 200)'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND NOT (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 200)`,
			wantCompute: nil,
		},
		{
			name:        "chained selects inside jq",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | select(.action == "login")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500) AND (COALESCE(json_type(payload, '$.action'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.action') = 'login')`,
			wantCompute: nil,
		},
		{
			name:        "two jq selects",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500)' | jq 'select(.action == "login")'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500) AND (COALESCE(json_type(payload, '$.action'), 'NULL') IN ('VARCHAR') AND json_extract_string(payload, '$.action') = 'login')`,
			wantCompute: nil,
		},
		{
			name:        "jq select then grep",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500)' | grep timeout`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500) AND regexp_matches(raw, 'timeout')`,
			wantCompute: nil,
		},
	})
}

// TestJq_RowFilter covers a select chain the shard can't fully apply:
// the compute engine runs it on each row's payload, after any head/tail
// limit, and keeps the raw line. Selects that lower still prefilter.
func TestJq_RowFilter(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name:          "select whose condition does not lower",
			pipeline:      `cat vimgym.log | jq 'select(has("error"))'`,
			wantSQL:       `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ?`,
			wantCompute:   nil,
			wantRowStages: []string{`jq 'select(has("error"))'`},
		},
		{
			name:          "select comparing two fields",
			pipeline:      `cat vimgym.log | jq 'select(.start < .stop)'`,
			wantSQL:       `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ?`,
			wantCompute:   nil,
			wantRowStages: []string{`jq 'select(.start < .stop)'`},
		},
		{
			name:          "select with arithmetic",
			pipeline:      `cat vimgym.log | jq 'select((.x + .y) > 100)'`,
			wantSQL:       `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ?`,
			wantCompute:   nil,
			wantRowStages: []string{`jq 'select((.x + .y) > 100)'`},
		},
		{
			name:          "literal on the left does not lower",
			pipeline:      `cat vimgym.log | jq 'select(500 == .status)'`,
			wantSQL:       `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ?`,
			wantCompute:   nil,
			wantRowStages: []string{`jq 'select(500 == .status)'`},
		},
		{
			name:          "lowered select prefilters, the rest filters in compute",
			pipeline:      `cat vimgym.log | jq 'select(.status == 500) | select(has("error"))'`,
			wantSQL:       `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ? AND (COALESCE(json_type(payload, '$.status'), 'NULL') IN ('BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT') AND CAST(json_extract(payload, '$.status') AS DOUBLE) = 500)`,
			wantCompute:   nil,
			wantRowStages: []string{`jq 'select(.status == 500) | select(has("error"))'`},
		},
		{
			name:          "select after tail runs in compute, on the last N rows",
			pipeline:      `tail -n 5 vimgym.log | jq 'select(.status == 500)'`,
			wantSQL:       `SELECT raw, ts, event_id, line_no, payload FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 5) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantCompute:   nil,
			wantRowStages: []string{`jq 'select(.status == 500)'`},
		},
		{
			name:          "row filter then a compute stage",
			pipeline:      `tail -n 5 vimgym.log | jq 'select(.status == 500)' | wc -l`,
			wantSQL:       `SELECT raw, ts, event_id, line_no, payload FROM (SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, CAST(payload AS VARCHAR) AS payload FROM logs WHERE namespace = ? ORDER BY ts DESC, event_id DESC NULLS LAST, line_no DESC NULLS LAST LIMIT 5) AS t ORDER BY ts, event_id NULLS FIRST, line_no NULLS FIRST`,
			wantCompute:   []string{`wc -l`},
			wantRowStages: []string{`jq 'select(.status == 500)'`},
		},
	})
}

// TestPrefilter_PositionalFields pins $N lowering. Ingest splits the
// fields column the way awk splits $0, so $N is fields[N] (NULL past the
// last field, which awk reads as unset). A string constant compares as
// a string, exactly; a number compares as a number or a string by the
// field's value, so that lowers to a prefilter covering both.
func TestPrefilter_PositionalFields(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name:        "high positional field",
			pipeline:    `cat vimgym.log | awk '$15 == "value"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND list_extract(fields, 15) = 'value'`,
			wantCompute: nil,
		},
		{
			name:        "field 1",
			pipeline:    `cat vimgym.log | awk '$1 == "2026-03-28"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND list_extract(fields, 1) = '2026-03-28'`,
			wantCompute: nil,
		},
		{
			name:        "missing field equals empty string",
			pipeline:    `cat vimgym.log | awk '$9 == ""'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND COALESCE(list_extract(fields, 9), '') = ''`,
			wantCompute: nil,
		},
		{
			name:        "field matches regex",
			pipeline:    `cat vimgym.log | awk '$2 !~ /^GET/'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND NOT regexp_matches(COALESCE(list_extract(fields, 2), ''), '^GET')`,
			wantCompute: nil,
		},
		{
			name:        "field numeric equality",
			pipeline:    `cat vimgym.log | awk '$3 == 200'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) = 200 OR list_extract(fields, 3) = '200' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk '$3 == 200'`},
		},
		{
			name:        "field numeric not equal",
			pipeline:    `cat vimgym.log | awk '$3 != 0'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) != 0 OR list_extract(fields, 3) != '0' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk '$3 != 0'`},
		},
		{
			name:        "field greater than int",
			pipeline:    `cat vimgym.log | awk '$3 > 100'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) > 100 OR list_extract(fields, 3) > '100' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk '$3 > 100'`},
		},
		{
			name:        "field less than int: a missing field is 0",
			pipeline:    `cat vimgym.log | awk '$3 < 50'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (list_extract(fields, 3) IS NULL OR TRY_CAST(list_extract(fields, 3) AS DOUBLE) < 50 OR list_extract(fields, 3) < '50' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk '$3 < 50'`},
		},
		{
			name:        "numeric-looking string constant compares as a number",
			pipeline:    `cat vimgym.log | awk '$3 == "200"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ? AND (TRY_CAST(list_extract(fields, 3) AS DOUBLE) = 200 OR list_extract(fields, 3) = '200' OR regexp_matches(list_extract(fields, 3), '^[+-]?(?i:inf|nan|0x)'))`,
			wantCompute: []string{`awk '$3 == "200"'`},
		},
		{
			name:        "computed field never pushes",
			pipeline:    `cat vimgym.log | awk '$(NF-1) == "x"'`,
			wantSQL:     `SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk '$(NF-1) == "x"'`},
		},
		{
			// cut splits on each tab, not on runs of blanks like
			// fields, so it splits raw itself.
			name:     "cut splits raw",
			pipeline: `cat vimgym.log | cut -f3`,
			wantSQL:  "SELECT COALESCE(array_to_string([string_split(raw, '\t')[3]], '\t'), '') as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?",
		},
		{
			name:        "print of positional fields projects",
			pipeline:    `cat vimgym.log | awk '{ print $2, $3 }'`,
			wantSQL:     `SELECT concat_ws(' ', COALESCE(list_extract(fields, 2), ''), COALESCE(list_extract(fields, 3), '')) as raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs WHERE namespace = ?`,
			wantCompute: nil,
		},
	})
}

type planCase struct {
	name        string
	pipeline    string
	wantSQL     string
	wantCompute []string
	// wantRowStages is QueryPlan.RowStages; nil for most cases.
	wantRowStages []string
}

func runPlanCases(t *testing.T, cases []planCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages\n got: %q\nwant: %q", plan.ComputeStages, tt.wantCompute)
			}
			if !slices.Equal(plan.RowStages, tt.wantRowStages) {
				t.Errorf("RowStages = %q, want %q", plan.RowStages, tt.wantRowStages)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}
