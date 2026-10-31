package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// shape renders a stage's nodes compactly: Filter= is exact, Filter~ a
// prefilter, and an opaque node or prefilter carries its reason code.
func shape(nodes []node) (string, []reasonCode) {
	var parts []string
	var whys []reasonCode
	for _, n := range nodes {
		switch n := n.(type) {
		case *filterNode:
			if n.exact {
				parts = append(parts, "Filter=")
			} else {
				parts = append(parts, "Filter~")
				whys = append(whys, n.why.code)
			}
		case *opaqueNode:
			parts = append(parts, "Opaque")
			whys = append(whys, n.why.code)
		default:
			parts = append(parts, nodeKind(n))
		}
	}
	return strings.Join(parts, " "), whys
}

func lowerText(t *testing.T, stage string) *loweredStage {
	t.Helper()
	script, err := bash.Parse(stage)
	if err != nil {
		t.Fatal(err)
	}
	return lowerStage(parseStage(script.Commands[0].First.Commands[0].(*bash.SimpleCommand)), 1, false)
}

// TestFrontends pins what each frontend lowers a stage to, and the
// typed reason when it's opaque or only a prefilter.
func TestFrontends(t *testing.T) {
	tests := []struct {
		stage string
		want  string
		whys  []reasonCode
	}{
		{`grep -i timeout`, "Filter=", nil},
		{`grep -A 3 timeout`, "Opaque", []reasonCode{reasonFlag}},
		{`sed -n '/x/p'`, "Filter=", nil},
		{`sed '/x/d'`, "Filter=", nil},
		{`sed '/x/!d'`, "Filter=", nil},
		{`sed -n '/x/!p'`, "Filter=", nil},
		{`sed -n '/x/d'`, "Opaque", []reasonCode{reasonSedProgram}},
		{`sed 's/a/b/'`, "Opaque", []reasonCode{reasonSedProgram}},
		{`head -n 5`, "Limit", nil},
		{`sort -rn`, "Sort", nil},
		{`uniq -c`, "Uniq", nil},
		{`uniq -d`, "Opaque", []reasonCode{reasonFlag}},
		{`wc -l`, "Count", nil},
		{`wc -w`, "Opaque", []reasonCode{reasonNoFrontend}},
		{`cut -d' ' -f2`, "Project", nil},
		{`cut -f 1,3-`, "Project", nil},
		{`cut -c 1-12`, "Opaque", []reasonCode{reasonCutArgs}},
		{`cut -d, -f -3`, "Opaque", []reasonCode{reasonCutFields}},
		{`cut -d '' -f 1`, "Opaque", []reasonCode{reasonCutDelimiter}},

		{`awk 'LEVEL == "ERROR"'`, "Filter=", nil},
		{`awk 'NR >= 2 && NR <= 5'`, "Limit", nil},
		{`awk '{ print LEVEL }'`, "Project", nil},
		{`awk '{ print $2 }'`, "Project", nil},
		{`awk '{ print $NF }'`, "Project", nil},
		{`awk '{ print length }'`, "Project", nil},
		{`awk '{ print $(NF-1) }'`, "Opaque", []reasonCode{reasonAwkComputedField}},
		{`awk '{ print $i }'`, "Opaque", []reasonCode{reasonAwkComputedField}},
		{`awk '{ x++ } END { print x }'`, "Opaque", []reasonCode{reasonAwkUnpatterned}},
		{`awk '$4 == "ERROR"'`, "Filter=", nil},
		{`awk '$2 ~ /time/'`, "Filter=", nil},
		{`awk '$NF == "x"'`, "Filter=", nil},
		{`awk 'length > 80'`, "Filter=", nil},
		{`awk 'length($3) == 0'`, "Filter=", nil},
		{`awk '$(NF-1) == "x"'`, "Opaque", []reasonCode{reasonAwkComputedField}},
		{`awk '$3 > 10'`, "Filter~ Opaque", []reasonCode{reasonAwkStrnum, reasonAwkStrnum}},
		{`awk 'LEVEL == "500"'`, "Filter~ Opaque", []reasonCode{reasonAwkStrnum, reasonAwkStrnum}},
		{`awk 'TS > "2026"'`, "Opaque", []reasonCode{reasonAwkTS}},
		{`awk 'BEGIN { FS = "," } LEVEL == "x"'`, "Opaque", []reasonCode{reasonAwkBegin}},
		{`awk 'NR > 1 && LEVEL == "x"'`, "Opaque", []reasonCode{reasonAwkRecordCounter}},
		{`awk -F, 'LEVEL == "x"'`, "Opaque", []reasonCode{reasonFlag}},
		// The predicate is partial for one reason, the stage runs for another.
		{`awk '$(NF-1) > 10 && LEVEL == "x" { printf "%s\n", $1 }'`, "Filter~ Opaque", []reasonCode{reasonAwkComputedField, reasonAwkAction}},
		// The action is a print SQL computes: only the pattern holds the stage back.
		{`awk '$NF > 10 && LEVEL == "x" { print $1 }'`, "Filter~ Opaque", []reasonCode{reasonAwkStrnum, reasonAwkStrnum}},
		{`awk 'LEVEL == "a" { print "A" } LEVEL == "b" { print "B" }'`, "Filter~ Opaque", []reasonCode{reasonAwkRules, reasonAwkRules}},

		{`jq 'select(.status == 500)'`, "Filter=", nil},
		{`jq -r 'select(.status == 500)'`, "Filter=", nil},
		{`jq 'select(.status == 500) | .action'`, "Filter= Project", nil},
		{`jq -r 'select(.status == 500) | .action'`, "Filter= Project", nil},
		{`jq 'select(.a | test("x")) | .b'`, "Opaque", []reasonCode{reasonJqSelect}},
		{`jq 'select(.status == 500) | keys'`, "Filter~ Opaque", []reasonCode{reasonJqTail, reasonJqTail}},
		{`jq -s 'length'`, "Opaque", []reasonCode{reasonJqSlurp}},
		{`jq --arg x y '.a'`, "Opaque", []reasonCode{reasonFlag}},
	}
	for _, tt := range tests {
		t.Run(tt.stage, func(t *testing.T) {
			got, whys := shape(lowerText(t, tt.stage).nodes)
			if got != tt.want {
				t.Errorf("nodes = %s, want %s", got, tt.want)
			}
			if len(whys) != len(tt.whys) {
				t.Fatalf("reasons = %v, want %v", whys, tt.whys)
			}
			for i := range whys {
				if whys[i] != tt.whys[i] {
					t.Errorf("reason %d = %q, want %q", i, reason{code: whys[i]}, reason{code: tt.whys[i]})
				}
			}
		})
	}
}

