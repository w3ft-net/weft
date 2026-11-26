//go:build js && wasm

// Package boot is weft's shellcore WASM binary for use with
// xterm.js. The bridge plumbing (shellcore <-> JS callbacks, xterm
// input, history) lives in warp's wasm.Boot. After Boot,
// the browser shell is configured the same way the CLI shell is:
// by constructing a *shell.Shell and calling its ConfigureCore.
// That single entry point is what guarantees the browser and CLI
// surfaces stay aligned — every embedder-owned bit (range builtin,
// reserved names, wit grammar, help corpus, vim popup hooks, FS,
// customer prompt) is installed in one place.
//
// The process is cmd/wasm, whose main calls Main; a deployment builds its own
// main that does the same, so the same binary can be built from a module that
// vendors this one.
//
// Build: GOOS=js GOARCH=wasm go build -o frontend/public/shell.wasm ./cmd/wasm/
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall/js"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/graybeardcorp/warp/wasm"
	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/shell"
	"github.com/w3ft-net/weft/theme"
	"github.com/w3ft-net/weft/timerange"
	"github.com/w3ft-net/weft/tracecmd"
)

var jsCallbacks js.Value

// sharedFS is the one MountFS every pane shares. Browser tmux boots
// multiple cores (one per pane) over this single FS so a file written
// in one pane is visible in another (real-tmux semantics). It is built
// on the first init and reused by every subsequent pane. FS operations
// proxy through the JS callbacks, which hit the /api/fs endpoints, so a
// shared *MountFS across panes is correct: they all read the same
// server-side group filesystem.
var sharedFS *shellcore.MountFS

// Options is what a deployment adds to the browser shell.
type Options struct {
	// Tiers is the plan table `help cron` shows. Nil leaves that section out.
	Tiers shell.TierLimitsProvider
}

var options Options

// Main boots the browser shell and blocks for the life of the page.
func Main(o Options) {
	options = o
	// weft has one kind of shell, so it registers the default mode. A
	// request with an instance id boots one pane of a browser tmux,
	// sharing sharedFS; the wasmPane* exports that drive it
	// (input/resize/close/…) are registered by wasm.Boot.
	wasm.RegisterBoot("", func(r wasm.BootRequest) {
		if r.Instance == "" {
			bootSingle(r.Width, r.Height, r.Callbacks)
		} else {
			bootPane(r.Instance, r.Width, r.Height, r.Callbacks)
		}
	})
	// All other wasm* exports (HandleInput, Resize, SpinnerTick,
	// SetInput, GetInput, HandleQueryResult, HandleFollowRow,
	// HandleFollowDone) are registered by wasm.Boot.
	//
	// wasmRenderSpanRollup / wasmParseTraceRows are stateless, called
	// directly by JS (not routed through a core/pane) once React has
	// the raw rows for a TraceRequest.Pipeline re-run — see
	// docs/plans/browser-span-view.md. They wrap theme's rollup/parse
	// math so the self-time interval-union and percentile computation
	// stay in one language rather than getting reimplemented in JS.
	js.Global().Set("wasmRenderSpanRollup", js.FuncOf(wasmRenderSpanRollup))
	js.Global().Set("wasmRenderSpanAggregate", js.FuncOf(wasmRenderSpanAggregate))
	js.Global().Set("wasmRenderSpanSamples", js.FuncOf(wasmRenderSpanSamples))
	js.Global().Set("wasmParseTraceRows", js.FuncOf(wasmParseTraceRows))
	js.Global().Set("wasmParseTraceList", js.FuncOf(wasmParseTraceList))
	js.Global().Set("wasmSamplesPipeline", js.FuncOf(wasmSamplesPipeline))
	js.Global().Set("wasmTraceIDPipeline", js.FuncOf(wasmTraceIDPipeline))
	select {}
}

// jsStringSlice converts a JS array-of-strings value into a []string.
func jsStringSlice(v js.Value) []string {
	n := v.Length()
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = v.Index(i).String()
	}
	return out
}

// jsonResult marshals v to JSON and returns it as a JS string, or a
// {"error": ...} JSON string on a marshal failure (theme's rollup/row
// types are plain exported-field structs, so in practice this never
// fails — the fallback exists so a caller gets a parseable response
// shape either way instead of a Go panic crossing the WASM boundary).
func jsonResult(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	return string(b)
}

// wasmRenderSpanRollup(lines: string[]) → JSON string of
// theme.SpanRollupRow[] for the JSON lines of `trace --spans --id <id>`.
func wasmRenderSpanRollup(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	return jsonResult(theme.RenderSpanRollup(lines))
}

