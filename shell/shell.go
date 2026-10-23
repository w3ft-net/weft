package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/warp/pkg/syntax"
	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/internal/timerange"
	"github.com/graybeardcorp/weft/theme"
	"github.com/graybeardcorp/weft/tracecmd"
)

// Row is a single result row consumed by the shell. Carries only
// Raw — historically this was *pb.ResultRow, but the shell never
// needed more than the rendered line, and depending on the proto
// type pulled grpc+protobuf into the WASM build.
type Row struct {
	Raw string
}

// QueryResult is the output of a query — either result rows or text,
// optionally accompanied by per-user one-time notices that the
// runner prepends to the displayed output.
type QueryResult struct {
	Rows    []Row
	Text    string
	Notices []QueryResultNotice
	// FinalDirStack is the directory stack (index 0 == cwd) the server
	// reports after running the pipeline — the "ferry back" half of the
	// dirstack courier. The interactive runner caches it via
	// core.SetDirStack so the prompt and the next request track the cwd a
	// shipped `cd`/`pushd` established. Nil when the server returned no
	// dirstack (e.g. the in-process dev shell, or a follow stream).
	FinalDirStack []string
}

// QueryResultNotice is the shell-package mirror of the wire Notice.
// Lives here (not imported from `shell/http_remote.go`) so the
// in-process dev shell and the remote HTTP shell can populate the
// same field shape.
type QueryResultNotice struct {
	Key   string
	Kind  string
	Title string
	Body  string
}

// QueryFunc is the function signature the shell uses to execute queries.
// rangeSpec is the un-resolved user-typed window ("1d..", "30m..", or an ISO
// pair). The API server resolves it on every request against server-side
// time.Now() — clients deliberately do NOT pre-resolve, so a long-lived
// shell pane never queries against a stale window.
//
// dirStack is the client's cached directory stack (index 0 == cwd) shipped
// with the pipeline so the server resolves cd/relative paths against the
// caller's live directory; the post-run stack returns on
// QueryResult.FinalDirStack. Pass nil for a fresh shell rooted at "/".
type QueryFunc func(pipeline, rangeSpec string, dirStack []string) (*QueryResult, error)

// FollowFunc is the function signature for tail -f streaming.
type FollowFunc func(ctx context.Context, pipeline, rangeSpec string, onRow func(Row)) error

