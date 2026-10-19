package shell

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/warp/pkg/syntax"
	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/internal/timerange"
	"github.com/graybeardcorp/weft/spanview"
	"github.com/graybeardcorp/weft/theme"
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
	// Trace, when non-nil, takes over `trace <id>`, `trace <id>
	// --spans`, and `trace spans [name]` instead of registering
	// spanview's in-terminal View. The browser shell uses this to open
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
		core.RegisterBuiltin("chart", chartRequestBuiltin(s.Chart))
		core.RegisterPipeStage("chart", chart.ParsePipeArgs, chartRequestPipe(s.Chart))
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
// shell intercepts a `chart` invocation. Exactly one of Watch or
// Lines is set — Watch for the standalone `chart <name>` form, Lines
// for the pipe-stage `... | chart` form. The remaining fields carry
// the parsed render options so the modal can fully reproduce what
// the CLI would have drawn to scrollback.
type ChartRequest struct {
	// Pipeline is the line the user typed to open the chart, so the surface
	// that renders it can report itself closed against that command.
	Pipeline string
	// Watch is the watch name when the user typed `chart <name>`.
	// Empty when the request originates from `... | chart`.
	Watch string
	// Plot is the grouped, bucketed and aggregated data for `... | chart`,
	// reduced per the flags the user typed. The browser modal draws it
	// directly; nil for the watch form.
	Plot *chart.Plot
	// Kind is line / bar / sparkline / area / table.
	Kind string
	// Title is the user-supplied title for pipe form (--title);
	// empty for watch form.
	Title string
	// Limit is the watch-history depth for the watch form
	// (`chart <name> -n N`); ignored for pipe form.
	Limit int
}

