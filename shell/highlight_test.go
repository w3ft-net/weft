package shell

import (
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// shellcore's Tokenize/Token surface was deleted when the bash
// highlighter moved to pkg/syntax. Tokenization-level coverage now
// lives in pkg/syntax/bash_test.go; weft's smoke tests below stick to
// the public ANSI rendering API that weft itself depends on.

func TestANSIHighlight(t *testing.T) {
	out := shellcore.HighlightPipeline("cat *.log | grep ERROR | sort -rn")
	if out == "" {
		t.Fatal("HighlightPipeline returned empty string")
	}
	for _, word := range []string{"cat", "*.log", "|", "grep", "ERROR", "sort", "-rn"} {
		if !containsText(out, word) {
			t.Errorf("output missing word %q", word)
		}
	}
}

func TestANSIHighlightWithCursor(t *testing.T) {
	cases := []struct {
		name  string
		input string
		pos   int
	}{
		{"start", "cat", 0},
		{"end", "cat", 3},
		{"mid-token", "grep ERROR", 2},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := shellcore.HighlightWithCursor(tc.input, tc.pos)
			if out == "" {
				t.Fatal("empty output")
			}
		})
	}
}

func containsText(s, sub string) bool {
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
	for i := 0; i <= len(clean)-len(sub); i++ {
		if clean[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