// Shell is the weft interactive shell.
type Shell struct {
	Query  QueryFunc
	Follow FollowFunc
	// Range is the current un-resolved range spec. Defaults to "1d..".
	// Updated by the `range <spec>` builtin and by --range one-shot flags.
	// Shipped verbatim to the API server, which resolves against its clock.
	Range string
	Apps  []string
	FS    shellcore.FS
	// Chart, when non-nil, takes over both `chart <name>` and
	// `... | chart` instead of registering chart's ASCII renderers.
	// The browser shell uses this to open a React modal that re-uses
	// the dashboard chart-tile component, rather than rasterising into
	// xterm scrollback. When nil, ConfigureCore falls back to
	// chart.Register against the FS's WatchResultsProvider — the CLI
	// path. See ChartRequest for the payload shape.
	Chart func(ChartRequest)
	// Dashboard, when non-nil, takes over the `dashboard` builtin and
	// is invoked with the requested dashboard name (empty string for
	// bare `dashboard`). The browser shell uses this to navigate the
	// React app to /dashboards/<name> instead of rendering an
	// in-terminal view. When nil, ConfigureCore falls back to
	// dashboard.Register if the FS satisfies dashboard.Provider —
	// that's the CLI path.
	Dashboard func(name string)
	// Trace, when non-nil, takes over `trace` (the recent-traces list),
	// `trace --id <id>`, `trace --spans --id <id>`, and `trace --spans
	// [--name <span>]` instead of registering spanview's in-terminal View. The browser shell uses this to open
	// a React modal (list/samples, mirroring spanview's own UX)
	// instead of rendering ANSI into xterm scrollback. When nil,
	// ConfigureCore leaves `trace` unregistered — cmd/weft/runner
	// registers spanview.Register (interactive REPL) or callers fall
	// back to RenderTraceIfApplicable/RenderSpanRollupIfApplicable
	// (one-shot mode) instead. See TraceRequest for the payload shape.
	Trace func(TraceRequest)
	// OnRangeChange, when non-nil, is invoked by the `range <spec>`
	// builtin with the new un-resolved spec after s.Range is updated.
	// shellcore no longer carries range state (warp removed
	// SetRange/ActionSetRange in favor of an opaque prompt
	// SetLeadSegment), so embedders that mirror the window elsewhere
	// ship it out-of-band through this hook. The browser bridge uses it
	// to drive the JS onRangeChange chip callback; the CLI leaves it nil
	// (the inline prompt segment is the only surface it needs).
	OnRangeChange func(spec string)
	User          string // username shown in prompt (e.g. "alice")
	Customer      string // customer/team shown after the @ in the prompt (e.g. "acme"); empty falls back to "weft"
	// TermWidth is the caller's known terminal width, used by
	// ExecNonInteractive's call into RenderTraceIfApplicable to size
	// `trace <id>`'s 3-column layout — the coordinator has no way to know
	// this, so it's the client's job to supply it before calling Query.
	// The interactive REPL doesn't read this field; it passes its own
	// live, resize-aware width (shellcore.ShellCore.Width()) straight to
	// RenderTraceIfApplicable instead. 0 (the zero value, e.g. stdout
	// isn't a real terminal) falls back to 80, matching pipe-stage
	// chart's existing non-interactive default.
	TermWidth int
}

// ConfigureCore wires every embedder-owned surface weft installs on a
// fresh shellcore — syntax fallback, FS + chart/dashboard hookups,
// user/customer prompt, range, help corpus, git subcommand grammar,
// reserved names, `range` builtin, and the vim post-save popup. The
// CLI Run() and the browser cmd/wasm bridge both go through this
// single entry point. History persistence is intentionally NOT set
// here: CLI installs a file-backed store; wasm.Boot installs a
// JS-callback-backed store before this runs.
func (s *Shell) ConfigureCore(core *shellcore.ShellCore) {
	core.SetSyntaxFallback(syntax.LangBash)
	// Theme syntax highlighting off the startup logo's thread colors
	// (see weftPalette). Without this the shell renders warp's
	// DefaultPalette, which doesn't share weft's blue/gold weave.
	core.SetPalette(weftPalette())
	core.SetSpinnerStyle(weftSpinnerStyle())
	if s.FS != nil {
		core.SetFS(s.FS)
	}
	// Dashboard + chart fallback registration (the CLI path that
	// rasterises into scrollback) lives in cmd/weft/runner because
	// it imports the heavy `dashboard` and full `chart` packages —
	// pulling those into the shell/ import graph adds multi-MB to
	// the WASM build for code the browser never runs. Browser-side
	// callback hookup stays here so a single Shell{} + ConfigureCore
	// call still wires the full surface.
	if s.Dashboard != nil {
		core.RegisterBuiltin("dashboard", dashboardNavigateBuiltin(s.Dashboard))
	}
	if s.Chart != nil {
		core.RegisterBuiltin("chart", s.chartRequestBuiltin(s.Chart))
		core.RegisterPipeStage("chart", chart.ParsePipeArgs, s.chartRequestPipe(s.Chart))
	}
	if s.Trace != nil {
		core.RegisterBuiltin("trace", traceRequestBuiltin(s.Trace))
	}
	if s.User != "" {
		core.SetUser(s.User)
	}
	customer := s.Customer
	if customer == "" {
		customer = "weft"
	}
	core.SetCustomer(customer)
	// Resolve once for the PS1 duration label. The server is what actually
	// runs the query against an up-to-date "now"; this snapshot just exists
	// so the prompt can render `[1d]` (formatDuration(end-start)) — that
	// duration is stable for both relative and absolute specs, so a stale
	// snapshot is harmless for display purposes.
	if seg := rangeLeadSegment(s.Range); seg != "" {
		core.SetLeadSegment(seg)
	}
	core.SetHelp(weftHelp{tiers: defaultTierLimits{}})
	// No client-side path rewriter anymore: the shell ships every line
	// verbatim and the server (a real pkg/bash interpreter) resolves
	// paths, so `git log -- <path>` reaches it with the `log` subcommand
	// keyword intact — no grammar registration needed.
	// Reserve weft's natively-implemented command names so a saved
	// /bin/<name> function can't shadow them. These run through the
	// bash interpreter's RegisterCommand path (see
	// coordinator/weftcmds.go), not shellcore's RegisterBuiltin.
	for _, name := range []string{
		"alert", "git", "explain",
		"weft.latest", "weft.avg", "weft.max", "weft.min",
		"weft.sum", "weft.stddev", "weft.first", "weft.count",
		"weft.percentile", "weft.delta", "weft.slope", "weft.rate",
	} {
		core.RegisterReservedName(name)
	}
	// `range` was a shellcore-inline builtin; it's a weft concept
	// (the query time window) and now lives here. The handler
	// covers both bare `range` (display current) and
	// `range <spec>` (set new) — same UX warp's inline version
	// provided.
	core.RegisterBuiltin("range", rangeBuiltinFor(s))
	// Post-save commit-message popup for /bin/<name> and
	// /cron.d/<name>. Pre-A.4.5 this lived in shellcore as the
	// default OnQuit branch; now installed by weft as embedder
	// VimHooks via StartVimBuffer.
	core.SetVimHooks(installVimPopupHooks(core))
}

