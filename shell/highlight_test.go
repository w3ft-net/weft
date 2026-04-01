package shell

import (
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []token
	}{
		{
			name:  "simple command",
			input: "grep ERROR",
			expect: []token{
				{tokCmd, "grep"},
				{tokSpace, " "},
				{tokText, "ERROR"},
			},
		},
		{
			name:  "pipeline",
			input: "cat api.log | grep ERROR | sort",
			expect: []token{
				{tokCmd, "cat"},
				{tokSpace, " "},
				{tokFile, "api.log"},
				{tokSpace, " "},
				{tokPipe, "|"},
				{tokSpace, " "},
				{tokCmd, "grep"},
				{tokSpace, " "},
				{tokText, "ERROR"},
				{tokSpace, " "},
				{tokPipe, "|"},
				{tokSpace, " "},
				{tokCmd, "sort"},
			},
		},
		{
			name:  "flags",
			input: "sort -rn -k 3",
			expect: []token{
				{tokCmd, "sort"},
				{tokSpace, " "},
				{tokFlag, "-rn"},
				{tokSpace, " "},
				{tokFlag, "-k"},
				{tokSpace, " "},
				{tokText, "3"},
			},
		},
		{
			name:  "single quoted string",
			input: "awk '{print $4}'",
			expect: []token{
				{tokCmd, "awk"},
				{tokSpace, " "},
				{tokString, "'{print $4}'"},
			},
		},
		{
			name:  "double quoted string",
			input: `grep "hello world"`,
			expect: []token{
				{tokCmd, "grep"},
				{tokSpace, " "},
				{tokString, `"hello world"`},
			},
		},
		{
			name:  "glob file",
			input: "cat *.log",
			expect: []token{
				{tokCmd, "cat"},
				{tokSpace, " "},
				{tokFile, "*.log"},
			},
		},
		{
			name:  "path file",
			input: "cat /var/log/app.log",
			expect: []token{
				{tokCmd, "cat"},
				{tokSpace, " "},
				{tokFile, "/var/log/app.log"},
			},
		},
		{
			name:  "pipe resets command expectation",
			input: "cat foo | awk '{x}' | sort -r",
			expect: []token{
				{tokCmd, "cat"},
				{tokSpace, " "},
				{tokText, "foo"},
				{tokSpace, " "},
				{tokPipe, "|"},
				{tokSpace, " "},
				{tokCmd, "awk"},
				{tokSpace, " "},
				{tokString, "'{x}'"},
				{tokSpace, " "},
				{tokPipe, "|"},
				{tokSpace, " "},
				{tokCmd, "sort"},
				{tokSpace, " "},
				{tokFlag, "-r"},
			},
		},
		{
			name:   "empty input",
			input:  "",
			expect: nil,
		},
		{
			name:  "jq pipeline",
			input: `jq 'select(.status > 500)' | jq '{path, status}'`,
			expect: []token{
				{tokCmd, "jq"},
				{tokSpace, " "},
				{tokString, "'select(.status > 500)'"},
				{tokSpace, " "},
				{tokPipe, "|"},
				{tokSpace, " "},
				{tokCmd, "jq"},
				{tokSpace, " "},
				{tokString, "'{path, status}'"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.input)
			if len(got) != len(tt.expect) {
				t.Fatalf("tokenize(%q): got %d tokens, want %d\n  got:  %v\n  want: %v",
					tt.input, len(got), len(tt.expect), got, tt.expect)
			}
			for i := range got {
				if got[i].kind != tt.expect[i].kind || got[i].value != tt.expect[i].value {
					t.Errorf("token[%d]: got {%d %q}, want {%d %q}",
						i, got[i].kind, got[i].value, tt.expect[i].kind, tt.expect[i].value)
				}
			}
		})
	}
}

func TestHighlightPipeline(t *testing.T) {
	// Smoke test — ensure it doesn't panic and preserves all text content.
	input := "cat *.log | grep ERROR | sort -rn"
	out := highlightPipeline(input)
	if out == "" {
		t.Fatal("highlightPipeline returned empty string")
	}
	// All original words should appear in the output (may be wrapped in ANSI codes).
	for _, word := range []string{"cat", "*.log", "|", "grep", "ERROR", "sort", "-rn"} {
		if !containsText(out, word) {
			t.Errorf("output missing word %q", word)
		}
	}
}

func containsText(s, sub string) bool {
	// Strip ANSI escape sequences for comparison.
	clean := ""
	inEsc := false
	for _, r := range s {
		if r == '\033' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		clean += string(r)
	}
	return len(sub) > 0 && len(clean) > 0 && contains(clean, sub)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > len(sub) && searchString(s, sub))
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHighlightWithCursor(t *testing.T) {
	// Cursor at start.
	out := highlightWithCursor("cat", 0)
	if out == "" {
		t.Fatal("empty output for cursor at start")
	}

	// Cursor at end.
	out = highlightWithCursor("cat", 3)
	if out == "" {
		t.Fatal("empty output for cursor at end")
	}

	// Cursor in middle of token.
	out = highlightWithCursor("grep ERROR", 2)
	if out == "" {
		t.Fatal("empty output for cursor mid-token")
	}

	// Empty input, cursor at 0.
	out = highlightWithCursor("", 0)
	if out == "" {
		t.Fatal("empty output for empty input")
	}
}