// chartRequestBuiltin returns a `chart` builtin that forwards the
// parsed watch args to the embedder callback. Used by the browser
// shell; the CLI registers chart.Builtin (ASCII renderer) instead.
func chartRequestBuiltin(emit func(ChartRequest)) shellcore.BuiltinFunc {
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
		emit(ChartRequest{
			Pipeline: c.RunningPipeline(),
			Watch:    parsed.Name,
			Kind:     parsed.Kind,
			Limit:    parsed.Limit,
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
func chartRequestPipe(emit func(ChartRequest)) shellcore.PipeStageRenderFunc {
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
		plot, errMsg := chart.PlotFromStream(lines, args)
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
		if len(args) >= 2 {
			name = args[1]
		}
		navigate(name)
		return true
	}
}

// TraceRequest is the payload Shell.Trace receives when the browser
// shell intercepts a `trace` invocation. Mode distinguishes plain
// `trace <id>` ("gap", the gap-histogram RenderTraceIfApplicable
// renders for the CLI) from `trace <id> --spans` ("rollup", Phase 1's
// single-trace span rollup, which gets the SELF/TOTAL column) from
// `trace spans [name]` ("aggregate", Phase 2's cross-trace
// aggregation, which gets the full percentile spread instead — see
// theme.SpanRollupColumns' singleTrace doc comment for why the two
// phases render different columns).
type TraceRequest struct {
	// Pipeline is the full "trace ..." pipeline to (re-)run — the
	// browser modal fetches its own rows via the shell's existing
	// async query path rather than being handed pre-fetched Lines,
	// since the pipe-stage precedent (ChartRequest.Plot) doesn't
	// apply: `trace` is never a pipe-stage destination, always the
	// pipeline's own source command.
	Pipeline string
	// Title is the trace ID (gap/rollup) or span name/"all spans"
	// (aggregate) — the same status-bar text spanview.ListArgs.Title
	// already carries for the CLI's interactive View.
	Title string
	// Mode is "gap", "rollup", or "aggregate".
	Mode string
}

// traceRequestBuiltin returns a `trace` builtin that forwards the
// parsed request to the embedder callback instead of entering
// spanview's in-terminal View or falling through to normal pipeline
// dispatch (which the CLI's one-shot mode instead recognizes after
// the fact via isTracePipeline/isSpansPipeline). It reuses
// spanview.ParseSpansLine for the rollup/aggregate shapes — the exact
// arg grammar spanview.Builtin itself dispatches on — rather than
// forking that parsing, and adds one predicate of its own for the
// plain `trace <id>` shape, which ParseSpansLine deliberately leaves
// unmatched (spanview.Builtin lets that shape fall through to normal
// dispatch since the CLI renders it via RenderTraceIfApplicable, not
// a View).
func traceRequestBuiltin(emit func(TraceRequest)) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 0 || args[0] != "trace" {
			return false
		}
		if listArgs, ok := spanview.ParseSpansLine(args); ok {
			mode := "rollup"
			if len(args) > 1 && args[1] == "spans" {
				mode = "aggregate"
			}
			emit(TraceRequest{Pipeline: listArgs.Pipeline, Title: listArgs.Title, Mode: mode})
			return true
		}
		// Plain `trace <id>`: exactly one non-flag argument and it
		// isn't the "spans" sub-verb (which ParseSpansLine already
		// handles, including its own arity failures like `trace
		// spans a b`  — falling through here for those would wrongly
		// treat "spans" as a trace id).
		if len(args) == 2 && args[1] != "spans" {
			traceID := args[1]
			emit(TraceRequest{Pipeline: "trace " + traceID, Title: traceID, Mode: "gap"})
			return true
		}
		return false
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
		if handled, output := s.execFSBuiltin(line); handled {
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
		out, errMsg := chart.RenderFromStream(lines, *chartArgs, 80, 20)
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

// isTracePipeline reports whether pipeline's source command is the bare
// `trace <id>` verb — the shape emitTraceTagged's delimited output is
// meant for. A trace piped into further stages (`trace <id> | grep ...`)
// never reaches this path at all (it has compute stages, so the
// coordinator's zero-stage traceTag branch that emits the delimited
// format doesn't fire for it), but checking here too means a client-side
// bug in that routing fails safe — plain rows, not a garbled column
// layout — rather than silently reformatting something that was never
// meant for it.
func isTracePipeline(pipeline string) bool {
	fields := strings.Fields(pipeline)
	return len(fields) > 0 && fields[0] == "trace"
}

// RenderTraceIfApplicable renders result's rows into theme's 3-column
// gap-histogram display when pipeline is a bare `trace <id>` query and the
// result hasn't already come back pre-rendered as Text. This is the one
// place that decision gets made — every client entry point (ExecNonInteractive
// here, and cmd/weft/runner's interactive REPL) calls it rather than each
// re-deriving its own copy, which is how the REPL ended up missing this
// rendering entirely the first time around. termWidth <= 0 falls back to 80,
// matching pipe-stage chart's existing non-interactive default. Returns
// (nil, false) when pipeline isn't a trace query, so the caller falls
// through to its normal Text/Rows handling unchanged.
func RenderTraceIfApplicable(pipeline string, result *QueryResult, termWidth int) ([]string, bool) {
	if result == nil || result.Text != "" || !isTracePipeline(pipeline) {
		return nil, false
	}
	if termWidth <= 0 {
		termWidth = 80
	}
	lines := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		lines[i] = row.Raw
	}
	return theme.RenderTraceColumns(lines, termWidth), true
}

// isSpansPipeline reports whether pipeline is a bare `trace <id>
// --spans` query (Phase 1; the flag can appear anywhere among the
// arguments — see planner/parse.go's case "trace") or a `trace spans
// [name]` query (Phase 2's cross-trace aggregation). Checked
// separately from, and before, isTracePipeline: every one of these is
// also a trace query, but only the span rollup should render for it.
func isSpansPipeline(pipeline string) bool {
	fields := strings.Fields(pipeline)
	if len(fields) == 0 || fields[0] != "trace" {
		return false
	}
	if len(fields) > 1 && fields[1] == "spans" {
		return true
	}
	for _, f := range fields[1:] {
		if f == "--spans" {
			return true
		}
	}
	return false
}

// isSpansAggregatePipeline reports whether pipeline is specifically a
// `trace spans [name]` query (Phase 2) as opposed to `trace <id>
// --spans` (Phase 1). Callers already know it's some spans pipeline
// (isSpansPipeline) before checking this; it exists to gate
// self-time — see theme.SpanRollupColumns' singleTrace: Phase 1 fetches a
// complete single trace so self-time is accurate, Phase 2 only fetches
// rows matching one span name so it can't see cross-name children at
// all and would understate delegation while looking complete.
func isSpansAggregatePipeline(pipeline string) bool {
	fields := strings.Fields(pipeline)
	return len(fields) > 1 && fields[0] == "trace" && fields[1] == "spans"
}

// RenderSpanRollupIfApplicable renders result's rows into theme's flat
// span-rollup table when pipeline is a `trace <id> --spans` (Phase 1)
// or `trace spans [name]` (Phase 2) query and the result hasn't
// already come back pre-rendered as Text. Both phases share one
// parser/renderer — theme.RenderSpanRollup doesn't care whether the
// rows it's handed came from one trace or many — but only Phase 1 gets
// the SELF column (see isSpansAggregatePipeline and
// theme.SpanRollupColumns' singleTrace doc comment). The interactive REPL
// instead enters spanview's SpanView for the same query (see
// cmd/weft/runner/runner.go), so this function is only wired into
// ExecNonInteractive, not the REPL's runQuery. termWidth <= 0 falls
// back to 80, matching RenderTraceIfApplicable. Returns (nil, false)
// when pipeline isn't a spans query, so the caller falls through to
// its normal trace/Text/Rows handling unchanged.
func RenderSpanRollupIfApplicable(pipeline string, result *QueryResult, termWidth int) ([]string, bool) {
	if result == nil || result.Text != "" || !isSpansPipeline(pipeline) {
		return nil, false
	}
	if termWidth <= 0 {
		termWidth = 80
	}
	lines := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		lines[i] = row.Raw
	}
	singleTrace := !isSpansAggregatePipeline(pipeline)
	return theme.RenderSpanRollupTable(theme.RenderSpanRollup(lines), termWidth, singleTrace), true
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
// paths, flags, files as well as directories). Returns (handled, output).
func (s *Shell) execFSBuiltin(line string) (bool, string) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false, ""
	}
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "chart":
		// Non-interactive chart: render a single frame and return.
		// `--follow` is meaningless here (no terminal to redraw into),
		// so we ignore the flag and just paint the latest frame.
		wr, ok := s.FS.(chart.WatchResultsProvider)
		if !ok {
			return true, "chart: cron results not supported in this backend\n"
		}
		// Parse a minimal subset of flags: the watch name, -t, -n.
		var name, kind string
		limit := 50
		kind = "line"
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "-t" && i+1 < len(args):
				kind = args[i+1]
				i++
			case a == "-n" && i+1 < len(args):
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					limit = n
				}
				i++
			case a == "-f" || a == "--follow" || a == "-i":
				// follow / interval ignored in one-shot mode.
				if a == "-i" && i+1 < len(args) {
					i++
				}
			case strings.HasPrefix(a, "-"):
				// Unknown flag — let it pass through silently.
			default:
				if name == "" {
					name = a
				}
			}
		}
		if name == "" {
			return true, "usage: chart <cron> [-t TYPE] [-n LIMIT]\n"
		}
		name = strings.TrimPrefix(name, "/cron.d/")
		name = strings.TrimPrefix(name, "cron.d/")
		out, errMsg := chart.RenderOneShot(wr, name, kind, limit, 80, 20)
		if errMsg != "" {
			return true, "chart: " + errMsg + "\n"
		}
		return true, out
	}

	return false, ""
}