// wasmRenderSpanAggregate(lines: string[]) → JSON string of
// theme.SpanRollupRow[] for the JSON lines of `trace --spans [--name <span>]`.
func wasmRenderSpanAggregate(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	return jsonResult(theme.RenderSpanAggregate(lines))
}

// wasmRenderSpanSamples(lines: string[]) → JSON string of
// theme.SpanOccurrence[] for the JSON lines of `trace --spans --name <span> --samples`.
func wasmRenderSpanSamples(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	return jsonResult(theme.RenderSpanSamples(lines, theme.SpanSamplesLimit))
}

// wasmParseTraceRows(lines: string[]) → JSON string of
// theme.TraceRow[], the same parse step RenderTraceColumns (the ANSI
// renderer) uses internally — see theme.ParseTraceRows.
func wasmParseTraceRows(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	return jsonResult(theme.ParseTraceRows(lines))
}

// wasmParseTraceList(lines: string[], nowMs: number) → JSON string of
// theme.TraceListRow[] for the JSON lines of a bare `trace`, with ages
// relative to nowMs.
func wasmParseTraceList(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	now := time.UnixMilli(int64(args[1].Float()))
	return jsonResult(theme.ParseTraceList(now, lines))
}

// wasmSamplesPipeline(aggregate, name, label: string) → the pipeline that
// lists one aggregate row's slowest occurrences; see tracecmd.SamplesPipeline.
func wasmSamplesPipeline(_ js.Value, args []js.Value) any {
	return tracecmd.SamplesPipeline(args[0].String(), args[1].String(), args[2].String())
}

// wasmTraceIDPipeline(id: string) → the pipeline that shows one request.
func wasmTraceIDPipeline(_ js.Value, args []js.Value) any {
	return tracecmd.IDPipeline(args[0].String())
}

// buildSharedFS constructs (once) the MountFS shared by every pane.
func buildSharedFS(cb js.Value) *shellcore.MountFS {
	if sharedFS != nil {
		return sharedFS
	}
	mfs := shellcore.NewMountFS()
	mfs.Register(wasm.NewJSMount("/bin", true, cb, "bash"))
	mfs.Register(wasm.NewJSMount("/cron.d", true, cb, "bash"))
	mfs.Register(wasm.NewJSMount("/etc", true, cb, "yaml"))
	mfs.Register(wasm.NewJSMount("/log", false, cb, ""))
	mfs.Register(wasm.NewJSMount("/tmp", true, cb, ""))
	sharedFS = mfs
	return mfs
}

// cronFS adds chart.CronSource to the shared MountFS: the pane's
// fsGetCronSeries callback fetches /api/crons/<name>/chart synchronously,
// like the mounts' file callbacks.
type cronFS struct {
	*shellcore.MountFS
	cb js.Value
}

func (f cronFS) GetCronSeries(name, rangeSpec string) (chart.CronSeries, error) {
	res := f.cb.Call("fsGetCronSeries", name, rangeSpec)
	if e := res.Get("error"); e.Truthy() {
		return chart.CronSeries{}, errors.New(e.String())
	}
	return shell.DecodeCronSeries([]byte(res.Get("body").String()))
}

// buildShell wires a *shell.Shell for one core against the shared FS.
// The range/dashboard/chart hooks fire JS callbacks; each pane gets its
// own callbacks object (own write target) so output lands in its own
// xterm, but they share one server-side FS and coordinator.
func buildShell(cb js.Value, mfs *shellcore.MountFS) *shell.Shell {
	s := &shell.Shell{
		Tiers:    options.Tiers,
		FS:       cronFS{MountFS: mfs, cb: cb},
		Customer: readCustomer(cb),
		Range:    "1d..",
		Dashboard: func(name string) {
			cb.Call("dashboardNavigate", name)
		},
		Chart: func(r shell.ChartRequest) {
			spec := map[string]any{
				"pipeline": r.Pipeline,
				"cron":     r.Watch,
				"kind":     r.Kind,
				"title":    r.Title,
			}
			if r.Plot != nil {
				// JSON keeps the nested series out of syscall/js's
				// hand-built value conversion; bridge.ts parses it back.
				if b, err := json.Marshal(r.Plot); err == nil {
					spec["plot"] = string(b)
				}
			}
			cb.Call("chartOpen", spec)
		},
		Trace: func(r shell.TraceRequest) {
			cb.Call("traceOpen", map[string]any{
				"pipeline": r.Pipeline,
				"title":    r.Title,
				"mode":     r.Mode,
			})
		},
	}
	s.OnRangeChange = func(spec string) {
		rs, re, _ := timerange.ParseRange(spec)
		cb.Call("onRangeChange", spec, timerange.FormatRangePlain(rs, re))
	}
	return s
}

