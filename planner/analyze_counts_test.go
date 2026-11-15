package planner

import (
	"strings"
	"testing"
)

// TestRetrievalCount: the count pipeline is the user's own source and
// optimized stages, verbatim, then wc -l; and only where a count says
// something.
func TestRetrievalCount(t *testing.T) {
	const post = `awk '{ printf "%s\n", $0 }'`
	for _, tt := range []struct {
		pipeline string
		want     string
	}{
		{`cat /log/x/y/main.log | grep ERROR | ` + post, `cat /log/x/y/main.log | grep ERROR | wc -l`},
		{`cat /log/x/y/main.log | grep ERROR | jq -r .msg | ` + post, `cat /log/x/y/main.log | grep ERROR | jq -r .msg | wc -l`},
		{`cat /log/x/y/main.log | ` + post, `cat /log/x/y/main.log | wc -l`},
		{`cat /log/x/y/main.log | grep 'a  b' | ` + post, `cat /log/x/y/main.log | grep 'a  b' | wc -l`},
		// A filter after a line count runs on the rows it kept, in compute: only the
		// source is retrieval.
		{`tail -n 500 /log/x/y/main.log | grep ERROR | ` + post, `tail -n 500 /log/x/y/main.log | wc -l`},
		// Stages after the barrier are not run, optimized or not.
		{`cat /log/x/y/main.log | ` + post + ` | sort | uniq -c`, `cat /log/x/y/main.log | wc -l`},
		// The pipeline is its own result, or can't be counted.
		{`cat /log/x/y/main.log | grep ERROR`, ``},
		{`tail -f /log/x/y/main.log | ` + post, ``},
	} {
		t.Run(tt.pipeline, func(t *testing.T) {
			a, err := Analyze(tt.pipeline, "/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if a.RetrievalCount != tt.want {
				t.Errorf("RetrievalCount = %q, want %q", a.RetrievalCount, tt.want)
			}
		})
	}
}

// The count sits under the last optimized stage, right before the barrier's.
func TestRender_RetrievedRows(t *testing.T) {
	const post = `awk '{ printf "%s\n", $0 }'`
	for _, tt := range []struct {
		name     string
		pipeline string
		rows     int64
		count    string // the line printed
		next     string // the line it is followed by begins with
	}{
		{"after optimized stages", `cat /log/x/y/main.log | grep ERROR | jq -r .msg | ` + post, 259662,
			"259,662 lines retrieved with optimization", "3  awk"},
		{"no optimized stage", `cat /log/x/y/main.log | ` + post, 962932, "962,932 lines retrieved", "1  awk"},
		{"none", `cat /log/x/y/main.log | grep ERROR | ` + post, 0, "0 lines retrieved with optimization", "2  awk"},
		{"one", `cat /log/x/y/main.log | grep ERROR | ` + post, 1, "1 line retrieved with optimization", "2  awk"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.pipeline, "/", nil)
			if err != nil {
				t.Fatal(err)
			}
			a.Retrieved(tt.rows)
			lines := strings.Split(a.Render(false), "\n")
			for i, l := range lines {
				if strings.TrimSpace(l) != tt.count {
					continue
				}
				if i+1 >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), tt.next) {
					t.Errorf("count followed by %q, want a line starting %q", lines[i+1], tt.next)
				}
				return
			}
			t.Errorf("no %q line in:\n%s", tt.count, strings.Join(lines, "\n"))
		})
	}
}

func TestRender_RowsNoteReplacesTheCount(t *testing.T) {
	a, err := Analyze(`cat /log/x/y/main.log | grep ERROR | awk '{ printf "%s\n", $0 }'`, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.RowsNote = "took longer than 5s"
	if out := a.Render(false); !strings.Contains(out, "line count unavailable: took longer than 5s") {
		t.Errorf("no note in:\n%s", out)
	}
	// Static output is unchanged when nothing was counted.
	b, _ := Analyze(`cat /log/x/y/main.log | grep ERROR | awk '{ printf "%s\n", $0 }'`, "/", nil)
	if out := b.Render(false); strings.Contains(out, "lines retrieved") || strings.Contains(out, "unavailable") {
		t.Errorf("static output mentions a count:\n%s", out)
	}
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 259662: "259,662", 1234567: "1,234,567", -4321: "-4,321"} {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}
