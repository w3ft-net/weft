package main

import (
	"strings"
	"testing"
)

// TestRenderShell_Alerts feeds a representative alerts topic through
// the renderer and asserts on the shape of the output. Failures dump
// the full rendered text so drift in formatting is easy to inspect.
func TestRenderShell_Alerts(t *testing.T) {
	src := []byte(`## Alerts

An alert evaluates a value from stdin against a condition.
When the condition holds, ` + "`alert`" + ` writes to stderr.

## Conditions

- ` + "`above <N>`" + ` — value greater than N
- ` + "`below <N>`" + ` — value less than N

See ` + "`man alert`" + ` for grammar.

## /etc/notify.yaml

` + "`--notify=<alias>`" + ` looks up a destination. Example:

` + "```" + `
ops:
  slack:
    - "#alerts"
  pagerduty:
    - ops-daytime
` + "```" + `
`)

	out := renderShell(src, "")

	// Section headings should be orange-wrapped. The exact escape is
	// the ANSI 256-color code 208 (orange), same as shellcore's
	// ansiAccent.
	wantOrange := []string{"Alerts", "Conditions", "/etc/notify.yaml"}
	for _, h := range wantOrange {
		wrapped := "\x1b[38;5;208m" + h + "\x1b[0m"
		if !strings.Contains(out, wrapped) {
			t.Errorf("missing orange heading %q\n--- output ---\n%s", h, out)
		}
	}

	// Body paragraphs are two-space indented.
	wantIndented := []string{
		"  An alert evaluates a value from stdin against a condition.",
		"  See man alert for grammar.",
	}
	for _, line := range wantIndented {
		if !strings.Contains(out, line) {
			t.Errorf("missing indented line %q\n--- output ---\n%s", line, out)
		}
	}

	// Code fences render indented four spaces (no ANSI).
	wantCode := []string{
		"    ops:",
		"      slack:",
		`        - "#alerts"`,
	}
	for _, line := range wantCode {
		if !strings.Contains(out, line) {
			t.Errorf("missing code line %q\n--- output ---\n%s", line, out)
		}
	}

	// Inline code backticks should be stripped — the terminal doesn't
	// get anything useful from the backtick glyphs.
	if strings.Contains(out, "`alert`") {
		t.Errorf("inline backticks not stripped\n--- output ---\n%s", out)
	}

	// A list followed by a paragraph should have a blank line between
	// the last list item and the paragraph — otherwise the prose
	// reads as another bullet.
	if strings.Contains(out, "  - `below <N>` — value less than N\n  See man alert") {
		t.Errorf("missing blank line between list and following paragraph\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "value less than N\n\n  See man alert") {
		t.Errorf("expected blank line after list before next paragraph\n--- output ---\n%s", out)
	}

	// H2 sits flush against the first line of its body, matching the
	// current hand-authored style (`AccentText("Alerts")\n  body`).
	if !strings.Contains(out, "\x1b[38;5;208mAlerts\x1b[0m\n  An alert") {
		t.Errorf("H2 should be immediately followed by body line, got:\n%s", out)
	}
}

// TestRenderShell_Blockquote renders a blockquote and asserts the
// body comes out dim-gray-wrapped so intro lines in the help index
// stay visually quiet.
func TestRenderShell_Blockquote(t *testing.T) {
	out := renderShell([]byte("> weft shell\n\n## Topics\n\nbody\n"), "")
	if !strings.Contains(out, "\x1b[38;5;8mweft shell\x1b[0m") {
		t.Errorf("blockquote not rendered as dim text\n--- output ---\n%s", out)
	}
}

// TestRenderShell_Banner verifies the man-page banner is centered
// across 78 columns with "Weft Commands" between two copies of the
// name — matching the hand-authored ALERT(1) / LESS(1) banners.
func TestRenderShell_Banner(t *testing.T) {
	out := renderShell([]byte("## Name\n\nbody\n"), "ALERT(1)")
	first := strings.SplitN(out, "\n", 2)[0]
	if !strings.HasPrefix(first, "ALERT(1)") || !strings.HasSuffix(first, "ALERT(1)") {
		t.Errorf("banner doesn't wrap the name: %q", first)
	}
	if !strings.Contains(first, "Weft Commands") {
		t.Errorf("banner missing 'Weft Commands': %q", first)
	}
	// The second line should be blank (banner then blank then body).
	lines := strings.SplitN(out, "\n", 3)
	if len(lines) < 2 || lines[1] != "" {
		t.Errorf("expected blank line after banner, got %q", lines)
	}
}
