package shellcore

import (
	"strings"
	"testing"
)

// typeAndEnter pushes a string into the shell input and submits it.
func typeAndEnter(c *ShellCore, s string) {
	for _, r := range s {
		c.HandleEvent(KeyEvent{Rune: r})
	}
	c.HandleEvent(KeyEvent{Key: KeyEnter})
}

// TestChartPipeStripsAndDispatches verifies that `... | chart -x ...`
// strips chart from the upstream pipeline, dispatches the upstream
// query, and renders the result into scrollback when the rows arrive.
func TestChartPipeStripsAndDispatches(t *testing.T) {
	core, h := newTestCore([]string{"api"})
	core.SetFS(&fakeWatchFS{})

	typeAndEnter(core, "cat /log/api.log | chart -x .ts -y .v --bucket 1m -a sum")

	if len(h.actions) == 0 {
		t.Fatal("no action dispatched")
	}
	q, ok := h.actions[0].(ActionQuery)
	if !ok {
		t.Fatalf("first action: got %T, want ActionQuery", h.actions[0])
	}
	if q.Pipeline != "cat /log/api.log" {
		t.Errorf("dispatched pipeline = %q, want %q (chart stripped)",
			q.Pipeline, "cat /log/api.log")
	}
	if core.pendingChart == nil {
		t.Fatal("pendingChart not set")
	}
	if !core.pendingChart.xSpec.IsJSON() || core.pendingChart.xSpec.Path != "ts" {
		t.Errorf("pendingChart.xSpec = %+v, want JSON path ts", core.pendingChart.xSpec)
	}

	// Deliver result rows; chart should render to scrollback.
	core.HandleEvent(QueryResultEvent{Text: strings.Join([]string{
		`{"ts":"2026-01-01T00:00:10Z","v":1}`,
		`{"ts":"2026-01-01T00:00:30Z","v":2}`,
		`{"ts":"2026-01-01T00:01:05Z","v":4}`,
		`{"ts":"2026-01-01T00:01:50Z","v":8}`,
	}, "\n") + "\n"})

	if core.pendingChart != nil {
		t.Error("pendingChart should be cleared after render")
	}
	out := h.Scrollback()
	if out == "" {
		t.Error("scrollback empty after chart render")
	}
	// Sanity: the rendered chart should contain at least one of the
	// y-tick labels (the smaller bucket sum is 3, the larger is 12).
	if !strings.Contains(out, "12") && !strings.Contains(out, "3") {
		t.Errorf("scrollback missing expected tick label:\n%s", out)
	}
}

// TestChartPipeBadFlagSurfacesError verifies a flag-parse error during
// startQuery short-circuits to scrollback without dispatching.
func TestChartPipeBadFlagSurfacesError(t *testing.T) {
	core, h := newTestCore([]string{"api"})
	core.SetFS(&fakeWatchFS{})

	typeAndEnter(core, "cat /log/api.log | chart -x 1 -y .latency")

	for _, a := range h.actions {
		if _, ok := a.(ActionQuery); ok {
			t.Error("bad chart args should not dispatch a query")
		}
	}
	if core.pendingChart != nil {
		t.Error("pendingChart should remain nil on parse error")
	}
	out := h.Scrollback()
	if !strings.Contains(out, "must both be positional or both") {
		t.Errorf("expected mixing error in scrollback, got:\n%s", out)
	}
}

// TestSplitTerminalStageChart verifies the boundary detection picks up
// chart and returns its argv tokens with quote unwrapping.
func TestSplitTerminalStageChart(t *testing.T) {
	weft, tokens := splitTerminalStage(
		`cat /log/api.log | jq '.path' | chart -x .path -y .latency --title "by path"`)
	if weft != "cat /log/api.log | jq '.path'" {
		t.Errorf("weft part: got %q", weft)
	}
	if len(tokens) == 0 || tokens[0] != "chart" {
		t.Fatalf("tokens: got %v, want chart head", tokens)
	}
	// --title argument should arrive unquoted.
	wantArgs := []string{"chart", "-x", ".path", "-y", ".latency", "--title", "by path"}
	if len(tokens) != len(wantArgs) {
		t.Fatalf("argv: got %v, want %v", tokens, wantArgs)
	}
	for i := range tokens {
		if tokens[i] != wantArgs[i] {
			t.Errorf("argv[%d] = %q, want %q", i, tokens[i], wantArgs[i])
		}
	}
}

// TestSplitTerminalStageLessUnchanged guards the legacy less behavior
// while we share splitTerminalStage with chart.
func TestSplitTerminalStageLess(t *testing.T) {
	weft, tokens := splitTerminalStage(`cat /log/api.log | less`)
	if weft != "cat /log/api.log" || len(tokens) == 0 || tokens[0] != "less" {
		t.Errorf("less split: weft=%q tokens=%v", weft, tokens)
	}
	// Public form preserves the string-only contract.
	weftPub, term := SplitTerminalStage(`cat /log/api.log | less`)
	if weftPub != "cat /log/api.log" || term != "less" {
		t.Errorf("public SplitTerminalStage: weft=%q term=%q", weftPub, term)
	}
}