// ChartRequest is the payload Shell.Chart receives when the browser
// shell intercepts a `chart` invocation. Plot is always set; Watch
// names the cron for the standalone `chart <name>` form. The remaining
// fields carry the parsed render options so the modal can fully
// reproduce what the CLI would have drawn to scrollback.
type ChartRequest struct {
	// Pipeline is the line the user typed to open the chart, so the surface
	// that renders it can report itself closed against that command.
	Pipeline string
	// Watch is the cron name when the user typed `chart <name>`, so the
	// modal can link to it. Empty when the request originates from
	// `... | chart`.
	Watch string
	// Plot is the grouped, bucketed and aggregated data, reduced per the
	// flags the user typed: from the upstream lines for `... | chart`, from
	// the cron's stored series over the shell's range for `chart <name>`.
	// The browser modal draws it directly.
	Plot *chart.Plot
	// Kind is line / bar / sparkline / area / table.
	Kind string
	// Title is the user-supplied title (--title).
	Title string
}

// chartRequestBuiltin returns a `chart` builtin that reads the cron's
// series over the shell's range, reduces it with chart.CronPlot (the same
// reduction the CLI draws) and hands the plot to the embedder callback.
// Used by the browser shell; the CLI registers chart.Builtin (ASCII
// renderer) instead.
func (s *Shell) chartRequestBuiltin(emit func(ChartRequest)) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 0 || args[0] != "chart" {
			return false
		}
		parsed, errMsg := chart.ParseWatchCmd(args[1:])
		if errMsg != "" {
			c.AppendScrollback(shellcore.ErrorText(errMsg) + "\n")
			return true
		}
		if parsed.Follow {
			c.AppendScrollback(shellcore.ErrorText("chart: --follow is not supported in the browser; open the cron page instead") + "\n")
			return true
		}
		src, ok := s.FS.(chart.CronSource)
		if !ok {
			c.AppendScrollback(shellcore.ErrorText("chart: cron results not supported in this backend") + "\n")
			return true
		}
		series, err := src.GetCronSeries(parsed.Name, s.Range)
		if err != nil {
			c.AppendScrollback(shellcore.ErrorText("chart: "+err.Error()) + "\n")
			return true
		}
		plot, msg := chart.CronPlot(series, parsed)
		if msg == "" && len(plot.Series) == 0 {
			msg = fmt.Sprintf("no numeric results for cron %q in this range", parsed.Name)
		}
		if msg != "" {
			c.AppendScrollback(shellcore.ErrorText("chart: "+msg) + "\n")
			return true
		}
		emit(ChartRequest{
			Pipeline: c.RunningPipeline(),
			Watch:    parsed.Name,
			Plot:     &plot,
			Kind:     parsed.Kind,
			Title:    parsed.Title,
		})
		return true
	}
}

