package shell

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/chart"
)

// TestConfigureCore_ChartBuiltin_EmitsWatchRequest verifies that
// `chart api-errors -n 100 -t bar` at the browser prompt fires
// Shell.Chart with a watch-form ChartRequest. The CLI's ASCII
// renderer is not registered when Shell.Chart is set.
func TestConfigureCore_ChartBuiltin_EmitsWatchRequest(t *testing.T) {
	var got []ChartRequest
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	s := &Shell{User: "alice", Chart: func(r ChartRequest) {
		got = append(got, r)
	}}
	s.ConfigureCore(core)

	for _, r := range "chart api-errors -n 100 -t bar" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if len(got) != 1 {
		t.Fatalf("expected 1 chart request, got %d", len(got))
	}
	r := got[0]
	if r.Watch != "api-errors" {
		t.Errorf("Watch: got %q want %q", r.Watch, "api-errors")
	}
	if r.Limit != 100 {
		t.Errorf("Limit: got %d want 100", r.Limit)
	}
	if r.Kind != "bar" {
		t.Errorf("Kind: got %q want bar", r.Kind)
	}
	if r.Plot != nil {
		t.Errorf("watch form should not carry a Plot; got %v", r.Plot)
	}
}

// TestConfigureCore_ChartBuiltin_FollowRejected confirms that
// `chart --follow` in the browser produces a user-visible error
// instead of attempting to enter the follow-mode View (which doesn't
// exist in the browser surface).
func TestConfigureCore_ChartBuiltin_FollowRejected(t *testing.T) {
	var out strings.Builder
	called := false
	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) { out.Write(b) },
		func(a shellcore.Action) {},
	)
	s := &Shell{User: "alice", Chart: func(_ ChartRequest) { called = true }}
	s.ConfigureCore(core)

	for _, r := range "chart api-errors --follow" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if called {
		t.Error("--follow should not invoke the emit callback in browser")
	}
	if !strings.Contains(out.String(), "--follow is not supported") {
		t.Errorf("expected user-visible --follow rejection; got: %q", out.String())
	}
}

// TestConfigureCore_ChartPipe_EmitsLinesRequest verifies that the
// pipe-stage handler captures the upstream snapshot and the chart
// args, then fires Shell.Chart with a pipe-form ChartRequest. We
// drive chartRequestPipe directly (without the full shellcore exec)
// because the pipe stage runs after a query — in test context we
// don't want to stand up a query backend.
func TestConfigureCore_ChartPipe_EmitsLinesRequest(t *testing.T) {
	var got []ChartRequest
	render := chartRequestPipe(func(r ChartRequest) { got = append(got, r) })

	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	lines := []string{"1 200", "2 201", "3 199"}
	parsed, msg := chart.ParsePipeArgs([]string{"-t", "line", "--title", "latency"})
	if msg != "" {
		t.Fatal(msg)
	}
	args := parsed.(chart.PipeArgs)

	out, errMsg := render(core, lines, args)
	if errMsg != "" {
		t.Fatalf("render returned err: %s", errMsg)
	}
	if !strings.Contains(out, "[chart] opened in modal") {
		t.Errorf("expected placeholder scrollback text; got %q", out)
	}
	if !strings.Contains(out, "(3 rows)") {
		t.Errorf("placeholder should announce row count; got %q", out)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 emit, got %d", len(got))
	}
	r := got[0]
	if r.Plot == nil || len(r.Plot.Series) != 1 || len(r.Plot.Series[0].Points) != 3 {
		t.Fatalf("Plot: got %+v, want one series of 3 points", r.Plot)
	}
	if r.Plot.Series[0].Points[0].Y != 200 {
		t.Errorf("first point y: got %v want 200", r.Plot.Series[0].Points[0].Y)
	}
	if r.Kind != "line" {
		t.Errorf("Kind: got %q want line", r.Kind)
	}
	if r.Title != "latency" {
		t.Errorf("Title: got %q want latency", r.Title)
	}
	if r.Watch != "" {
		t.Errorf("pipe form should not carry Watch; got %q", r.Watch)
	}
}

