package store

import (
	"strings"
	"testing"
)

func TestCompileWatchPipeline_Schema(t *testing.T) {
	cases := []struct {
		name            string
		pipeline        string
		wantValue       string
		wantTag         string
		wantAlert       bool
		wantErrContains string
	}{
		{"default text rule", "cat /log/a.log | wc -l", "", "", false, ""},
		{"headers", "# value: .pct\n# tag: .host\ncat /log/a.log | jq -c .", ".pct", ".host", false, ""},
		{"value header alone", "# value: .pct\ncat /log/a.log", ".pct", "", false, ""},
		{"alert flags", "foo | alert above 80 -v 2 -t 1", "2", "1", true, ""},
		{"flagless alert", "foo | alert above 80", "", "", true, ""},
		{"headers and alert -v", "# value: .pct\nfoo | alert above 80 -v .pct", "", "", false, "sets its schema twice"},
		{"headers and flagless alert", "# value: .pct\nfoo | alert above 80", "", "", false, "declares its schema with alert -v .pct"},
		{"tag without value", "# tag: .host\nfoo", "", "", false, "'# tag:' needs a '# value:' header"},
		{"column number header", "# value: 2\nfoo", "", "", false, "takes a JSON path"},
		{"header given twice", "# value: .a\n# value: .b\nfoo", "", "", false, "given twice"},
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CompileWatchPipeline(c.pipeline)
			if c.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrContains) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if str(got.ValueField) != c.wantValue || str(got.TagField) != c.wantTag || got.HasAlert != c.wantAlert {
				t.Fatalf("value=%q tag=%q alert=%v, want %q %q %v",
					str(got.ValueField), str(got.TagField), got.HasAlert, c.wantValue, c.wantTag, c.wantAlert)
			}
		})
	}
}
