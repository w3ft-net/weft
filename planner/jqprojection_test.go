package planner

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/jq"
)

// TestJqRootKeys: a program is sent an object of just its keys only when
// nothing it does can see the rest of the object, and a program that can is
// refused for a stated reason.
func TestJqRootKeys(t *testing.T) {
	for _, tt := range []struct {
		program string
		want    []string
		refused string // a fragment of the reason, when refused
	}{
		// Keys, and what is done to their values.
		{program: `.a`, want: []string{"a"}},
		{program: `.a.b.c`, want: []string{"a"}},
		{program: `.["a"]`, want: []string{"a"}},
		{program: `.items[]`, want: []string{"items"}},
		{program: `.foo | length`, want: []string{"foo"}},
		{program: `.tags | tostring`, want: []string{"tags"}},
		{program: `.items | map(. * 2)`, want: []string{"items"}},
		{program: `.a?`, want: []string{"a"}},
		{program: `.a // .b`, want: []string{"a", "b"}},
		{program: `{s: .status, a: .action}`, want: []string{"action", "status"}},
		{program: `{status, action}`, want: []string{"action", "status"}},
		{program: `[.x, .y] | add`, want: []string{"x", "y"}},
		{program: `.x, .y`, want: []string{"x", "y"}},
		{program: `"\(.a) \(.b)"`, want: []string{"a", "b"}},
		{program: `(.a // 0) + (.b // 0)`, want: []string{"a", "b"}},
		{program: `.a == null`, want: []string{"a"}},
		{program: `if .s == 1 then .a else .b end`, want: []string{"a", "b", "s"}},
		{program: `if .s == 500 then .a else empty end`, want: []string{"a", "s"}},
		{program: `select(.status == 500) | .action`, want: []string{"action", "status"}},
		{program: `select(.a | test("x")) | .b`, want: []string{"a", "b"}},
		{program: `select(.a > 1 and .b < 2) | {a, b}`, want: []string{"a", "b"}},
		{program: `.a | select(. > 1)`, want: []string{"a"}},
		{program: `1 + 2`, want: nil},
		{program: `"x"`, want: nil},

		// Anything that can see the rest of the object.
		{program: `.`, refused: "whole object"},
		{program: `keys`, refused: "reads the whole object"},
		{program: `length`, refused: "reads the whole object"},
		{program: `to_entries`, refused: "reads the whole object"},
		{program: `has("a")`, refused: "reads the whole object"},
		{program: `del(.a)`, refused: "reads the whole object"},
		{program: `. + {x: 1}`, refused: "whole object"},
		{program: `{a: .a} + .`, refused: "whole object"},
		{program: `.[]`, refused: "reads the whole object"},
		{program: `..`, refused: "reads the whole object"},
		{program: `.a, .`, refused: "whole object"},
		{program: `[., .a]`, refused: "whole object"},
		{program: `{a: .}`, refused: "whole object"},
		{program: `select(.a)`, refused: "outputs the whole object"},
		{program: `select(.a) | .`, refused: "whole object"},
		{program: `select(.)`, refused: "whole object"},
		{program: `if .a then . else empty end`, refused: "whole object"},
		{program: `if .a then .b end`, refused: "whole object"},
		{program: `.a as $x | $x`, refused: "reads the whole object"},
		{program: `tostring`, refused: "reads the whole object"},
		{program: `map_values(.)`, refused: "reads the whole object"},
		{program: `.[0]`, refused: "other than a key"},
		{program: `.[.k]`, refused: "other than a key"},
		{program: `.["a-b"]`, refused: "can't name"},
		{program: `.a | input`, refused: "other input"},
		{program: `input`, refused: "reads the whole object"},
	} {
		t.Run(tt.program, func(t *testing.T) {
			expr, err := jq.Parse(tt.program)
			if err != nil {
				t.Fatal(err)
			}
			got, why := jqRootKeys(expr)
			if tt.refused != "" {
				if why == "" || !strings.Contains(why, tt.refused) {
					t.Errorf("keys = %v, why = %q, want it refused for %q", got, why, tt.refused)
				}
				return
			}
			if why != "" {
				t.Fatalf("refused: %s", why)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("keys = %v, want %v", got, tt.want)
			}
		})
	}
}

// The plan: a first jq stage that stays in compute and reads some keys is
// sent just those; others, or one that sits where it can't be, read the whole
// payload.
func TestProjectPayload_Plan(t *testing.T) {
	for _, tt := range []struct {
		pipeline string
		narrowed bool
	}{
		{`cat vimgym.log | jq '.items | map(. * 2)'`, true},
		{`cat vimgym.log | jq '"\(.host) \(.used_gb)"'`, true},
		{`cat vimgym.log | jq 'select(.type == "disk") | "\(.host)"'`, true},
		{`cat vimgym.log | jq '.a | length' | sort`, true},
		{`head -n 100 vimgym.log | jq '.a | length'`, true},
		// Reads the object.
		{`cat vimgym.log | jq 'keys'`, false},
		{`cat vimgym.log | jq 'select(.a | test("x"))'`, false},
		{`cat vimgym.log | jq -s 'map(.a)'`, false},
		// Other shapes: a follow, a trace, another first stage.
		{`tail -f vimgym.log | jq '.a | length'`, false},
		{`cat vimgym.log | sort | jq '.a | length'`, false},
		// Lowered whole: nothing left to narrow.
		{`cat vimgym.log | jq '.a'`, false},
		{`cat vimgym.log | jq 'select(.status == 500)'`, false},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			_, c, err := compileBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if got := c != nil && c.payloadSelect != ""; got != tt.narrowed {
				t.Errorf("narrowed = %v, want %v\n%s", got, tt.narrowed, Explain(tt.pipeline))
			}
		})
	}
}
