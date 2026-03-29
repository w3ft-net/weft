package planner

import "testing"

// ===========================================================================
// Edge cases and failure modes for predicate pushdown.
//
// Organized by subsystem, then by "should succeed" vs "should fail gracefully"
// vs "should refuse to push down".
// ===========================================================================

// ---------------------------------------------------------------------------
// Source parsing edge cases
// ---------------------------------------------------------------------------

func TestEdge_Source(t *testing.T) {
	t.Run("error cases", func(t *testing.T) {
		errorCases := []struct {
			name     string
			pipeline string
		}{
			{"empty string", ``},
			{"unknown source command", `ls vimgym.log`},
			{"cat with no file", `cat`},
			{"head with no file", `head`},
			{"tail with no file", `tail`},
		}
		for _, tt := range errorCases {
			t.Run(tt.name, func(t *testing.T) {
				_, err := Plan(tt.pipeline)
				if err == nil {
					t.Errorf("Plan(%q): expected error, got nil", tt.pipeline)
				}
			})
		}
	})

	t.Run("different app names", func(t *testing.T) {
		tests := []struct {
			name     string
			pipeline string
			wantSQL  string
		}{
			{
				name:     "callbox app",
				pipeline: `cat callbox.log`,
				wantSQL:  `SELECT * FROM logs WHERE app = 'callbox'`,
			},
			{
				name:     "hyphenated app name",
				pipeline: `cat my-service.log`,
				wantSQL:  `SELECT * FROM logs WHERE app = 'my-service'`,
			},
			{
				name:     "underscored app name",
				pipeline: `cat my_service.log`,
				wantSQL:  `SELECT * FROM logs WHERE app = 'my_service'`,
			},
			{
				name:     "head on warp.log",
				pipeline: `head warp.log`,
				wantSQL:  `SELECT * FROM logs ORDER BY ts ASC LIMIT 10`,
			},
			{
				name:     "tail on warp.log",
				pipeline: `tail -50 warp.log`,
				wantSQL:  `SELECT * FROM logs ORDER BY ts DESC LIMIT 50`,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				plan, err := Plan(tt.pipeline)
				if err != nil {
					t.Fatalf("Plan(%q): %v", tt.pipeline, err)
				}
				if plan.ShardSQL != tt.wantSQL {
					t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Awk edge cases
// ---------------------------------------------------------------------------

func TestEdge_Awk_NonPushable(t *testing.T) {
	// These awk programs should NOT push down at all — the entire stage
	// becomes compute.
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// BEGIN block: no pushable filter
		{
			name:        "BEGIN block only",
			pipeline:    `cat vimgym.log | awk 'BEGIN { print "header" }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk 'BEGIN { print "header" }'`},
		},
		// No pattern at all, just an action on every line
		{
			name:        "unconditional print action",
			pipeline:    `cat vimgym.log | awk '{ print $2 }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk '{ print $2 }'`},
		},
		// User-defined function
		{
			name:        "program with function definition",
			pipeline:    `cat vimgym.log | awk 'function foo() { return 1 } /test/'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk 'function foo() { return 1 } /test/'`},
		},
		// Assignment in pattern position — not a filter
		{
			name:        "assignment expression is not a filter",
			pipeline:    `cat vimgym.log | awk '{ x = $3 + 1; print x }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk '{ x = $3 + 1; print x }'`},
		},
		// Non-pushable field access: $NF (variable index, not a literal)
		{
			name:        "variable field index NF",
			pipeline:    `cat vimgym.log | awk '$NF == "ERROR"'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk '$NF == "ERROR"'`},
		},
		// Field compared to another field — can't push down
		{
			name:        "field vs field comparison",
			pipeline:    `cat vimgym.log | awk '$3 > $4'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk '$3 > $4'`},
		},
		// Function call in pattern — can't push down
		{
			name:        "function call in pattern",
			pipeline:    `cat vimgym.log | awk 'length($0) > 100'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk 'length($0) > 100'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

func TestEdge_Awk_Pushable(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// $0 references (whole record)
		{
			name:     "field zero equals string",
			pipeline: `cat vimgym.log | awk '$0 == "exact match"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND raw = 'exact match'`,
		},
		// Deeply nested boolean logic
		{
			name:     "nested AND inside OR",
			pipeline: `cat vimgym.log | awk '($LEVEL == "ERROR" && $3 > 100) || $LEVEL == "FATAL"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND ((level = 'ERROR' AND CAST(list_extract(fields, 3) AS DOUBLE) > 100) OR level = 'FATAL')`,
		},
		// Large positional field number
		{
			name:     "high positional field",
			pipeline: `cat vimgym.log | awk '$15 == "value"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND list_extract(fields, 15) = 'value'`,
		},
		// Field 1
		{
			name:     "field 1",
			pipeline: `cat vimgym.log | awk '$1 == "2026-03-28"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND list_extract(fields, 1) = '2026-03-28'`,
		},
		// Numeric equality (not just comparison)
		{
			name:     "field numeric equality",
			pipeline: `cat vimgym.log | awk '$3 == 200'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(list_extract(fields, 3) AS DOUBLE) = 200`,
		},
		// Numeric not-equal
		{
			name:     "field numeric not equal",
			pipeline: `cat vimgym.log | awk '$3 != 0'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(list_extract(fields, 3) AS DOUBLE) != 0`,
		},
		// Meta-var less-than (non-standard but expressible)
		{
			name:     "meta-var less than string",
			pipeline: `cat vimgym.log | awk '$LEVEL < "WARN"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND level < 'WARN'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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
// Awk partial pushdown edge cases
// ---------------------------------------------------------------------------

func TestEdge_Awk_PartialPushdown(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// Pattern + printf (not print)
		{
			name:        "filter with printf action",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { printf "%s %s\n", $1, $2 }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR'`,
			wantCompute: []string{`awk '{ printf "%s %s\n", $1, $2 }'`},
		},
		// Regex pattern + non-default action
		{
			name:        "regex pattern with print action",
			pipeline:    `cat vimgym.log | awk '/timeout/ { print $1, $2 }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%timeout%'`,
			wantCompute: []string{`awk '{ print $1, $2 }'`},
		},
		// Filter + increment + END (count errors)
		{
			name:        "count pattern with END",
			pipeline:    `cat vimgym.log | awk '/error/ { c++ } END { print c }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%error%'`,
			wantCompute: []string{`awk '/error/ { c++ } END { print c }'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

// ---------------------------------------------------------------------------
// Jq edge cases
// ---------------------------------------------------------------------------

func TestEdge_Jq_NonPushable(t *testing.T) {
	// jq expressions that cannot be pushed down at all.
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// Identity expression
		{
			name:        "identity",
			pipeline:    `cat vimgym.log | jq '.'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq '.'`},
		},
		// Field access without select
		{
			name:        "bare field access",
			pipeline:    `cat vimgym.log | jq '.payload'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq '.payload'`},
		},
		// Object construction without select
		{
			name:        "object construction",
			pipeline:    `cat vimgym.log | jq '{user: .user_id, action: .action}'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq '{user: .user_id, action: .action}'`},
		},
		// Array construction
		{
			name:        "array expression",
			pipeline:    `cat vimgym.log | jq '[.user_id, .action]'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq '[.user_id, .action]'`},
		},
		// keys/length/type
		{
			name:        "keys builtin",
			pipeline:    `cat vimgym.log | jq 'keys'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'keys'`},
		},
		{
			name:        "length builtin",
			pipeline:    `cat vimgym.log | jq 'length'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'length'`},
		},
		// map
		{
			name:        "map expression",
			pipeline:    `cat vimgym.log | jq 'map(.status)'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'map(.status)'`},
		},
		// select with non-comparison condition (has, contains, test)
		{
			name:        "select with has",
			pipeline:    `cat vimgym.log | jq 'select(has("error"))'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'select(has("error"))'`},
		},
		// select with arithmetic in condition
		{
			name:        "select with arithmetic",
			pipeline:    `cat vimgym.log | jq 'select((.x + .y) > 100)'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'select((.x + .y) > 100)'`},
		},
		// select comparing two fields (no literal)
		{
			name:        "select comparing two fields",
			pipeline:    `cat vimgym.log | jq 'select(.start < .end)'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'select(.start < .end)'`},
		},
		// Literal on the left side of comparison
		{
			name:        "literal on left side",
			pipeline:    `cat vimgym.log | jq 'select(500 == .status)'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`jq 'select(500 == .status)'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

func TestEdge_Jq_Pushable(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// Boolean field comparison
		{
			name:     "select boolean true",
			pipeline: `cat vimgym.log | jq 'select(.success == true)'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.success') AS BOOL) = true`,
		},
		// Less-than on json field
		{
			name:     "select less than",
			pipeline: `cat vimgym.log | jq 'select(.duration_ms < 100)'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.duration_ms') AS DOUBLE) < 100`,
		},
		// Greater-than-or-equal
		{
			name:     "select gte",
			pipeline: `cat vimgym.log | jq 'select(.retries >= 3)'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.retries') AS DOUBLE) >= 3`,
		},
		// Three chained selects
		{
			name:     "three chained selects",
			pipeline: `cat vimgym.log | jq 'select(._level == "ERROR") | select(.status == 500) | select(.action == "login")'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR' AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND json_extract_string(payload, '$.action') = 'login'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

func TestEdge_Jq_PartialPushdown(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// select | field access
		{
			name:        "select then field access",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .payload'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{`jq '.payload'`},
		},
		// select | select | object construction (two pushable, one not)
		{
			name:        "two selects then construction",
			pipeline:    `cat vimgym.log | jq 'select(._level == "ERROR") | select(.status == 500) | {ts: ._ts}'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{`jq '{ts: ._ts}'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

// ---------------------------------------------------------------------------
// Grep edge cases
// ---------------------------------------------------------------------------

func TestEdge_Grep(t *testing.T) {
	t.Run("non-pushable flags", func(t *testing.T) {
		// Flags that we don't know how to push down — entire stage goes to compute.
		tests := []struct {
			name        string
			pipeline    string
			wantSQL     string
			wantCompute []string
		}{
			{
				name:        "grep -c count mode",
				pipeline:    `cat vimgym.log | grep -c ERROR`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`grep -c ERROR`},
			},
			{
				name:        "grep -n line numbers",
				pipeline:    `cat vimgym.log | grep -n ERROR`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`grep -n ERROR`},
			},
			{
				name:        "grep -E extended regex",
				pipeline:    `cat vimgym.log | grep -E 'error|warn'`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`grep -E error|warn`},
			},
			{
				name:        "grep -w word match",
				pipeline:    `cat vimgym.log | grep -w error`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`grep -w error`},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				plan, err := Plan(tt.pipeline)
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
	})
}

// ---------------------------------------------------------------------------
// Sed edge cases
// ---------------------------------------------------------------------------

func TestEdge_Sed(t *testing.T) {
	t.Run("non-pushable", func(t *testing.T) {
		tests := []struct {
			name        string
			pipeline    string
			wantSQL     string
			wantCompute []string
		}{
			// Substitution without -n is not a filter
			{
				name:        "substitution without -n",
				pipeline:    `cat vimgym.log | sed 's/error/ERROR/g'`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`sed 's/error/ERROR/g'`},
			},
			// -n with delete instead of print
			{
				name:        "-n with delete command",
				pipeline:    `cat vimgym.log | sed -n '/pattern/d'`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`sed -n '/pattern/d'`},
			},
			// No -n flag with address print (prints everything plus matching lines twice)
			{
				name:        "address print without -n",
				pipeline:    `cat vimgym.log | sed '/timeout/p'`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`sed '/timeout/p'`},
			},
			// Range address
			{
				name:        "range address",
				pipeline:    `cat vimgym.log | sed -n '/start/,/end/p'`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`sed -n '/start/,/end/p'`},
			},
			// Multiple commands
			{
				name:        "multiple commands with semicolon",
				pipeline:    `cat vimgym.log | sed -n '/error/p; /warn/p'`,
				wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
				wantCompute: []string{`sed -n '/error/p; /warn/p'`},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				plan, err := Plan(tt.pipeline)
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
	})
}

// ---------------------------------------------------------------------------
// SQL special character escaping
// ---------------------------------------------------------------------------

func TestEdge_SQLEscaping(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		wantSQL  string
	}{
		// Single quote in string literal
		{
			name:     "awk string with single quote",
			pipeline: `cat vimgym.log | awk '$LEVEL == "can'"'"'t"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'can''t'`,
		},
		// LIKE special chars in grep pattern
		{
			name:     "grep pattern with percent",
			pipeline: `cat vimgym.log | grep 100%`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%100\%%'`,
		},
		{
			name:     "grep pattern with underscore",
			pipeline: `cat vimgym.log | grep user_id`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%user\_id%'`,
		},
		// LIKE special chars in awk regex
		{
			name:     "awk regex with underscore",
			pipeline: `cat vimgym.log | awk '/user_name/'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%user\_name%'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
			if err != nil {
				t.Fatalf("Plan(%q): %v", tt.pipeline, err)
			}
			if plan.ShardSQL != tt.wantSQL {
				t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, tt.wantSQL)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Pipeline ordering and interaction edge cases
// ---------------------------------------------------------------------------

func TestEdge_PipelineOrdering(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// head in the middle after a filter — should merge into pushdown
		{
			name:        "filter then head in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | head -5`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%ERROR%' ORDER BY ts ASC LIMIT 5`,
			wantCompute: nil,
		},
		// tail in the middle — should merge
		{
			name:        "filter then tail in pipeline",
			pipeline:    `cat vimgym.log | grep ERROR | tail -5`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%ERROR%' ORDER BY ts DESC LIMIT 5`,
			wantCompute: nil,
		},
		// head as source then filter
		{
			name:        "head source then awk filter",
			pipeline:    `head -50 vimgym.log | awk '$LEVEL == "ERROR"'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR' ORDER BY ts ASC LIMIT 50`,
			wantCompute: nil,
		},
		// All compute — no pushable stages at all
		{
			name:        "cat then sort then uniq",
			pipeline:    `cat vimgym.log | sort | uniq -c`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{"sort", "uniq -c"},
		},
		// Five-stage mixed pipeline
		{
			name:        "five stage mixed",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep timeout | sort | uniq -c | head -10`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR' AND raw LIKE '%timeout%'`,
			wantCompute: []string{"sort", "uniq -c", "head -10"},
		},
		// Partial awk pushdown followed by more compute
		{
			name:        "partial awk then sort",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR" { print $3 }' | sort`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR'`,
			wantCompute: []string{`awk '{ print $3 }'`, "sort"},
		},
		// Partial jq pushdown followed by more compute
		{
			name:        "partial jq then sort",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500) | .payload' | sort`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{`jq '.payload'`, "sort"},
		},
		// wc without -l flag — not pushable
		{
			name:        "wc without flags",
			pipeline:    `cat vimgym.log | grep ERROR | wc`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%ERROR%'`,
			wantCompute: []string{"wc"},
		},
		// sort with flags — still compute
		{
			name:        "sort with flags",
			pipeline:    `cat vimgym.log | grep ERROR | sort -rn`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%ERROR%'`,
			wantCompute: []string{"sort -rn"},
		},
		// uniq without -c — still compute
		{
			name:        "uniq without flags",
			pipeline:    `cat vimgym.log | grep ERROR | sort | uniq`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%ERROR%'`,
			wantCompute: []string{"sort", "uniq"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

// ---------------------------------------------------------------------------
// Cross-tool interaction: mixing tools in a single pipeline
// ---------------------------------------------------------------------------

func TestEdge_CrossTool(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// jq select pushes, then awk print is compute
		{
			name:        "jq select then awk print",
			pipeline:    `cat vimgym.log | jq 'select(.status == 500)' | awk '{ print $1 }'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{`awk '{ print $1 }'`},
		},
		// grep then jq select then sort
		{
			name:        "grep then jq select then sort",
			pipeline:    `cat vimgym.log | grep error | jq 'select(.status == 500)' | sort`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%error%' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: []string{"sort"},
		},
		// awk filter then jq select — both push
		{
			name:        "awk filter then jq select",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | jq 'select(.status == 500)'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR' AND CAST(json_extract(payload, '$.status') AS INT) = 500`,
			wantCompute: nil,
		},
		// sed filter then grep — both push
		{
			name:        "sed filter then grep",
			pipeline:    `cat vimgym.log | sed -n '/timeout/p' | grep ERROR`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND raw LIKE '%timeout%' AND raw LIKE '%ERROR%'`,
			wantCompute: nil,
		},
		// Four different tools all pushing
		{
			name:        "four tools all pushable",
			pipeline:    `cat vimgym.log | awk '$LEVEL == "ERROR"' | grep timeout | jq 'select(.status == 500)' | sed -n '/fatal/p'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR' AND raw LIKE '%timeout%' AND CAST(json_extract(payload, '$.status') AS INT) = 500 AND raw LIKE '%fatal%'`,
			wantCompute: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

// ---------------------------------------------------------------------------
// NR edge cases
// ---------------------------------------------------------------------------

func TestEdge_NR(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		// Single NR bound (not a range) — should NOT push as LIMIT/OFFSET,
		// because NR >= 10 alone means "skip first 9" which needs the whole
		// stream. We only handle closed ranges NR >= X && NR <= Y.
		{
			name:        "NR lower bound only",
			pipeline:    `cat vimgym.log | awk 'NR >= 10'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk 'NR >= 10'`},
		},
		{
			name:        "NR upper bound only",
			pipeline:    `cat vimgym.log | awk 'NR <= 50'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk 'NR <= 50'`},
		},
		// NR range starting at 1 — equivalent to head
		{
			name:     "NR range from 1",
			pipeline: `cat vimgym.log | awk 'NR >= 1 && NR <= 20'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' LIMIT 20`,
		},
		// NR mixed with regular predicate — can't combine
		{
			name:        "NR mixed with filter",
			pipeline:    `cat vimgym.log | awk 'NR >= 10 && $LEVEL == "ERROR"'`,
			wantSQL:     `SELECT * FROM logs WHERE app = 'vimgym'`,
			wantCompute: []string{`awk 'NR >= 10 && $LEVEL == "ERROR"'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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

// ---------------------------------------------------------------------------
// Warp.log (all-apps) with filters
// ---------------------------------------------------------------------------

func TestEdge_WarpLog(t *testing.T) {
	tests := []struct {
		name        string
		pipeline    string
		wantSQL     string
		wantCompute []string
	}{
		{
			name:     "warp.log with awk app filter",
			pipeline: `cat warp.log | awk '$APP == "vimgym" && $LEVEL == "ERROR"'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'vimgym' AND level = 'ERROR'`,
		},
		{
			name:     "warp.log with grep",
			pipeline: `cat warp.log | grep timeout`,
			wantSQL:  `SELECT * FROM logs WHERE raw LIKE '%timeout%'`,
		},
		{
			name:     "warp.log with jq meta app filter",
			pipeline: `cat warp.log | jq 'select(._app == "callbox")'`,
			wantSQL:  `SELECT * FROM logs WHERE app = 'callbox'`,
		},
		{
			name:        "warp.log head with sort",
			pipeline:    `head warp.log | sort`,
			wantSQL:     `SELECT * FROM logs ORDER BY ts ASC LIMIT 10`,
			wantCompute: []string{"sort"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Plan(tt.pipeline)
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
