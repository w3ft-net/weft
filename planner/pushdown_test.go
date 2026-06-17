package planner

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Tier 1: Single-stage pushdown
//
// Each case is a pipeline with one stage after the source command.  The
// planner should push the entire stage down into SQL and leave nothing for
// the compute layer.
// ---------------------------------------------------------------------------

func TestPushdown_SingleStage_Awk(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// Meta-variable equality
		{
			name:     "level equals string",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
		},
		{
			name:     "app equals string",
			pipeline: `cat warp.log | awk '$NAMESPACE == "vimgym"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'vimgym'`,
		},
		{
			name:     "instance equals string",
			pipeline: `cat vimgym.log | awk '$INSTANCE == "i-abc123"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND instance = 'i-abc123'`,
		},

		// Meta-variable inequality / comparison
		{
			name:     "level not equal",
			pipeline: `cat vimgym.log | awk '$LEVEL != "DEBUG"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level != 'DEBUG'`,
		},

		// Positional field comparison (numeric)
		{
			name:     "field greater than int",
			pipeline: `cat vimgym.log | awk '$3 > 100'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(list_extract(fields, 3) AS DOUBLE) > 100`,
		},
		{
			name:     "field less than int",
			pipeline: `cat vimgym.log | awk '$3 < 50'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(list_extract(fields, 3) AS DOUBLE) < 50`,
		},
		{
			name:     "field greater than or equal",
			pipeline: `cat vimgym.log | awk '$3 >= 100'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(list_extract(fields, 3) AS DOUBLE) >= 100`,
		},
		{
			name:     "field equals string",
			pipeline: `cat vimgym.log | awk '$2 == "POST"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND list_extract(fields, 2) = 'POST'`,
		},

		// Regex match
		{
			name:     "bare regex pattern",
			pipeline: `cat vimgym.log | awk '/timeout/'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
		},
		{
			name:     "field regex match",
			pipeline: `cat vimgym.log | awk '$0 ~ /timeout/'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
		},
		{
			name:     "negated regex",
			pipeline: `cat vimgym.log | awk '$0 !~ /timeout/'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND NOT regexp_matches(raw, 'timeout')`,
		},
		{
			// A bare /re/ as a sub-expression of an && chain. warp parses the
			// bare regex into a RegexLit inside the ExprPattern, so this guards
			// extractAwkExpr's RegexLit recursion (the same node that broke the
			// top-level bare-filter pushdown when warp's pattern AST changed).
			name:     "bare regex AND meta-var",
			pipeline: `cat vimgym.log | awk '/timeout/ && $LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout') AND level = 'ERROR'`,
		},

		// Compound conditions
		{
			name:     "AND two meta-vars",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR" && $INSTANCE == "i-abc123"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND instance = 'i-abc123'`,
		},
		{
			name:     "OR two meta-vars",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR" || $LEVEL == "WARN"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND (level = 'ERROR' OR level = 'WARN')`,
		},
		{
			name:     "mixed AND with field and meta-var",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR" && $3 > 100'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND CAST(list_extract(fields, 3) AS DOUBLE) > 100`,
		},

		// NR (row number) ranges → LIMIT/OFFSET
		{
			name:     "NR range",
			pipeline: `cat vimgym.log | awk 'NR >= 10 && NR <= 50'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? LIMIT 41 OFFSET 9`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

func TestPushdown_SingleStage_Jq(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "select on json field equality",
			pipeline: `cat vimgym.log | jq 'select(.status == 500)'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
		},
		{
			name:     "select on json field string",
			pipeline: `cat vimgym.log | jq 'select(.action == "login")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND json_extract_string(payload, '$.action') = 'login'`,
		},
		{
			name:     "select on meta _app",
			pipeline: `cat warp.log | jq 'select(._namespace == "vimgym")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = 'vimgym'`,
		},
		{
			name:     "select on meta _level",
			pipeline: `cat vimgym.log | jq 'select(._level == "ERROR")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
		},
		{
			name:     "select with numeric comparison",
			pipeline: `cat vimgym.log | jq 'select(.duration_ms > 1000)'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.duration_ms') AS DOUBLE) > 1000`,
		},
		{
			name:     "select not equal",
			pipeline: `cat vimgym.log | jq 'select(.status != 200)'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) != 200`,
		},
		{
			name:     "chained selects inside jq",
			pipeline: `cat vimgym.log | jq 'select(.status == 500) | select(.action == "login")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND json_extract_string(payload, '$.action') = 'login'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

func TestPushdown_SingleStage_Grep(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "literal string",
			pipeline: `cat vimgym.log | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
		},
		{
			name:     "case insensitive",
			pipeline: `cat vimgym.log | grep -i error`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'error', 'i')`,
		},
		{
			name:     "inverted match",
			pipeline: `cat vimgym.log | grep -v DEBUG`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND NOT regexp_matches(raw, 'DEBUG')`,
		},
		{
			name:     "inverted case insensitive",
			pipeline: `cat vimgym.log | grep -iv debug`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND NOT regexp_matches(raw, 'debug', 'i')`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

func TestPushdown_SingleStage_Sed(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "address pattern filter",
			pipeline: `cat vimgym.log | sed -n '/timeout/p'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

func TestPushdown_SingleStage_HeadTail(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "head default",
			pipeline: `head vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts ASC LIMIT 10`,
		},
		{
			name:     "head with count",
			pipeline: `head -50 vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts ASC LIMIT 50`,
		},
		{
			// tail wraps its DESC-LIMIT pushdown in an outer ASC SELECT so
			// rows reach the user in chronological order, matching how a
			// tail of a file on disk reads. Inner subquery still does the
			// cheap per-shard top-N work; the outer flip is free.
			name:     "tail default",
			pipeline: `tail vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts DESC LIMIT 10) AS t ORDER BY ts ASC`,
		},
		{
			name:     "tail with count",
			pipeline: `tail -100 vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts DESC LIMIT 100) AS t ORDER BY ts ASC`,
		},
		{
			name:     "head -n flag",
			pipeline: `head -n 25 vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts ASC LIMIT 25`,
		},
		{
			name:     "tail -n flag",
			pipeline: `tail -n 50 vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM (SELECT raw, ts FROM logs WHERE namespace = ? ORDER BY ts DESC LIMIT 50) AS t ORDER BY ts ASC`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

func TestPushdown_SingleStage_Source(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		{
			name:     "cat bare — all rows for app",
			pipeline: `cat vimgym.log`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ?`,
		},
		{
			name:     "cat warp.log — all apps",
			pipeline: `cat warp.log`,
			wantSQL:  `SELECT raw, ts FROM logs`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tier 2: Two-stage pipelines where BOTH stages merge into pushdown
//
// The planner should fold both stages into the SQL WHERE clause, producing
// no compute stages.
// ---------------------------------------------------------------------------

func TestPushdown_MergedStages(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// awk | grep → AND merge
		{
			name:     "awk filter then grep",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND regexp_matches(raw, 'timeout')`,
		},

		// grep | awk → AND merge
		{
			name:     "grep then awk filter",
			pipeline: `cat vimgym.log | grep timeout | awk '$LEVEL == "ERROR"'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout') AND level = 'ERROR'`,
		},

		// awk | awk → AND merge
		{
			name:     "two awk filters",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"' | awk '$3 > 100'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND CAST(list_extract(fields, 3) AS DOUBLE) > 100`,
		},

		// jq select | jq select → AND merge
		{
			name:     "two jq selects",
			pipeline: `cat vimgym.log | jq 'select(.status == 500)' | jq 'select(.action == "login")'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND json_extract_string(payload, '$.action') = 'login'`,
		},

		// grep | grep → AND merge
		{
			name:     "two greps",
			pipeline: `cat vimgym.log | grep timeout | grep ERROR`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout') AND regexp_matches(raw, 'ERROR')`,
		},

		// grep -v | grep → AND merge (inverted + normal)
		{
			name:     "inverted grep then grep",
			pipeline: `cat vimgym.log | grep -v DEBUG | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND NOT regexp_matches(raw, 'DEBUG') AND regexp_matches(raw, 'timeout')`,
		},

		// awk | grep -i → AND merge with ILIKE
		{
			name:     "awk filter then case-insensitive grep",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep -i timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND regexp_matches(raw, 'timeout', 'i')`,
		},

		// jq select | grep → AND merge
		{
			name:     "jq select then grep",
			pipeline: `cat vimgym.log | jq 'select(.status == 500)' | grep timeout`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND regexp_matches(raw, 'timeout')`,
		},

		// awk filter | head → WHERE + LIMIT
		{
			name:     "awk filter then head",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"' | head -20`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' ORDER BY ts ASC LIMIT 20`,
		},

		// grep | head → WHERE + LIMIT
		{
			name:     "grep then head",
			pipeline: `cat vimgym.log | grep timeout | head -5`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout') ORDER BY ts ASC LIMIT 5`,
		},

		// awk | sed address filter → AND merge
		{
			name:     "awk filter then sed address",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"' | sed -n '/fatal/p'`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND regexp_matches(raw, 'fatal')`,
		},

		// three stages all pushable
		{
			name:     "three pushable stages",
			pipeline: `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep timeout | head -10`,
			wantSQL:  `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR' AND regexp_matches(raw, 'timeout') ORDER BY ts ASC LIMIT 10`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != 0 {
				t.Errorf("expected no compute stages, got %v", plan.ComputeStages)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tier 3: Two-stage pipelines where the second stage CANNOT be pushed down
//
// The planner should push down the first stage into SQL and leave the second
// stage (and everything after it) as compute stages.
// ---------------------------------------------------------------------------

func TestPushdown_ComputeSplit(t *testing.T) {
	tests := []struct {
		name         string
		pipeline     string
		wantSQL      string
		wantCompute  []string // string representation of each compute stage
	}{
		// sort — requires seeing all rows
		{
			name:        "awk filter then sort",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | sort`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{"sort"},
		},

		// uniq -c — aggregation
		{
			name:        "awk filter then uniq -c",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | uniq -c`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{"uniq -c"},
		},

		// wc -l — aggregation
		{
			name:        "awk filter then wc -l",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | wc -l`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{"wc -l"},
		},

		// sed substitution — modifies output, not a filter
		{
			name:        "awk filter then sed substitution",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | sed 's/foo/bar/'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{"sed 's/foo/bar/'"},
		},

		// awk print — output formatting
		{
			name:        "awk filter then awk print",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | awk '{ print $2, $1 }'`,
			wantSQL:     `SELECT concat_ws(' ', list_extract(fields, 2), list_extract(fields, 1)) as raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: nil, // simple $N print is fully pushed down as projection
		},

		// awk print with bare metavar — projects the column directly.
		{
			name:        "awk print bare metavar",
			pipeline:    `cat vimgym.log | awk '{ print $HOST }'`,
			wantSQL:     `SELECT instance as raw, ts FROM logs WHERE namespace = ?`,
			wantCompute: nil,
		},

		// awk print mixing metavar and positional fields — the motivating case.
		{
			name:        "awk print metavar with positional fields",
			pipeline:    `cat vimgym.log | awk '{ print $HOST, $1, $4 }'`,
			wantSQL:     `SELECT concat_ws(' ', instance, list_extract(fields, 1), list_extract(fields, 4)) as raw, ts FROM logs WHERE namespace = ?`,
			wantCompute: nil,
		},

		// awk print concatenating a metavar with a positional field — exercises
		// the recursive ConcatExpr path picking up the new IdentExpr index branch.
		{
			name:        "awk print concat metavar and field",
			pipeline:    `cat vimgym.log | awk '{ print $HOST "/" $1 }'`,
			wantSQL:     `SELECT concat(concat(instance, '/'), list_extract(fields, 1)) as raw, ts FROM logs WHERE namespace = ?`,
			wantCompute: nil,
		},

		// Unknown $IDENT (not a known metavar) bails — the awk stage stays in
		// compute so the user-defined variable semantics survive.
		{
			name:        "awk print unknown metavar stays in compute",
			pipeline:    `cat vimgym.log | awk '{ print $FOO }'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ?`,
			wantCompute: []string{`awk '{ print $FOO }'`},
		},

		// jq object construction — pushes both as the WHERE (awk filter) and
		// as a json_object SELECT (jq construction folded into projection).
		{
			name:        "awk filter then jq construction",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | jq '{ts: ._ts, msg: .message}'`,
			wantSQL:     `SELECT CAST(json_object('ts', ts, 'msg', json_extract(payload, '$.message')) AS VARCHAR) as raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: nil,
		},

		// awk END block — needs all rows before producing output
		{
			name:        "awk with END block",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { count++ } END { print count }'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{`awk '$LEVEL == "ERROR" { count++ } END { print count }'`},
		},

		// jq select then sort — select pushes down, sort doesn't
		{
			name:        "jq select then sort",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500)' | sort`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{"sort"},
		},

		// grep then awk END — grep pushes down, awk END doesn't
		{
			name:        "grep then awk END block",
			pipeline:    `cat vimgym.log | grep timeout | awk '{ count++ } END { print count }'`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
			wantCompute: []string{`awk '{ count++ } END { print count }'`},
		},

		// sort | uniq -c — both are compute, sort comes first
		{
			name:        "awk filter then sort then uniq -c",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | sort | uniq -c`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{"sort", "uniq -c"},
		},

		// Multiple compute stages after pushdown
		{
			name:        "grep then sort then head",
			pipeline:    `cat vimgym.log | grep timeout | sort | head -20`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND regexp_matches(raw, 'timeout')`,
			wantCompute: []string{"sort", "head -20"},
		},

		// Awk with filter AND print action — both predicate and projection
		// fold; the metavar in the print body resolves to its column.
		{
			name:        "awk filter + metavar projection fully folds",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { print $3, $INSTANCE }'`,
			wantSQL:     `SELECT concat_ws(' ', list_extract(fields, 3), instance) as raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: nil,
		},

		// Pushable stage after a non-pushable stage stays in compute
		{
			name:        "sort between two pushable stages",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | sort | grep timeout`,
			wantSQL:     `SELECT raw, ts FROM logs WHERE namespace = ? AND level = 'ERROR'`,
			wantCompute: []string{"sort", "grep timeout"},
		},

		// jq pipe from select to construction — select pushes as predicate,
		// construction folds into a json_object SELECT, no compute remainder.
		{
			name:        "jq select piped to object construction",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | {ts: ._ts, msg: .message}'`,
			wantSQL:     `SELECT CAST(json_object('ts', ts, 'msg', json_extract(payload, '$.message')) AS VARCHAR) as raw, ts FROM logs WHERE namespace = ? AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: nil,
		},
	}



	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
			if len(plan.ComputeStages) != len(tt.wantCompute) {
				t.Fatalf("ComputeStages count: got %d, want %d\n got: %v\nwant: %v",
					len(plan.ComputeStages), len(tt.wantCompute), plan.ComputeStages, tt.wantCompute)
			}
			for i, want := range tt.wantCompute {
				if plan.ComputeStages[i] != want {
					t.Errorf("ComputeStages[%d]\n got: %s\nwant: %s", i, plan.ComputeStages[i], want)
				}
			}
		})
	}
}