// bootPane boots one pane of a multi-pane page.
func bootPane(instance string, width, height int, cb js.Value) {
	core := wasm.Boot(wasm.BootOpts{
		Instance:  instance,
		Width:     width,
		Height:    height,
		Callbacks: cb,
		AsyncRuns: true,
	})
	reportCompletions(core, cb)

	s := buildShell(cb, buildSharedFS(cb))
	useBrowserRemote(s, cb)
	s.ConfigureCore(core)

	rsStart, rsEnd, _ := timerange.ParseRange(s.Range)
	cb.Call("onRangeChange", s.Range, timerange.FormatRangePlain(rsStart, rsEnd))

	core.Redraw()
}

// bootSingle boots the page's one terminal.
// callbacks: write, notifyQuit, apiBase, authHeaders, tourAdvance,
// fsReadFile, fsWriteFile, fsReadDir, fsRemove, fsGetWatchResults,
// onRangeChange, commandCompleted, loadHistory, saveHistory, getUser,
// getCustomer.
func bootSingle(width, height int, cb js.Value) {
	jsCallbacks = cb

	// The single-pane shell is just the "" instance of the pane registry:
	// booting under the empty id keeps the legacy singleton exports
	// (wasmHandleInput, …) pointed at it. It shares the same builders as
	// the multi-pane path so behavior can't drift between the two.
	core := wasm.Boot(wasm.BootOpts{
		Width:     width,
		Height:    height,
		Callbacks: jsCallbacks,
		AsyncRuns: true,
	})
	reportCompletions(core, jsCallbacks)

	s := buildShell(jsCallbacks, buildSharedFS(jsCallbacks))
	useBrowserRemote(s, jsCallbacks)
	s.ConfigureCore(core)

	// Mirror the initial range to JS — the CLI's tty prompt shows the
	// window inline; the browser displays it in a separate header chip via
	// the onRangeChange callback. We pass both the raw spec (which the JS
	// network layer reuses on every fetch) and the formatted label for the
	// chip. Format with a fresh ParseRange snapshot — the duration label
	// is stable across the lifetime of a relative spec.
	rsStart, rsEnd, _ := timerange.ParseRange(s.Range)
	jsCallbacks.Call("onRangeChange", s.Range, timerange.FormatRangePlain(rsStart, rsEnd))

	core.Redraw()
}

// useBrowserRemote sends the shell's lines to the server over HTTP: Go's
// net/http is fetch under GOOS=js, so the browser runs the same client as the
// CLI (shell.ExecClient). The core runs each line on its own goroutine
// (AsyncRuns), and the run waits on the response; cancelling the run aborts
// the fetch. The page supplies the API base and, per request, the auth
// headers (apiBase, authHeaders).
func useBrowserRemote(s *shell.Shell, cb js.Value) {
	client := &shell.ExecClient{
		BaseURL: strings.TrimSuffix(cb.Get("apiBase").String(), "/"),
		Headers: func() map[string]string {
			hdrs := cb.Call("authHeaders")
			keys := js.Global().Get("Object").Call("keys", hdrs)
			out := make(map[string]string, keys.Length())
			for i := 0; i < keys.Length(); i++ {
				k := keys.Index(i).String()
				out[k] = hdrs.Get(k).String()
			}
			return out
		},
		Transport: fetchTransport{},
	}
	s.QueryCtx = func(ctx context.Context, pipeline, rangeSpec string, dirStack []string) (*shell.QueryResult, error) {
		return client.Query(ctx, pipeline, rangeSpec, dirStack)
	}
	s.Follow = func(ctx context.Context, pipeline, rangeSpec string, onRow func(shell.Row)) error {
		return client.Follow(ctx, pipeline, rangeSpec, onRow)
	}
}

func readCustomer(cb js.Value) string {
	fn := cb.Get("getCustomer")
	if fn.IsUndefined() || fn.IsNull() {
		return ""
	}
	c := cb.Call("getCustomer").String()
	if c == "undefined" {
		return ""
	}
	return c
}

// reportCompletions forwards every completed shell line to the host as
// commandCompleted(line, exitCode). The core publishes CommandCompleted
// for each line — inline builtins, dispatched pipelines (when their result
// arrives) and failures alike — so the host can track progress from what
// the user actually ran and whether it succeeded, rather than inferring it
// from the pipeline text the server sees. Called once per core, for the
// single-pane shell and every pane.
func reportCompletions(core *shellcore.Shell, cb js.Value) {
	ch := shellcore.Subscribe[shellcore.CommandCompleted](core)
	go func() {
		for ev := range ch {
			if fn := cb.Get("commandCompleted"); fn.Type() == js.TypeFunction {
				cb.Call("commandCompleted", ev.Pipeline, ev.ExitCode)
			}
		}
	}()
}
