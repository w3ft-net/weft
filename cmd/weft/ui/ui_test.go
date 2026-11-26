package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func colorStyles(buf *bytes.Buffer) Styles {
	r := lipgloss.NewRenderer(buf)
	r.SetColorProfile(termenv.ANSI256)
	return StylesFor(r)
}

func TestNoColorOverridesTerminalProfile(t *testing.T) {
	defer func(n bool) { NoColor = n }(NoColor)
	NoColor = true
	var buf bytes.Buffer
	st := NewStyles(&buf)
	if out := st.Head.Render("X") + st.Err.Render("Y"); strings.Contains(out, "\x1b") {
		t.Errorf("escape codes under --nocolor: %q", out)
	}
}

// Tables line up whether or not they are colored, and carry the palette.
func TestWriteTableAlignsAndColors(t *testing.T) {
	cols := []Column{NameCol("NAME", 8), StatusCol("STATUS", 9), {Title: "NOTE"}}
	rows := [][]string{{"api", "pending", "ready"}, {"compute", "sampling 4/50", "x"}}
	var color, plain bytes.Buffer
	WriteTable(&color, colorStyles(&color), "  ", cols, rows)
	WriteTable(&plain, NewStyles(&plain), "  ", cols, rows)
	if strings.Contains(plain.String(), "\x1b") {
		t.Fatalf("plain table has escapes: %q", plain.String())
	}
	if got := ansi.Strip(color.String()); got != plain.String() {
		t.Errorf("color changed layout:\n%q\n%q", got, plain.String())
	}
	for _, want := range []string{"38;5;39", "38;5;75", "1;32m", "38;5;208"} { // head, name, ready, active
		if !strings.Contains(color.String(), want) {
			t.Errorf("colored table missing %q", want)
		}
	}
}

func TestPaintKeepsSurroundingNewlines(t *testing.T) {
	var buf bytes.Buffer
	got := ansi.Strip(Paint(colorStyles(&buf).Err, "\nboom\n"))
	if got != "\nboom\n" {
		t.Errorf("Paint = %q", got)
	}
}

func TestColorDiffTintsAddsAndRemoves(t *testing.T) {
	var buf bytes.Buffer
	out := colorStyles(&buf).ColorDiff("--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n ctx")
	if ansi.Strip(out) != "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n ctx" {
		t.Errorf("diff text changed: %q", out)
	}
	if !strings.Contains(out, "32m+new") || !strings.Contains(out, "38;5;209m-old") {
		t.Errorf("diff not tinted: %q", out)
	}
}