// stderrLineMarker opens a line of upstream stderr: the browser's exec path
// merges a pipeline's stderr into its lines as red text (network.ts).
const stderrLineMarker = "\x1b[91m"

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// splitDiagnostics separates a pipeline's data rows from the stderr lines
// merged among them, returning the diagnostics as plain text.
func splitDiagnostics(lines []string) (rows, diagnostics []string) {
	for _, l := range lines {
		if strings.HasPrefix(l, stderrLineMarker) {
			diagnostics = append(diagnostics, ansiSeq.ReplaceAllString(l, ""))
			continue
		}
		rows = append(rows, l)
	}
	return rows, diagnostics
}

// chartRequestPipe returns a PipeStageRenderFunc that hands the
// upstream pipeline snapshot to the embedder callback and writes a
// terse placeholder to scrollback (so the user sees that something
// happened — the chart itself appears in a separate React modal).
func (s *Shell) chartRequestPipe(emit func(ChartRequest)) shellcore.PipeStageRenderFunc {
	return func(c *shellcore.ShellCore, lines []string, parsed any) (string, string) {
		args, ok := parsed.(chart.PipeArgs)
		if !ok {
			return "", "chart: internal error: parsed args not PipeArgs"
		}
		rows, diagnostics := splitDiagnostics(lines)
		if len(rows) == 0 && len(diagnostics) > 0 {
			// The upstream pipeline failed; its error is the news, not a
			// row to parse.
			return "", "chart: " + strings.Join(diagnostics, "; ")
		}
		lines = rows
		plot, errMsg := chart.PlotFromStream(lines, args.WithRange(s.Range))
		if errMsg != "" {
			return "", "chart: " + errMsg
		}
		emit(ChartRequest{
			Pipeline: c.RunningPipeline(),
			Plot:     &plot,
			Kind:     args.Kind,
			Title:    args.Title,
		})
		return shellcore.DimText("[chart] opened in modal ("+strconv.Itoa(len(lines))+" rows)") + "\n", ""
	}
}

// dashboardNavigateBuiltin returns a `dashboard` builtin that forwards
// the requested name to a Go-side callback rather than rendering a
// shellcore View. The browser embedder uses this so `dashboard prod`
// at the prompt navigates the React app to /dashboards/prod. Bare
// `dashboard` passes "" — the embedder decides whether that means
// "go to the index" or "show usage".
func dashboardNavigateBuiltin(navigate func(name string)) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 0 || args[0] != "dashboard" {
			return false
		}
		name := ""
		for _, a := range args[1:] {
			switch {
			case strings.HasPrefix(a, "-"):
				c.AppendScrollback(shellcore.ErrorText("dashboard: unknown option "+a) + "\n")
				return true
			case name != "":
				c.AppendScrollback(shellcore.ErrorText("dashboard: unexpected argument "+a) + "\n")
				return true
			default:
				name = a
			}
		}
		navigate(name)
		return true
	}
}