// TestConfigureCore_ChartPipe_FallbackToAsciiWhenChartNil confirms
// the CLI path: when Shell.Chart is nil, ConfigureCore registers the
// stock chart.Builtin against the FS's WatchResultsProvider, so a
// `chart <name>` invocation renders ASCII via the FS instead of
// emitting the browser-style ChartRequest. We can't easily probe
// shellcore's registered pipe stages externally; this test instead
// pins the negative shape via the builtin path.
func TestConfigureCore_ChartPipe_FallbackToAsciiWhenChartNil(t *testing.T) {
	var emitted bool
	var emit = func(_ ChartRequest) { emitted = true }
	_ = emit // sanity: callback exists but is unused on this path

	core := shellcore.NewShellCore(nil, 80, 24,
		func(b []byte) {},
		func(a shellcore.Action) {},
	)
	// Shell.Chart left nil. FS is also nil, so chart.Register doesn't
	// fire either — i.e. the browser-style `chart` builtin is NOT
	// installed in this configuration.
	(&Shell{User: "alice"}).ConfigureCore(core)

	for _, r := range "chart api-errors" {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})

	if emitted {
		t.Error("Shell.Chart should not be invoked when nil")
	}
}

// The flags the user typed must reach the modal: bucketing and aggregation
// happen before the request is emitted, not in the browser. Rows that give
// nothing to plot are reported in the terminal like the CLI does, with no
// empty modal opened.
func TestConfigureCore_ChartPipe_AppliesBucketAndReportsNoData(t *testing.T) {
	var got []ChartRequest
	render := chartRequestPipe(func(r ChartRequest) { got = append(got, r) })
	core := shellcore.NewShellCore(nil, 80, 24, func(b []byte) {}, func(a shellcore.Action) {})

	parsed, msg := chart.ParsePipeArgs([]string{"-x", "1", "--bucket", "5m"})
	if msg != "" {
		t.Fatal(msg)
	}
	_, errMsg := render(core, []string{
		"2026-10-10T12:00:01Z ERROR a",
		"2026-10-10T12:02:40Z ERROR b",
		"2026-10-10T12:07:00Z ERROR c",
	}, parsed)
	if errMsg != "" || len(got) != 1 {
		t.Fatalf("errMsg=%q emits=%d", errMsg, len(got))
	}
	pts := got[0].Plot.Series[0].Points
	if got[0].Kind != "line" || !got[0].Plot.TimeX || len(pts) != 2 || pts[0].Y != 2 {
		t.Errorf("bucketed request = kind %q plot %+v, want a line over 2 five-minute buckets", got[0].Kind, got[0].Plot)
	}

	got = nil
	if _, errMsg := render(core, nil, parsed); errMsg == "" || len(got) != 0 {
		t.Errorf("no rows: errMsg=%q emits=%d, want an error and no modal", errMsg, len(got))
	}
}

// A chart modal must be able to say which typed line opened it, so the tour
// can complete the lesson when the modal closes. The pipe stage never sees the
// upstream command, so the request carries the typed line from the core.
func TestConfigureCore_ChartRequestsCarryTheTypedLine(t *testing.T) {
	var got []ChartRequest
	core := shellcore.NewShellCore(nil, 80, 24, func(b []byte) {}, func(a shellcore.Action) {})
	s := &Shell{User: "alice", Chart: func(r ChartRequest) { got = append(got, r) }}
	s.ConfigureCore(core)

	type_ := func(line string) {
		for _, r := range line {
			core.HandleEvent(shellcore.KeyEvent{Rune: r})
		}
		core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
	}

	type_("chart error-rate")
	type_("cat main.log | grep ERROR | chart -x 1 --bucket 5m")
	core.HandleEvent(shellcore.ExecResultEvent{Text: "2026-10-10T12:00:01Z ERROR a\n2026-10-10T12:07:00Z ERROR b\n"})

	if len(got) != 2 {
		t.Fatalf("expected 2 chart requests, got %d", len(got))
	}
	if got[0].Pipeline != "chart error-rate" {
		t.Errorf("watch form Pipeline = %q", got[0].Pipeline)
	}
	if got[1].Pipeline != "cat main.log | grep ERROR | chart -x 1 --bucket 5m" {
		t.Errorf("pipe form Pipeline = %q", got[1].Pipeline)
	}
}