// TestFrontendReadsMeta pins which stages need the meta-variable
// columns, decided from the syntax tree.
func TestFrontendReadsMeta(t *testing.T) {
	tests := []struct {
		stage string
		want  bool
	}{
		{`awk '{ print HOST }'`, true},
		{`awk -F, '{ print HOST }'`, true},
		{`awk '{ print "HOST" }'`, false},
		{`awk '{ print MYHOST }'`, false},
		{`jq -r '$host'`, true},
		{`jq 'select($pod == "a")'`, true},
		{`jq '. as $level | $level'`, true}, // a rebinding is read conservatively
		{`jq '.host'`, false},
		{`jq '.a.host_name'`, false},
		{`jq '"$host"'`, false},
		{`jq '$__loc__'`, false},
		{`grep HOST`, false},
	}
	for _, tt := range tests {
		if got := lowerText(t, tt.stage).readsMeta; got != tt.want {
			t.Errorf("%s: readsMeta = %v, want %v", tt.stage, got, tt.want)
		}
	}
}

// TestPasses pins where filter pushdown stops and what the pass log
// says about it.
func TestPasses(t *testing.T) {
	tests := []struct {
		pipeline string
		shard    int    // nodes the shard runs
		log      string // a pass log line that must appear
	}{
		{`cat vimgym.log | grep a | grep b`, 2, "Filter[2: grep] into shard SQL (exact)"},
		{`tail -n 5 vimgym.log | grep a`, 1, "Filter[1: grep] held above Limit[source: tail]: a line count ends predicate pushdown"},
		{`cat vimgym.log | head -3 | head -2`, 1, "Limit[2: head] held above Limit[1: head]"},
		{`cat vimgym.log | uniq | grep a`, 0, "stopped at Uniq[1: uniq]"},
		{`cat vimgym.log | grep a | sort | uniq -c | sort -rn`, 4, "Sort[2: sort] + Uniq[3: uniq] + Sort[4: sort] became the shard's GROUP BY (count_line)"},
		{`cat vimgym.log | sort -ru`, 1, "Sort[1: sort] became the shard's GROUP BY (line)"},
		{`cat vimgym.log | awk '{ print $2 }' | wc -l`, 2, "Count[2: wc] became the shard's GROUP BY (total)"},
		{`tail -n 5 vimgym.log | wc -l`, 1, "skipped Count[1: wc]: a pushed line count is applied after the shards"},
		{`cat vimgym.log | sort | uniq -c | awk '{ print LEVEL }'`, 0, "skipped Sort[1: sort] + Uniq[2: uniq]: Project[3: awk] names a meta-variable"},
		{`cat vimgym.log | grep -E x | jq 'select(.a | test("b"))' | wc -l`, 1, "skipped Count[3: wc]: row stages run in compute"},
		{`cat vimgym.log | sort -n | uniq -c`, 0, "skipped Sort[1: sort]: a flag changes its compare key"},
		{`cat vimgym.log | jq 'select(.a == 1) | keys' | grep b`, 1, "Filter[1: jq] into shard SQL (prefilter; its stage still runs)"},
		{`cat vimgym.log | grep a | awk '{ print LEVEL }'`, 2, "Project[2: awk] became the SELECT"},
		{`cat vimgym.log | jq '.a' | awk 'LEVEL == "x"'`, 1, "Project[1: jq] stays in compute: Filter[2: awk] names a meta-variable"},
		{`tail -n 5 vimgym.log | grep a | sort`, 1, "skipped Sort[2: sort]: it sorts what a pushed line count kept"},
		{`cat vimgym.log | grep a | sort -r`, 1, "Sort[2: sort] became the shard's ORDER BY raw DESC"},
		{`cat vimgym.log | sort -n`, 0, "skipped Sort[1: sort]: a flag changes its compare key"},
		{`cat vimgym.log | uniq | sort`, 0, "skipped Sort[2: sort]: it isn't next to the shard"},
		{`cat vimgym.log | sort | grep a | sort -r | sed -n '/b/p'`, 2, "Filter[4: sed] moved below Sort[1: sort]: the shard filters each line"},
		{`cat vimgym.log | sort | grep a | uniq -c`, 3, "Filter[2: grep] moved below Sort[1: sort]"},
		{`cat vimgym.log | sort -n | grep a`, 0, "skipped Sort[1: sort]: a flag changes its compare key"},
		{`cat vimgym.log | sort access.log | uniq -c`, 0, "skipped Sort[1: sort]: a flag changes its compare key"},
		{`tail -n 5 vimgym.log | sort | grep a`, 1, "skipped Filter[2: grep]: a pushed line count picks its rows before the filter"},
		{`cat vimgym.log | sort | jq 'select(.a == 1)'`, 0, "stopped at Sort[1: sort]"},
		{`cat vimgym.log | jq -r .a | grep x | sort`, 3, "Filter[2: grep] moved below Project[1: jq]: the shard filters each line"},
		{`cat vimgym.log | jq -r .a | grep x | awk '{ print LEVEL }'`, 2, "skipped Filter[2: grep]: Project[3: awk] names a meta-variable"},
		{`cat vimgym.log | grep x | awk '{ print $2 }' | grep y | sort | uniq -c`, 5, "Filter[3: grep] moved below Project[2: awk]"},
	}
	for _, tt := range tests {
		t.Run(tt.pipeline, func(t *testing.T) {
			_, c, err := compile(tt.pipeline, 0, "", time.Time{}, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.shard != tt.shard {
				t.Errorf("shard runs %d nodes, want %d", c.shard, tt.shard)
			}
			var log []string
			for _, p := range c.passes {
				log = append(log, p.pass+": "+p.msg)
			}
			if !strings.Contains(strings.Join(log, "\n"), tt.log) {
				t.Errorf("pass log missing %q:\n%s", tt.log, strings.Join(log, "\n"))
			}
		})
	}
}

// TestExplainShowsIR golden-tests Explain's IR and pass sections.
func TestExplainShowsIR(t *testing.T) {
	got := Explain(`cat /log/acme/api/main.log | awk '$(NF-1) > 10 && LEVEL == "ERROR" { print $1 $2 }' | head -5`)
	want := `Pipeline: cat /log/acme/api/main.log | awk '$(NF-1) > 10 && LEVEL == "ERROR" { print $1 $2 }' | head -5

Plan:
  Scan     namespace=acme service=api kind=main
  Filter   prefilter  level = 'ERROR'                              [1: awk]
           not exact: reads a field by a computed index ($i, $(n+1))
  ─── shard │ compute ───
  Opaque   awk '$(NF-1) > 10 && LEVEL == "ERROR" { print $1 $2 }'  [1: awk]
           reads a field by a computed index ($i, $(n+1))
  Limit    5  ts ASC                                               [2: head]

Passes:
  filter-pushdown      Filter[1: awk] into shard SQL (prefilter; its stage still runs)
  filter-pushdown      stopped at Opaque[1: awk]: it runs in compute, and so does every node above it
  field-projection     skipped Filter[1: awk]: it reads $0 or a field position it can't name ahead of time
  sort-limit-fusion    no sort in compute, so the shard orders by ts
`
	if i := strings.Index(got, "\nShard SQL:"); i >= 0 {
		got = got[:i]
	}
	if got != want {
		t.Errorf("Explain:\n%s\nwant:\n%s", got, want)
	}
	full := Explain(`cat /log/acme/api/main.log | awk '$NF > 10 && LEVEL == "ERROR" { print $1 $2 }' | head -5`)
	for _, line := range []string{
		`1. awk '$NF > 10 && LEVEL == "ERROR" { print $1 $2 }'  [stage 1, verbatim]`,
		`2. head -5  [stage 2, verbatim]`,
	} {
		if !strings.Contains(full, line) {
			t.Errorf("compute stages missing %q:\n%s", line, full)
		}
	}
}