// TraceRequest is the payload Shell.Trace receives when the browser
// shell intercepts a `trace` invocation. Mode names the view the browser
// opens, one per shape of the command (see the tracecmd package):
//
//	"list"      a bare `trace`: the recent traces
//	"gap"       `trace --id <id>`: one request's lines, as the gap-histogram
//	            RenderTraceIfApplicable renders for the CLI
//	"rollup"    `trace --spans --id <id>`: one request's span breakdown, with
//	            the self-time column
//	"aggregate" `trace --spans [--name <span>]`: operations across requests,
//	            with the percentile spread instead (see
//	            theme.SpanRollupColumns' singleTrace doc comment for why the
//	            two span views render different columns)
type TraceRequest struct {
	// Pipeline is the full "trace ..." pipeline to (re-)run — the
	// browser modal fetches its own rows via the shell's existing
	// async query path rather than being handed pre-fetched Lines,
	// since the pipe-stage precedent (ChartRequest.Plot) doesn't
	// apply: `trace` is never a pipe-stage destination, always the
	// pipeline's own source command.
	Pipeline string
	// Title is the status-bar text: see tracecmd.Cmd.Title.
	Title string
	// Mode is "list", "gap", "rollup", or "aggregate".
	Mode string
}

// traceRequestBuiltin returns a `trace` builtin that forwards the
// parsed request to the embedder callback instead of falling through to
// normal pipeline dispatch (which the CLI's one-shot mode instead
// recognizes after the fact, see RenderTraceIfApplicable). A line that
// is not exactly one `trace` command — a pipe or redirect after it, a
// grammar error, or the `--samples` view the browser has no modal for —
// returns false and dispatches like any other pipeline.
func traceRequestBuiltin(emit func(TraceRequest)) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 0 || args[0] != "trace" {
			return false
		}
		cmd, err := tracecmd.Parse(args[1:])
		if err != nil {
			return false
		}
		var mode string
		switch cmd.Mode() {
		case tracecmd.ModeList:
			mode = "list"
		case tracecmd.ModeLines:
			mode = "gap"
		case tracecmd.ModeBreakdown:
			mode = "rollup"
		case tracecmd.ModeSpans:
			mode = "aggregate"
		default:
			return false
		}
		emit(TraceRequest{Pipeline: cmd.String(), Title: cmd.Title(), Mode: mode})
		return true
	}
}

// rangeBuiltinFor returns a `range` builtin closure bound to a specific
// Shell. The closure stores the un-resolved spec on s.Range so
// subsequent queries ship it verbatim to the server; it also refreshes
// the prompt's opaque lead segment (warp's SetLeadSegment seam, which
// replaced the removed SetRange) and fires s.OnRangeChange so embedders
// that mirror the window out-of-band (the browser chip) stay in sync.
// Range state lives on the Shell now, not in shellcore — bare `range`
// reads it back by re-parsing s.Range.
func rangeBuiltinFor(s *Shell) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 1 {
			start, end, err := timerange.ParseRange(s.Range)
			if err != nil {
				c.AppendScrollback(shellcore.ErrorText("range: "+err.Error()) + "\n")
				return true
			}
			c.AppendScrollback(FormatRange(start, end))
			return true
		}
		if len(args) >= 2 {
			spec := strings.Join(args[1:], " ")
			start, end, err := timerange.ParseRange(spec)
			if err != nil {
				c.AppendScrollback(shellcore.ErrorText("range: "+err.Error()) + "\n")
				return true
			}
			s.Range = spec
			c.SetLeadSegment(rangeLeadSegment(spec))
			if s.OnRangeChange != nil {
				s.OnRangeChange(spec)
			}
			c.AppendScrollback(FormatRange(start, end))
			return true
		}
		return false
	}
}

// applyRangeLine handles a bare `range` or `range <spec>` input
// against the Shell's stored spec — same surface as rangeBuiltinFor
// but for the one-shot, non-interactive path that doesn't drive
// through shellcore. Returns (handled, output, err).
//   - handled=false: line wasn't a range command; caller continues.
//   - handled=true, err!=nil: caller returns the err (s.Range untouched).
//   - handled=true, err==nil: caller writes output and returns.
func (s *Shell) applyRangeLine(line string) (handled bool, output string, err error) {
	if line == "range" {
		start, end, perr := timerange.ParseRange(s.Range)
		if perr != nil {
			return true, "", perr
		}
		return true, timerange.FormatRangePlain(start, end), nil
	}
	if !strings.HasPrefix(line, "range ") {
		return false, "", nil
	}
	spec := strings.TrimPrefix(line, "range ")
	start, end, perr := timerange.ParseRange(spec)
	if perr != nil {
		return true, "", perr
	}
	s.Range = spec
	return true, timerange.FormatRangePlain(start, end), nil
}

