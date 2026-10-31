package planner

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// TestAwkFieldReads: a program is cut down to its fields only when nothing it
// does can see the rest of the line, and a program that can is refused for a
// stated reason.
func TestAwkFieldReads(t *testing.T) {
	for _, tt := range []struct {
		program string
		want    fieldReads
		refused string // a fragment of the reason, when refused
	}{
		// Reads only fields.
		{program: `{ c[$4]++ } END { for (k in c) print k, c[k] }`, want: fieldReads{positions: []int{4}}},
		{program: `{ s[$4] += $9; if ($NF > m[$4]) m[$4] = $NF } END { for (k in s) print k, s[k], m[k] }`,
			want: fieldReads{positions: []int{4, 9}, last: true, nf: true}},
		{program: `$2 == "x" { printf "%s %s\n", $1, NF }`, want: fieldReads{positions: []int{1, 2}, nf: true}},
		{program: `{ n++ } END { print n }`, want: fieldReads{}},
		{program: `BEGIN { print "header" }`, want: fieldReads{}},
		{program: `{ print length($3), toupper($1), substr($2, 1, 3) }`, want: fieldReads{positions: []int{1, 2, 3}}},
		{program: `$1 ~ /^a/ { print $1 }`, want: fieldReads{positions: []int{1}}},
		{program: `{ x = $2; gsub(/a/, "b", x); print x }`, want: fieldReads{positions: []int{2}}},
		{program: `{ n = split($3, parts, /,/); print n }`, want: fieldReads{positions: []int{3}}},
		{program: `NR > 1 { print HOST, $1 }`, want: fieldReads{positions: []int{1}}},

		// Anything that can see the rest of the line.
		{program: `$1 == "x"`, refused: "no action"},
		{program: `{ print }`, refused: "no arguments"},
		{program: `{ print $0 }`, refused: "$0"},
		{program: `{ print length }`, refused: "bare length"},
		{program: `{ print length() }`, refused: "length"},
		{program: `/err/ { print $1 }`, refused: "/regex/"},
		{program: `{ if (/err/) print $1 }`, refused: "/regex/"},
		{program: `{ gsub(/a/, "b"); print $1 }`, refused: "gsub"},
		{program: `{ sub(/a/, "b", $2); print $1 }`, refused: "sub"},
		{program: `{ $2 = "x"; print $1 }`, refused: "assigns"},
		{program: `{ NF = 2; print $1 }`, refused: "assigns"},
		{program: `{ $3++ ; print $1 }`, refused: "assigns"},
		{program: `{ print $i }`, refused: "field position"},
		{program: `{ print $(NF-1) }`, refused: "field position"},
		{program: `{ print $($1) }`, refused: "field position"},
		{program: `{ print $64 }`, refused: "field position"},
		{program: `BEGIN { FS = "," } { print $1 }`, refused: "assigns"},
		{program: `{ print $1, FS }`, refused: "split"},
		{program: `{ while ((getline line) > 0) n++; print $1 }`, refused: "construct"},
		{program: `{ print $1 >> "/tmp/x" }`, refused: "redirects"},
		{program: `function f(a) { return a } { print f($1) }`, refused: "function"},
		{program: `{ system("true"); print $1 }`, refused: "system"},
	} {
		t.Run(tt.program, func(t *testing.T) {
			prog, err := awk.Parse(tt.program)
			if err != nil {
				t.Fatal(err)
			}
			got, why := awkFieldReads(prog)
			if tt.refused != "" {
				if why == "" || !strings.Contains(why, tt.refused) {
					t.Errorf("reads = %+v, why = %q, want it refused for %q", got, why, tt.refused)
				}
				return
			}
			if why != "" {
				t.Fatalf("refused: %s", why)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("reads = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The plan: a first awk stage that reads some fields is cut down to them; a
// stage that can't be, or sits where it can't, ships whole lines.
func TestProjectFields_Plan(t *testing.T) {
	for _, tt := range []struct {
		pipeline  string
		projected bool
	}{
		{`cat vimgym.log | awk '{ s[$4] += $9 } END { for (k in s) print k, s[k] }'`, true},
		{`cat vimgym.log | grep ERROR | awk '{ n++ } END { print n }'`, true},
		{`cat vimgym.log | awk '{ printf "%s %s\n", $1, $NF }'`, true},
		{`cat vimgym.log | awk '{ x = $3 + 1; print x }' | sort`, true},
		// Reads the line.
		{`cat vimgym.log | awk '{ print $0, $1 }'`, false},
		{`cat vimgym.log | awk '/a/ { n++ } END { print n }'`, false},
		// Other shapes: a projection ahead of it, a pushed line count, a
		// follow, a trace, another first stage.
		{`cat vimgym.log | cut -d' ' -f2- | awk '{ n += $1 } END { print n }'`, false},
		{`head -n 100 vimgym.log | awk '{ n += $1 } END { print n }'`, false},
		{`tail -f vimgym.log | awk '{ n += $1 } END { print n }'`, false},
		{`cat vimgym.log | sort | awk '{ n += $1 } END { print n }'`, false},
		// Lowered whole: nothing left to project.
		{`cat vimgym.log | awk '{ print $1 }'`, false},
		{`cat vimgym.log | awk '{ c[$1]++ } END { for (k in c) print k, c[k] }'`, false},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			plan, c, err := compile(tt.pipeline, 0, "", time.Time{}, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := c != nil && c.fieldSelect != ""; got != tt.projected {
				t.Errorf("projected = %v, want %v\n%s", got, tt.projected, Explain(tt.pipeline))
			}
			_ = plan
		})
	}
}

// A flagged awk stage is not cut down to its fields, and explain says why in
// terms of the flag: -F changes how a line splits, so its fields can't be
// picked by position; any other flag just isn't analyzed.
func TestExplain_FieldProjectionNamesTheFlagThatStopsIt(t *testing.T) {
	for _, c := range []struct{ prog, want string }{
		{`awk -F, '{ s += $2 } END { print s }'`, "skipped Opaque[1: awk]: -F changes how it splits a line, so the shard can't pick its fields by position"},
		{`awk -F ',' '{ s += $2 } END { print s }'`, "skipped Opaque[1: awk]: -F changes how it splits a line, so the shard can't pick its fields by position"},
		{`awk -v k=2 '{ s += $2 * k } END { print s }'`, "skipped Opaque[1: awk]: its flag -v isn't analyzed, so the shard sends whole lines"},
	} {
		out := Explain("cat /log/a/b/main.log | " + c.prog)
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "field-projection") {
				line = l
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(line), c.want) {
			t.Errorf("%s:\nfield-projection line = %q\nwant it to end with %q", c.prog, line, c.want)
		}
	}
}
