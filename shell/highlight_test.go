package shell

import (
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

func TestTokenizeDelegation(t *testing.T) {
	tokens := shellcore.Tokenize("cat api.log | grep ERROR")
	if len(tokens) == 0 {
		t.Fatal("expected tokens from shellcore.Tokenize")
	}
	if tokens[0].Kind != shellcore.TokCmd || tokens[0].Value != "cat" {
		t.Errorf("first token: got {%d %q}, want {%d %q}",
			tokens[0].Kind, tokens[0].Value, shellcore.TokCmd, "cat")
	}
}

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