func (s *Shell) ExecNonInteractive(line string, w io.Writer) error {
	line = strings.TrimSpace(line)

	if handled, out, err := s.applyRangeLine(line); handled {
		if err != nil {
			return err
		}
		fmt.Fprint(w, out)
		return nil
	}

	// FS builtins answered client-side.
	if s.FS != nil {
		if handled, output, err := s.execFSBuiltin(line); handled {
			if err != nil {
				return err
			}
			if output != "" {
				fmt.Fprint(w, output)
			}
			return nil
		}
	}

	// Query pipeline. Strip a pager suffix or pipe-stage chart —
	// one-shot mode has no screen to page or live-redraw against,
	// but pipe-stage chart renders to stdout once and exits, so it
	// is supported.
	weftPipeline, chartArgs, errMsg := chart.SplitPipeline(line)
	if errMsg != "" {
		return fmt.Errorf("chart: %s", errMsg)
	}
	if chartArgs == nil {
		weftPipeline, _ = shellcore.SplitTerminalStage(line)
	}

	// The server (a real pkg/bash interpreter) owns cd/chain/relative-path
	// resolution now, so the pipeline ships verbatim — no client-side
	// RewritePaths/classifier pass. One-shot mode has no persistent cwd to
	// ferry; an absolute `cd` inside a chain (`cd /log/acme && cat
	// api.log`) still resolves correctly server-side. nil dirstack ==
	// fresh shell rooted at "/".
	result, err := s.Query(weftPipeline, s.Range, nil)
	if err != nil {
		return err
	}
	if chartArgs != nil {
		lines := queryResultLines(result)
		out, errMsg := chart.RenderFromStream(lines, chartArgs.WithRange(s.Range), 80, 20)
		if errMsg != "" {
			return fmt.Errorf("chart: %s", errMsg)
		}
		fmt.Fprint(w, out)
		return nil
	}
	if result.Text != "" {
		fmt.Fprint(w, result.Text)
	} else if rendered, ok := RenderSpanRollupIfApplicable(weftPipeline, result, s.TermWidth); ok {
		for _, l := range rendered {
			fmt.Fprintln(w, l)
		}
	} else if rendered, ok := RenderTraceIfApplicable(weftPipeline, result, s.TermWidth); ok {
		for _, l := range rendered {
			fmt.Fprintln(w, l)
		}
	} else {
		for _, row := range result.Rows {
			fmt.Fprintln(w, row.Raw)
		}
	}
	return nil
}

// RenderTraceIfApplicable renders result's rows when pipeline is exactly
// one `trace` command whose output has a table: the recent-traces list, or
// one request's lines as a 3-column gap-histogram display. It does nothing
// when the result has already come back pre-rendered as Text, and nothing for
// a trace piped into further stages (those rows are the user's to shape) or
// for the span views, which RenderSpanRollupIfApplicable renders. This is the
// one place that decision gets made — every client entry point
// (ExecNonInteractive here, and cmd/weft/runner's interactive REPL) calls it
// rather than each re-deriving its own copy, which is how the REPL ended up
// missing this rendering entirely the first time around. termWidth <= 0
// falls back to 80, matching pipe-stage chart's existing non-interactive
// default. Returns (nil, false) when there is nothing to render, so the
// caller falls through to its normal Text/Rows handling unchanged.
func RenderTraceIfApplicable(pipeline string, result *QueryResult, termWidth int) ([]string, bool) {
	if result == nil || result.Text != "" {
		return nil, false
	}
	cmd, ok := tracecmd.ParseLine(pipeline)
	if !ok || (cmd.Mode() != tracecmd.ModeLines && cmd.Mode() != tracecmd.ModeList) {
		return nil, false
	}
	if termWidth <= 0 {
		termWidth = 80
	}
	lines := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		lines[i] = row.Raw
	}
	if cmd.Mode() == tracecmd.ModeList {
		return theme.RenderTraceList(time.Now(), lines, termWidth), true
	}
	return theme.RenderTraceColumns(lines, termWidth), true
}

