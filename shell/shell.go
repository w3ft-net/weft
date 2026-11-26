package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	warpfs "github.com/graybeardcorp/warp/pkg/fs"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/graybeardcorp/warp/pkg/syntax"
	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/theme"
	"github.com/w3ft-net/weft/timerange"
	"github.com/w3ft-net/weft/tracecmd"
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

// QueryCtxFunc is QueryFunc for an executor that can be told to stop waiting:
// ctx is cancelled when the user presses Ctrl+C. The browser shell uses it so
// the abort reaches the fetch; the CLI's plain Query cannot be interrupted, so
// its answer is simply dropped after a cancel.
type QueryCtxFunc func(ctx context.Context, pipeline, rangeSpec string, dirStack []string) (*QueryResult, error)

// FollowFunc is the function signature for tail -f streaming.
type FollowFunc func(ctx context.Context, pipeline, rangeSpec string, onRow func(Row)) error

// Shell is the weft interactive shell.
type Shell struct {
	// Tiers is what `help cron` says about plan limits. Nil, as in a build with no
	// plans, leaves that section out.
	Tiers TierLimitsProvider
	Query QueryFunc
	// QueryCtx, when non-nil, is used instead of Query to run a typed line.
	// Either one switches the shell onto remote execution (see remote.go).
	QueryCtx QueryCtxFunc
	Follow   FollowFunc
	// Range is the current un-resolved range spec. Defaults to "1d..".
	// Updated by the `range <spec>` builtin and by --range one-shot flags.
	// Shipped verbatim to the API server, which resolves against its clock.
	Range string
	Apps  []string
	FS    warpfs.FS
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
	// live, resize-aware width (shellcore.Shell.Width()) straight to
	// RenderTraceIfApplicable instead. 0 (the zero value, e.g. stdout
	// isn't a real terminal) falls back to 80, matching pipe-stage
	// chart's existing non-interactive default.
	TermWidth int
	// claimFns decides, per client command, whether a given line is the
	// client's. A command with no entry claims every line that is just that
	// command. See claim.
	claimFns map[string]func(args []string) bool

	// Width, when non-nil, supplies the live terminal width to the remote
	// path (trace tables size to it). It is called from the run goroutine,
	// so it must be safe for that; nil falls back to TermWidth.
	Width func() int
}

