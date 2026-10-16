package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func colorStyles(buf *bytes.Buffer) styles {
	r := lipgloss.NewRenderer(buf)
	r.SetColorProfile(termenv.ANSI256)
	return stylesFor(r)
}

// --nocolor is a global flag: it is removed from argv wherever it appears so
// no subcommand's own parser sees it.
func TestNoColorIsAGlobalFlag(t *testing.T) {
	defer func(a []string, n bool) { os.Args, noColor = a, n }(os.Args, noColor)
	for _, args := range [][]string{
		{"weft", "--nocolor", "source", "list"},
		{"weft", "source", "list", "--nocolor"},
		{"weft", "source", "--no-color", "list"},
	} {
		os.Args, noColor = args, false
		extractTopLevelFlags()
		if !noColor || strings.Join(os.Args, " ") != "weft source list" {
			t.Errorf("%v -> noColor=%v args=%v", args, noColor, os.Args)
		}
	}
}

// With --nocolor a terminal-bound writer still gets no escape codes.
func TestNoColorOverridesTerminalProfile(t *testing.T) {
	defer func(n bool) { noColor = n }(noColor)
	noColor = true
	var buf bytes.Buffer
	st := newStyles(&buf)
	if out := st.head.Render("X") + st.err.Render("Y"); strings.Contains(out, "\x1b") {
		t.Errorf("escape codes under --nocolor: %q", out)
	}
}

// Tables line up whether or not they are colored, and carry the palette.
func TestWriteTableAlignsAndColors(t *testing.T) {
	cols := []column{nameCol("NAME", 8), statusCol("STATUS", 9), {title: "NOTE"}}
	rows := [][]string{{"api", "pending", "ready"}, {"compute", "sampling 4/50", "x"}}
	var color, plain bytes.Buffer
	writeTable(&color, colorStyles(&color), "  ", cols, rows)
	writeTable(&plain, newStyles(&plain), "  ", cols, rows)
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
	got := ansi.Strip(paint(colorStyles(&buf).err, "\nboom\n"))
	if got != "\nboom\n" {
		t.Errorf("paint = %q", got)
	}
}

func TestColorDiffTintsAddsAndRemoves(t *testing.T) {
	var buf bytes.Buffer
	out := colorStyles(&buf).colorDiff("--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n ctx")
	if ansi.Strip(out) != "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n ctx" {
		t.Errorf("diff text changed: %q", out)
	}
	if !strings.Contains(out, "32m+new") || !strings.Contains(out, "38;5;209m-old") {
		t.Errorf("diff not tinted: %q", out)
	}
}
