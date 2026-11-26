package cli

import (
	"bytes"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/w3ft-net/weft/cmd/weft/ui"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shell"
)

func TestRenderPendingShowsMeterLastLineAndPreview(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 10, 0, time.UTC)
	var buf bytes.Buffer
	renderPending(&buf, []shell.PendingSource{{
		Service: "api", Namespace: "acme", Status: "sampling",
		SampleCollected: 25, SampleTarget: 50,
		SampleUpdatedAt: "2026-10-14T12:00:06Z",
		Sample:          []byte(`["first","GET  /health   200"]`),
	}}, now, 80)
	out := buf.String()
	for _, want := range []string{"[■■■■■■■■■■----------]", " 25/50 ", "sampling", "4s ago", "› GET /health 200"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// Right after a resample the sample is empty: say so, not "last line 3s ago".
func TestRenderPendingAfterResampleWaitsForLines(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 10, 0, time.UTC)
	var buf bytes.Buffer
	renderPending(&buf, []shell.PendingSource{{
		Service: "api", Namespace: "acme", Status: "sampling",
		SampleCollected: 0, SampleTarget: 50, SampleUpdatedAt: "2026-10-14T12:00:07Z",
	}}, now, 80)
	if out := buf.String(); !strings.Contains(out, "waiting for new lines") || strings.Contains(out, "3s ago") {
		t.Errorf("output:\n%s", out)
	}
}

// The AGENTS column says who reads a weftd source at a glance.
func TestAgentsText(t *testing.T) {
	for _, c := range []struct {
		s    shell.SourceInfo
		want string
	}{
		{shell.SourceInfo{SourceType: "weftd", Agents: &shell.AgentFleet{Count: 3, Oldest: "0.23.0", Newest: "0.28.0"}}, "0.23.0-0.28.0 (3 hosts)"},
		{shell.SourceInfo{SourceType: "weftd", Agents: &shell.AgentFleet{Count: 1, Oldest: "0.28.0", Newest: "0.28.0"}}, "0.28.0 (1 host)"},
		{shell.SourceInfo{SourceType: "weftd"}, "unknown"},
		{shell.SourceInfo{SourceType: "direct"}, "-"},
		{shell.SourceInfo{SourceType: "kinesis"}, "-"},
	} {
		if got := agentsText(c.s); got != c.want {
			t.Errorf("agentsText(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

// On a color terminal the table carries the weft palette; the layout (and so
// the plain-text output other tests pin) is unchanged.
func TestRenderPendingColorsFromWeftPalette(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 10, 0, time.UTC)
	src := []shell.PendingSource{{
		Service: "api", Namespace: "acme", Status: "pending",
		SampleCollected: 50, SampleTarget: 50, SampleUpdatedAt: "2026-10-14T12:00:06Z",
	}}
	var color, plain bytes.Buffer
	r := lipgloss.NewRenderer(&color)
	r.SetColorProfile(termenv.ANSI256)
	renderPendingStyled(ui.StylesFor(r), &color, src, now, 80)
	renderPending(&plain, src, now, 80)
	for _, want := range []string{"38;5;39", "38;5;75", "38;5;214"} {
		if !strings.Contains(color.String(), want) {
			t.Errorf("colored output missing %q:\n%q", want, color.String())
		}
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Errorf("non-terminal output must stay uncolored: %q", plain.String())
	}
	if stripped := ansi.Strip(color.String()); stripped != plain.String() {
		t.Errorf("color changed layout:\n%q\nvs\n%q", stripped, plain.String())
	}
}
