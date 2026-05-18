package shell

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/warp/pkg/syntax"
	"github.com/kevlarcon/weft/chart"
	"github.com/kevlarcon/weft/internal/timerange"
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
type QueryFunc func(pipeline, rangeSpec string) (*QueryResult, error)

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
	// ShareURLBase is the absolute base URL the `clipboard` builtin
	// embeds in deep-link URLs (e.g. https://w3ft.net). Empty falls
	// back to the production host so a CLI invocation without explicit
	// config still produces a shareable link.
	ShareURLBase string
}

// ConfigureCore wires every embedder-owned surface weft installs on a
// fresh shellcore — syntax fallback, FS + chart/dashboard hookups,
// user/customer prompt, range, help corpus, wit subcommand grammar,
// reserved names, `range` builtin, and the vim post-save popup. The
// CLI Run() and the browser cmd/wasm bridge both go through this
// single entry point. History persistence is intentionally NOT set
// here: CLI installs a file-backed store; wasm.Boot installs a
// JS-callback-backed store before this runs.
func (s *Shell) ConfigureCore(core *shellcore.ShellCore) {
	core.SetSyntaxFallback(syntax.LangBash)
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
	// Subcommand grammar matches the one ExecNonInteractive passes:
	// `wit log <path>` must preserve the keyword instead of
	// resolving `log` against `/` to the /log namespace.
	core.RegisterRewriteGrammar("wit", "log", "show", "diff", "checkout", "amend")
	// Reserve weft's natively-implemented command names so a saved
	// /bin/<name> function can't shadow them. These run through the
	// bash interpreter's RegisterCommand path (see
	// coordinator/weftcmds.go), not shellcore's RegisterBuiltin.
	for _, name := range []string{
		"alert",
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
	// `clipboard /bin/<name>` builds a deep-link URL for the current
	// range and pushes it onto the user's system clipboard via OSC-52.
	// The reader (Shell.tsx hydration) pre-fills the prompt rather
	// than auto-running, so the link is safe to hand to a teammate.
	shareBase := s.ShareURLBase
	if shareBase == "" {
		shareBase = "https://w3ft.net"
	}
	core.RegisterBuiltin("clipboard", clipboardBuiltin(s, shareBase))
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
	// Watch is the watch name when the user typed `chart <name>`.
	// Empty when the request originates from `... | chart`.
	Watch string
	// Lines is the upstream pipeline snapshot for `... | chart`. The
	// browser modal renders this directly; re-fetching the underlying
	// pipeline isn't supported at this layer because the original
	// pipeline string isn't reachable from a pipe-stage renderer.
	Lines []string
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
			c.AppendScrollback(shellcore.ErrorText("chart: --follow is not supported in the browser; open the watch page instead") + "\n")
			return true
		}
		emit(ChartRequest{
			Watch: parsed.Name,
			Kind:  parsed.Kind,
			Limit: parsed.Limit,
		})
		return true
	}
}

// chartRequestPipe returns a PipeStageRenderFunc that hands the
// upstream pipeline snapshot to the embedder callback and writes a
// terse placeholder to scrollback (so the user sees that something
// happened — the chart itself appears in a separate React modal).
func chartRequestPipe(emit func(ChartRequest)) shellcore.PipeStageRenderFunc {
	return func(_ *shellcore.ShellCore, lines []string, parsed any) (string, string) {
		args, ok := parsed.(chart.PipeArgs)
		if !ok {
			return "", "chart: internal error: parsed args not PipeArgs"
		}
		emit(ChartRequest{
			Lines: lines,
			Kind:  args.Kind,
			Title: args.Title,
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

	// FS builtins: ls, cat for /bin /cron.d /etc /log paths.
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

	// Resolve cd / pushd / popd against a transient dirstack so a
	// chained pipeline like `cd /log/acme && cat api.log` is rewritten
	// to `cd /log/acme && cat /log/acme/api.log` before reaching the
	// planner. Without this the planner sees the bare `api.log` and
	// rejects it as an unknown namespace.
	//
	// `wit` has a fixed subcommand grammar; declare it so the rewriter
	// preserves the keyword in `wit log /bin/scan` instead of joining
	// `log` against `/` and resolving it to the `/log` namespace root.
	var rewriteOpts []shellcore.RewriteOption
	rewriteOpts = append(rewriteOpts, shellcore.WithSubcommandBuiltin("wit", "log", "show", "diff", "checkout", "amend"))
	if pc, ok := s.FS.(interface {
		ClassifyPath(string) shellcore.PathKind
	}); ok {
		rewriteOpts = append(rewriteOpts, shellcore.WithPathClassifier(pc.ClassifyPath))
	}
	if rewritten, err := shellcore.RewritePaths(weftPipeline, shellcore.NewTransientDirStack(),
		rewriteOpts...,
	); err == nil {
		weftPipeline = rewritten
	}

	result, err := s.Query(weftPipeline, s.Range)
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
	} else {
		for _, row := range result.Rows {
			fmt.Fprintln(w, row.Raw)
		}
	}
	return nil
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

// execFSBuiltin handles ls, cat, stat for FS paths in non-interactive mode.
// Returns (handled, output).
func (s *Shell) execFSBuiltin(line string) (bool, string) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false, ""
	}
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "ls":
		path := "/"
		if len(args) > 0 {
			path = args[0]
		}
		if s.FS == nil {
			return false, ""
		}
		// Only handle FS paths.
		if path != "/" && !strings.HasPrefix(path, "/log") && !strings.HasPrefix(path, "/bin") &&
			!strings.HasPrefix(path, "/cron.d") && !strings.HasPrefix(path, "/etc") &&
			!strings.HasPrefix(path, "log") && !strings.HasPrefix(path, "bin") &&
			!strings.HasPrefix(path, "cron.d") && !strings.HasPrefix(path, "etc") {
			return false, ""
		}
		entries, err := s.FS.ReadDir(path)
		if err != nil {
			return true, fmt.Sprintf("ls: %v\n", err)
		}
		var sb strings.Builder
		for _, e := range entries {
			sb.WriteString(e.Name())
			if e.IsDir() {
				sb.WriteByte('/')
			}
			sb.WriteByte('\n')
		}
		return true, sb.String()

	case "cat":
		if len(args) == 0 {
			return false, ""
		}
		path := args[0]
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		// Only handle FS paths (not /log/*.log which goes to the query pipeline).
		if strings.HasPrefix(path, "/bin/") || strings.HasPrefix(path, "/cron.d/") || strings.HasPrefix(path, "/etc/") {
			data, err := s.FS.ReadFile(path)
			if err != nil {
				return true, fmt.Sprintf("cat: %v\n", err)
			}
			return true, string(data)
		}
		return false, ""

	case "chart":
		// Non-interactive chart: render a single frame and return.
		// `--follow` is meaningless here (no terminal to redraw into),
		// so we ignore the flag and just paint the latest frame.
		wr, ok := s.FS.(chart.WatchResultsProvider)
		if !ok {
			return true, "chart: watch results not supported in this backend\n"
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
			return true, "usage: chart <watch> [-t TYPE] [-n LIMIT]\n"
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
