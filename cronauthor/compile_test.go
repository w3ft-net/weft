package cronauthor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/cron"
)

func TestCompileWatchPipeline_TextType(t *testing.T) {
	cases := []struct {
		name, pipeline, wantErr string
		wantType                string
	}{
		{"default is number", "echo 1", "", cron.ValueNumber},
		{"text", "# type: text\necho i-0abc lease", "", cron.ValueText},
		{"text with JSON value", "# type: text\n# value: .holder\n# tag: .lease\ncat x", "", cron.ValueText},
		{"unknown type", "# type: number\necho 1", "'# type:' takes text", ""},
		{"type twice", "# type: text\n# type: text\necho a", "given twice", ""},
		{"text alert changed", "# type: text\necho a | alert changed", "", cron.ValueText},
		{"text alert absent", "# type: text\necho a | alert absent", "", cron.ValueText},
		{"text alert above", "# type: text\necho a | alert above 5", "this cron stores text (# type: text), and alert above compares numbers", ""},
		{"text alert matches", "# type: text\ncat /log/myco/prod/api/main.log | grep 'starting version' | awk '{v[HOST]=$NF} END {for (h in v) print v[h], h}' | alert matches '-rc[0-9]*$' -v 1 -t 2", "", cron.ValueText},
		{"number alert matches", "echo 1 | alert matches '^1'", "alert matches reads text; add '# type: text'", ""},
		{"text alert above lists matches", "# type: text\necho a | alert below 5", "use any, absent, changed or matches", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Compile(c.pipeline)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got.ValueType != c.wantType {
				t.Fatalf("type=%q err=%v, want %q", got.ValueType, err, c.wantType)
			}
		})
	}
}

// Only spelled-out names are seen; a name built at run time is the runtime
// check's job.
func TestCompileWatchPipeline_NumericRefs(t *testing.T) {
	got, err := Compile(`x=$(weft::avg errors 1h); weft::latest holders; weft::is_above 'disk' 80; weft::max $name 1h`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.NumericRefs) != "[errors disk]" {
		t.Fatalf("NumericRefs = %v, want [errors disk]", got.NumericRefs)
	}
}

// Inline `lua '<program>'` stages are scanned like the bash around them,
// and a program that doesn't parse is refused at save.
func TestCompileWatchPipeline_InlineLua(t *testing.T) {
	got, err := Compile(`lua 'print(weft.avg("errors", "1h"))' | alert above 5`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.NumericRefs) != "[errors]" {
		t.Fatalf("NumericRefs = %v, want [errors]", got.NumericRefs)
	}
	if _, err := Compile(`lua 'print(weft.avg("errors"'`); err == nil || !strings.Contains(err.Error(), "lua: syntax error") {
		t.Fatalf("inline Lua syntax error: %v", err)
	}
	// A program built at run time can't be read at save.
	if got, err := Compile(`lua "$prog"`); err != nil || len(got.NumericRefs) != 0 {
		t.Fatalf("dynamic program: refs=%v err=%v", got.NumericRefs, err)
	}
}
