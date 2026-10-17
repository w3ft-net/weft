package setup

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/muesli/termenv"
)

func testPreview() *ingest.Preview {
	return &ingest.Preview{
		Lines: [][]ingest.PreviewSegment{
			{{Text: "2026-01-01T00:00:00Z", Field: ingest.ProbeTimestamp}, {Text: " "}, {Text: "INFO", Field: ingest.ProbeLevel}, {Text: " started"}},
		},
		Fields: []ingest.ProbeField{ingest.ProbeTimestamp, ingest.ProbeLevel},
	}
}

func rendererWith(p termenv.Profile) *lipgloss.Renderer {
	r := lipgloss.NewRenderer(&bytes.Buffer{})
	r.SetColorProfile(p)
	return r
}

func TestRenderPreviewNoColorGolden(t *testing.T) {
	got := RenderPreviewWith(rendererWith(termenv.Ascii), testPreview(), 80)
	want := "2026-01-01T00:00:00Z INFO started\n\n■ timestamp  ■ level\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderPreviewColorsMatchPlain(t *testing.T) {
	col := RenderPreviewWith(rendererWith(termenv.ANSI256), testPreview(), 80)
	if !strings.Contains(col, "\x1b[") {
		t.Fatal("expected ANSI escapes")
	}
	if !strings.Contains(col, "38;5;39") || !strings.Contains(col, "38;5;208") {
		t.Fatalf("missing palette colors: %q", col)
	}
	plain := RenderPreviewWith(rendererWith(termenv.Ascii), testPreview(), 80)
	if ansi.Strip(col) != plain {
		t.Fatalf("stripped %q != plain %q", ansi.Strip(col), plain)
	}
}

func TestRenderPreviewWindowsToKeepValueVisible(t *testing.T) {
	long := strings.Repeat("x", 100)
	p := &ingest.Preview{
		Lines:  [][]ingest.PreviewSegment{{{Text: long}, {Text: "TRACE", Field: ingest.ProbeTraceID}, {Text: long}}},
		Fields: []ingest.ProbeField{ingest.ProbeTraceID},
	}
	out := ansi.Strip(RenderPreviewWith(rendererWith(termenv.Ascii), p, 40))
	line := strings.SplitN(out, "\n", 2)[0]
	if len([]rune(line)) != 40 || !strings.Contains(line, "TRACE") {
		t.Fatalf("bad window %q", line)
	}
}
