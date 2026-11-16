package theme

import (
	"strconv"
	"strings"
	"testing"
)

func TestTraceTag_Deterministic(t *testing.T) {
	a := TraceTag("acme/prod", "api")
	b := TraceTag("acme/prod", "api")
	if a != b {
		t.Errorf("TraceTag not deterministic across calls: %q vs %q", a, b)
	}
}

func TestTraceTag_DistinctAcrossServices(t *testing.T) {
	inputs := [][2]string{
		{"acme/prod", "api"},
		{"acme/prod", "edge"},
		{"acme/prod", "worker"},
		{"weft", "coordinator"},
	}
	seen := map[string]bool{}
	for _, in := range inputs {
		seen[TraceTag(in[0], in[1])] = true
	}
	if len(seen) < 2 {
		t.Errorf("all %d distinct service inputs mapped to the same badge: %v", len(inputs), seen)
	}
}

func TestTraceTag_LabelsBracketed(t *testing.T) {
	got := TraceTag("acme/prod", "api")
	if !strings.Contains(got, "[acme/prod/api]") {
		t.Errorf("TraceTag(%q, %q) = %q, want it to contain \"[acme/prod/api]\"", "acme/prod", "api", got)
	}
}

func TestTraceTag_EmptyNamespaceAndService(t *testing.T) {
	got := TraceTag("", "")
	if !strings.Contains(got, "[unknown]") {
		t.Errorf("TraceTag(\"\", \"\") = %q, want it to contain \"[unknown]\"", got)
	}
}

// TestTraceTag_NoCollisionWithSemanticPalette guards against a service
// badge accidentally sharing a 256-color code with anything
// Palette() already assigns meaning to (Error, Comment, etc.) — a
// badge in that color would visually misread as an error or a
// comment rather than a neutral origin tag.
func TestTraceTag_NoCollisionWithSemanticPalette(t *testing.T) {
	p := Palette()
	semantic := []string{
		p.Command, p.Keyword, p.Func, p.Builtin, p.Flag,
		p.String, p.StringEsc, p.Key, p.Number, p.Operator, p.Punct,
		p.Comment, p.Error,
		p.DiffAdd, p.DiffDel, p.DiffHunk, p.DiffFileHeader,
		p.MdHeading, p.MdEmphasis, p.MdCodeSpan, p.MdLinkText, p.MdLinkURL, p.MdListMarker, p.MdQuote, p.MdRule,
	}
	for _, code := range traceTagColors {
		needle := "38;5;" + strconv.Itoa(code) + "m"
		for _, s := range semantic {
			if strings.Contains(s, needle) {
				t.Errorf("trace badge color %d collides with a semantic palette code (found in %q)", code, s)
			}
		}
	}
}