// ConfigureCore wires every embedder-owned surface weft installs on a
// fresh shellcore — syntax fallback, FS + chart/dashboard hookups,
// user/customer prompt, range, help corpus, git subcommand grammar,
// reserved names, `range` builtin, and the vim post-save popup. The
// CLI Run() and the browser cmd/wasm bridge both go through this
// single entry point. History persistence is intentionally NOT set
// here: CLI installs a file-backed store; wasm.Boot installs a
// JS-callback-backed store before this runs.
func (s *Shell) ConfigureCore(core *shellcore.Shell) {
	// Theme syntax highlighting off the startup logo's thread colors
	// (see weftPalette). Without this the shell renders warp's
	// default palette, which doesn't share weft's blue/gold weave.
	cfg := core.Config()
	cfg.SyntaxFallback = syntax.LangBash
	pal := weftPalette()
	cfg.Palette = &pal
	if s.FS != nil {
		cfg.FS = s.FS
	}
	core.SetConfig(cfg)
	core.SetSpinnerStyle(weftSpinnerStyle())
	// Dashboard + chart fallback registration (the CLI path that
	// rasterises into scrollback) lives in cmd/weft/runner because
	// it imports the heavy `dashboard` and full `chart` packages —
	// pulling those into the shell/ import graph adds multi-MB to
	// the WASM build for code the browser never runs. Browser-side
	// callback hookup stays here so a single Shell{} + ConfigureCore
	// call still wires the full surface.
	if s.Dashboard != nil {
		core.RegisterCommand("dashboard", dashboardCommand(s.Dashboard))
	}
	if s.Chart != nil {
		core.RegisterCommand("chart", s.chartCommand(s.Chart))
	}
	if s.Trace != nil {
		core.RegisterCommand("trace", traceCommand(s.Trace))
		s.Claim("trace", func(args []string) bool { _, ok := traceRequest(args); return ok })
	}
	customer := s.Customer
	if customer == "" {
		customer = "weft"
	}
	// The prompt's duration label resolves the range spec for display only:
	// the server runs the query against its own clock, and the duration is
	// stable for relative and absolute specs alike.
	core.SetPrompt(weftPrompt(func() string { return rangeLeadSegment(s.Range) }, s.User, customer))
	core.SetHelp(weftHelp{tiers: s.Tiers})
	// No client-side path rewriter anymore: the shell ships every line
	// verbatim and the server (a real pkg/bash interpreter) resolves
	// paths, so `git log -- <path>` reaches it with the `log` subcommand
	// keyword intact — no grammar registration needed.
	// `range` was a shellcore-inline builtin; it's a weft concept
	// (the query time window) and now lives here. The handler
	// covers both bare `range` (display current) and
	// `range <spec>` (set new) — same UX warp's inline version
	// provided.
	core.RegisterCommand("range", rangeCommand(s))
	// Post-save commit-message popup for /bin/<name> and
	// /cron.d/<name>. Pre-A.4.5 this lived in shellcore as the
	// default OnQuit branch; now installed by weft as embedder
	// VimHooks via StartVimBuffer.
	core.SetVimHooks(installVimPopupHooks(core))
	if s.Query != nil || s.QueryCtx != nil {
		s.installRemote(core)
	}
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

// chartWatchRequest builds the request for the standalone `chart <cron>` form:
// it reads the cron's series over the shell's range and reduces it with
// chart.CronPlot (the same reduction the CLI draws). The error text is for the
// terminal and already carries the "chart: " prefix. Pipeline is left for the
// caller, which alone knows the typed line.
func (s *Shell) chartWatchRequest(args []string) (ChartRequest, string) {
	parsed, errMsg := chart.ParseWatchCmd(args)
	if errMsg != "" {
		return ChartRequest{}, errMsg
	}
	if parsed.Follow {
		return ChartRequest{}, "chart: --follow is not supported in the browser; open the cron page instead"
	}
	src, ok := s.FS.(chart.CronSource)
	if !ok {
		return ChartRequest{}, "chart: cron results not supported in this backend"
	}
	series, err := src.GetCronSeries(parsed.Name, s.Range)
	if err != nil {
		return ChartRequest{}, "chart: " + err.Error()
	}
	plot, msg := chart.CronPlot(series, parsed)
	if msg == "" && len(plot.Series) == 0 {
		msg = fmt.Sprintf("no numeric results for cron %q in this range", parsed.Name)
	}
	if msg != "" {
		return ChartRequest{}, "chart: " + msg
	}
	return ChartRequest{Watch: parsed.Name, Plot: &plot, Kind: parsed.Kind, Title: parsed.Title}, ""
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

// chartPipeRequest builds the request for the `... | chart` form from the
// upstream lines. note is the terse placeholder for the terminal (the chart
// itself opens in a separate React modal); errMsg already carries the
// "chart: " prefix.
func (s *Shell) chartPipeRequest(lines []string, args chart.PipeArgs) (req ChartRequest, note, errMsg string) {
	rows, diagnostics := splitDiagnostics(lines)
	if len(rows) == 0 && len(diagnostics) > 0 {
		// The upstream pipeline failed; its error is the news, not a row to
		// parse.
		return ChartRequest{}, "", "chart: " + strings.Join(diagnostics, "; ")
	}
	plot, msg := chart.PlotFromStream(rows, args.WithRange(s.Range))
	if msg != "" {
		return ChartRequest{}, "", "chart: " + msg
	}
	req = ChartRequest{Plot: &plot, Kind: args.Kind, Title: args.Title}
	return req, shellcore.DimText("[chart] opened in modal ("+strconv.Itoa(len(rows))+" rows)") + "\n", ""
}

// chartCommand is the `chart` command. One name, two forms, told apart by
// whether anything is piped in: alone it charts a cron (`chart error-rate`),
// after a pipe it charts the upstream output (`... | chart -x 1`). The modal is
// opened through emit on the event loop, with the typed line attached so the
// surface that shows it can report itself closed against that command.
func (s *Shell) chartCommand(emit func(ChartRequest)) func(cx *shellcore.Cmd) int {
	return func(cx *shellcore.Cmd) int {
		var req ChartRequest
		var note, errMsg string
		if cx.StdinIsTerminal() {
			req, errMsg = s.chartWatchRequest(cx.Args)
		} else if parsed, perr := chart.ParsePipeArgs(cx.Args); perr != "" {
			errMsg = "chart: " + perr
		} else {
			data, _ := io.ReadAll(cx.Stdin)
			var lines []string
			for _, ln := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(ln) != "" {
					lines = append(lines, ln)
				}
			}
			req, note, errMsg = s.chartPipeRequest(lines, parsed.(chart.PipeArgs))
		}
		if errMsg != "" {
			fmt.Fprintln(cx.Stderr, shellcore.ErrorText(errMsg))
			return 1
		}
		if err := cx.OnLoop(func(c *shellcore.Shell) error {
			req.Pipeline = c.RunningPipeline()
			emit(req)
			return nil
		}); err != nil {
			return 130 // cancelled before the modal could open
		}
		io.WriteString(cx.Stdout, note)
		return 0
	}
}

// dashboardCommand returns a `dashboard` command that forwards the requested
// name to a Go-side callback rather than rendering a shellcore View. The
// browser embedder uses this so `dashboard prod` at the prompt navigates the
// React app to /dashboards/prod. Bare `dashboard` passes "" — the embedder
// decides whether that means "go to the index" or "show usage". The callback
// runs on the event loop.
func dashboardCommand(navigate func(name string)) func(cx *shellcore.Cmd) int {
	return func(cx *shellcore.Cmd) int {
		name := ""
		for _, a := range cx.Args {
			switch {
			case strings.HasPrefix(a, "-"):
				fmt.Fprintln(cx.Stderr, shellcore.ErrorText("dashboard: unknown option "+a))
				return 1
			case name != "":
				fmt.Fprintln(cx.Stderr, shellcore.ErrorText("dashboard: unexpected argument "+a))
				return 1
			default:
				name = a
			}
		}
		if err := cx.OnLoop(func(*shellcore.Shell) error { navigate(name); return nil }); err != nil {
			return 130
		}
		return 0
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

// traceRequest parses the arguments of a `trace` line (without the word
// itself) into the request the browser opens, and reports whether the browser
// has a modal for it. The shapes it does not claim (a grammar error, or the
// `--samples` view) are the server's: the remote hook ships those lines like
// any other (see Claim), and the CLI's one-shot mode recognises them after
// the fact (see RenderTraceIfApplicable).
func traceRequest(args []string) (TraceRequest, bool) {
	cmd, err := tracecmd.Parse(args)
	if err != nil {
		return TraceRequest{}, false
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
		return TraceRequest{}, false
	}
	return TraceRequest{Pipeline: cmd.String(), Title: cmd.Title(), Mode: mode}, true
}

// traceCommand is the `trace` command: it forwards the request to the embedder
// callback, on the event loop. Only lines the browser can show reach it (see
// claims); anything else is shipped before it could run, so a rejection here
// can only happen on a core with no remote.
func traceCommand(emit func(TraceRequest)) func(cx *shellcore.Cmd) int {
	return func(cx *shellcore.Cmd) int {
		req, ok := traceRequest(cx.Args)
		if !ok {
			fmt.Fprintln(cx.Stderr, shellcore.ErrorText("trace: not supported here: trace "+strings.Join(cx.Args, " ")))
			return 1
		}
		if err := cx.OnLoop(func(*shellcore.Shell) error { emit(req); return nil }); err != nil {
			return 130
		}
		return 0
	}
}

// rangeCommand returns the `range` command bound to s: a bare `range` shows the
// current window, `range <spec>` sets it. It is a command, not a builtin, so a
// bad spec exits 1 and it works the same with or without SetPost. The window
// itself is client state: it never ships to the server except as the range
// argument of each query. Changing it touches the core (the prompt's window
// segment), which only the event loop may do, so that part goes through OnLoop.
func rangeCommand(s *Shell) func(cx *shellcore.Cmd) int {
	fail := func(cx *shellcore.Cmd, err error) int {
		fmt.Fprintln(cx.Stdout, shellcore.ErrorText("range: "+err.Error()))
		return 1
	}
	return func(cx *shellcore.Cmd) int {
		if len(cx.Args) == 0 {
			start, end, err := timerange.ParseRange(s.Range)
			if err != nil {
				return fail(cx, err)
			}
			io.WriteString(cx.Stdout, FormatRange(start, end))
			return 0
		}
		spec := strings.Join(cx.Args, " ")
		start, end, err := timerange.ParseRange(spec)
		if err != nil {
			return fail(cx, err)
		}
		if err := cx.OnLoop(func(c *shellcore.Shell) error {
			s.Range = spec
			if s.OnRangeChange != nil {
				s.OnRangeChange(spec)
			}
			return nil
		}); err != nil {
			return 130 // cancelled before it could apply
		}
		io.WriteString(cx.Stdout, FormatRange(start, end))
		return 0
	}
}

// oneShotRange is `range` for the one-shot path, which does not drive through
// shellcore: bare `range` shows the current window and `range <spec>` sets it,
// the same surface as rangeCommand.
func (s *Shell) oneShotRange(args []string, w io.Writer) error {
	spec := s.Range
	if len(args) > 0 {
		spec = strings.Join(args, " ")
	}
	start, end, err := timerange.ParseRange(spec)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		s.Range = spec
	}
	fmt.Fprint(w, timerange.FormatRangePlain(start, end))
	return nil
}

// ExecNonInteractive runs one line without a terminal, writing its output to w.
// The line runs through a headless pkg/bash runner on the same split as the
// interactive shell (bash.RemoteRewrite): `range` is the client's and
// everything else ships to the server. One-shot mode has no screen to page or
// live-redraw against, so a pager suffix is dropped, and a pipe-stage chart
// renders once to w.
func (s *Shell) ExecNonInteractive(line string, w io.Writer) error {
	line = strings.TrimSpace(line)

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

	// Strip a pager suffix or pipe-stage chart: chart renders from what the
	// rest of the line prints.
	weftPipeline, chartArgs, errMsg := chart.SplitPipeline(line)
	if errMsg != "" {
		return fmt.Errorf("chart: %s", errMsg)
	}
	if chartArgs == nil {
		weftPipeline, _ = splitTerminalStage(line)
	}

	// The server (a real pkg/bash interpreter) owns chain and relative-path
	// resolution, so its part of the line ships as typed. One-shot mode has no
	// persistent cwd to ferry; the runner starts at "/" and carries a `cd` in
	// the line to the segment after it.
	var runErr error
	fail := func(err error) int {
		if runErr == nil {
			runErr = err
		}
		return 1
	}
	var out io.Writer = w
	var captured bytes.Buffer
	if chartArgs != nil {
		out = &captured
	}
	ship := func(ctx context.Context, text string, stack []string, _ io.Reader, stdout, _ io.Writer) bash.ShipResult {
		var result *QueryResult
		var err error
		if s.QueryCtx != nil {
			result, err = s.QueryCtx(ctx, text, s.Range, stack)
		} else {
			result, err = s.Query(text, s.Range, stack)
		}
		if err != nil {
			return bash.ShipResult{Exit: fail(err)}
		}
		if result == nil {
			return bash.ShipResult{}
		}
		if result.Text != "" {
			fmt.Fprint(stdout, result.Text)
		} else if rendered, ok := RenderSpanRollupIfApplicable(text, result, s.TermWidth); ok && chartArgs == nil {
			for _, l := range rendered {
				fmt.Fprintln(stdout, l)
			}
		} else if rendered, ok := RenderTraceIfApplicable(text, result, s.TermWidth); ok && chartArgs == nil {
			for _, l := range rendered {
				fmt.Fprintln(stdout, l)
			}
		} else {
			for _, row := range result.Rows {
				fmt.Fprintln(stdout, row.Raw)
			}
		}
		return bash.ShipResult{DirStack: result.FinalDirStack}
	}
	classify := func(name string, _ []string) bash.Side {
		if name == "range" {
			return bash.Local
		}
		return bash.Remote
	}
	remote := bash.RemoteRewrite(classify, ship)
	cfg := bash.Config{
		AllowedCommands: []string{},
		// cd checks its directory through the client's filesystem; the server
		// takes the stack as given.
		ParseFns: []bash.ParseFn{func(o bash.ParseOrigin, script *bash.Script) {
			if o.Kind == bash.ParseRoot {
				remote(o, script)
			}
		}},
		Setup: func(_ context.Context, interp *bash.Interpreter, _ io.Writer, _ io.Reader) {
			interp.RegisterCommand("range", func(args []string, _ io.Reader, stdout, _ io.Writer) int {
				if err := s.oneShotRange(args, stdout); err != nil {
					return fail(err)
				}
				return 0
			})
		},
	}
	if s.FS != nil {
		cfg.FS = s.FS
	}
	var stderr bytes.Buffer
	res := bash.NewRunner(cfg)(context.Background(), bash.RunRequest{Line: weftPipeline, Stderr: &stderr}, out)
	switch {
	case runErr != nil:
		return runErr
	case res.Err != nil:
		return res.Err
	case res.Exit != 0:
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return fmt.Errorf("exit status %d", res.Exit)
	}
	if chartArgs != nil {
		lines := queryResultLines(&QueryResult{Text: captured.String()})
		rendered, errMsg := chart.RenderFromStream(lines, chartArgs.WithRange(s.Range), 80, 20)
		if errMsg != "" {
			return fmt.Errorf("chart: %s", errMsg)
		}
		fmt.Fprint(w, rendered)
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

// Claim registers a predicate that decides whether a line consisting of just
// the client command name, with these arguments, is the client's to run. A
// line it declines is shipped to the server like any other; this is how a
// command "yields". An embedder that registers its own client command (the
// CLI's in-terminal trace view) registers the matching predicate here.
func (s *Shell) Claim(name string, fn func(args []string) bool) {
	if s.claimFns == nil {
		s.claimFns = map[string]func([]string) bool{}
	}
	s.claimFns[name] = fn
}

// claims reports whether the client runs a line that is just the registered
// command name with args.
func (s *Shell) claims(name string, args []string) bool {
	if fn := s.claimFns[name]; fn != nil {
		return fn(args)
	}
	return true
}