// RenderSpanRollupIfApplicable renders result's rows into theme's flat
// span-rollup table when pipeline is `trace --spans --id <id>` (one
// request's breakdown, with the self-time column) or `trace --spans
// [--name <span>]` (operations across requests, with the percentile
// spread) and the result hasn't already come back pre-rendered as Text.
// Both share one parser/renderer — theme.RenderSpanRollup doesn't care whether
// the rows it's handed came from one trace or many — but only the first gets
// the SELF column (see theme.SpanRollupColumns' singleTrace doc comment). The
// interactive REPL instead enters spanview's SpanView for the same query
// (see cmd/weft/runner/runner.go), so this function is only wired into
// ExecNonInteractive, not the REPL's runQuery. termWidth <= 0 falls back to
// 80, matching RenderTraceIfApplicable. Returns (nil, false) when pipeline
// isn't one of those, so the caller falls through unchanged.
func RenderSpanRollupIfApplicable(pipeline string, result *QueryResult, termWidth int) ([]string, bool) {
	if result == nil || result.Text != "" {
		return nil, false
	}
	cmd, ok := tracecmd.ParseLine(pipeline)
	if !ok || (cmd.Mode() != tracecmd.ModeBreakdown && cmd.Mode() != tracecmd.ModeSpans) {
		return nil, false
	}
	if termWidth <= 0 {
		termWidth = 80
	}
	lines := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		lines[i] = row.Raw
	}
	if cmd.Mode() == tracecmd.ModeSpans {
		return theme.RenderSpanRollupTable(theme.RenderSpanAggregate(lines), termWidth, false), true
	}
	return theme.RenderSpanRollupTable(theme.RenderSpanRollup(lines), termWidth, true), true
}

// queryResultLines flattens a *QueryResult into one trimmed input line
// per upstream output row, dropping blanks. Used by the non-interactive
// pipe-stage chart path.
func queryResultLines(r *QueryResult) []string {
	if r == nil {
		return nil
	}
	if r.Text != "" {
		raw := strings.Split(r.Text, "\n")
		out := make([]string, 0, len(raw))
		for _, ln := range raw {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			out = append(out, ln)
		}
		return out
	}
	out := make([]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		if strings.TrimSpace(row.Raw) == "" {
			continue
		}
		out = append(out, row.Raw)
	}
	return out
}

// execFSBuiltin handles the FS-backed builtins the one-shot path answers
// client-side. ls and cat are not among them: they ship to the server like
// any other line, whose interpreter parses the whole line (pipes, several
// paths, flags, files as well as directories). Returns (handled, output, err).
func (s *Shell) execFSBuiltin(line string) (bool, string, error) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false, "", nil
	}
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "chart":
		// Non-interactive chart: render a single frame and return.
		// `--follow` is meaningless here (no terminal to redraw into),
		// so we ignore the flag and just paint the latest frame.
		src, ok := s.FS.(chart.CronSource)
		if !ok {
			return true, "chart: cron results not supported in this backend\n", nil
		}
		// Same grammar as the interactive form. --follow and -i are
		// accepted but meaningless here (no terminal to redraw into), so
		// the latest frame is painted once.
		parsed, perr := chart.ParseWatchCmd(args)
		if perr != "" {
			return true, "", errors.New(perr)
		}
		out, errMsg := chart.RenderOneShot(src, s.Range, parsed, 80, 20)
		if errMsg != "" {
			return true, "chart: " + errMsg + "\n", nil
		}
		return true, out, nil
	}

	return false, "", nil
}
