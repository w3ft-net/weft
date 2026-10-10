//go:build js && wasm

// Command wasm builds weft's shellcore WASM binary for use with
// xterm.js. The bridge plumbing (shellcore <-> JS callbacks, xterm
// input, history) lives in warp/pkg/shellcore/wasm.Boot. After Boot,
// the browser shell is configured the same way the CLI shell is:
// by constructing a *shell.Shell and calling its ConfigureCore.
// That single entry point is what guarantees the browser and CLI
// surfaces stay aligned — every embedder-owned bit (range builtin,
// reserved names, wit grammar, help corpus, vim popup hooks, FS,
// customer prompt) is installed in one place.
//
// Build: GOOS=js GOARCH=wasm go build -o frontend/public/shell.wasm ./cmd/wasm/
package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/warp/pkg/shellcore/wasm"
	"github.com/graybeardcorp/weft/internal/timerange"
	"github.com/graybeardcorp/weft/shell"
	"github.com/graybeardcorp/weft/theme"
)

var jsCallbacks js.Value

// sharedFS is the one MountFS every pane shares. Browser tmux boots
// multiple cores (one per pane) over this single FS so a file written
// in one pane is visible in another (real-tmux semantics). It is built
// on the first init and reused by every subsequent pane. FS operations
// proxy through the JS callbacks, which hit the /api/fs endpoints, so a
// shared *MountFS across panes is correct: they all read the same
// server-side tenant filesystem.
var sharedFS *shellcore.MountFS

func main() {
	js.Global().Set("wasmInit", js.FuncOf(wasmInit))
	// wasmInitPane boots one pane of a browser tmux: a core registered
	// under an instance id, sharing sharedFS. The wasmPane* exports that
	// drive it (input/resize/close/…) are registered by wasm.Boot.
	js.Global().Set("wasmInitPane", js.FuncOf(wasmInitPane))
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
	js.Global().Set("wasmParseTraceRows", js.FuncOf(wasmParseTraceRows))
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
// theme.SpanRollupRow[]. Covers both `trace <id> --spans` (Phase 1)
// and `trace spans [name]` (Phase 2) — theme.RenderSpanRollup doesn't
// care which; the caller (ShellTraceModal) picks singleTrace-vs-not
// column rendering from the TraceRequest.Mode it already has, the same
// distinction RenderSpanRollupIfApplicable (shell/shell.go) makes for
// the CLI's one-shot path.
func wasmRenderSpanRollup(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	return jsonResult(theme.RenderSpanRollup(lines))
}

// wasmParseTraceRows(lines: string[]) → JSON string of
// theme.TraceRow[], the same parse step RenderTraceColumns (the ANSI
// renderer) uses internally — see theme.ParseTraceRows.
func wasmParseTraceRows(_ js.Value, args []js.Value) any {
	lines := jsStringSlice(args[0])
	return jsonResult(theme.ParseTraceRows(lines))
}

// buildSharedFS constructs (once) the MountFS shared by every pane.
func buildSharedFS(cb js.Value) *shellcore.MountFS {
	if sharedFS != nil {
		return sharedFS
	}
	mfs := shellcore.NewMountFS()
	mfs.Register(wasm.NewJSMount("/bin", shellcore.MountCaps{Writable: true}, cb, "bash"))
	mfs.Register(wasm.NewJSMount("/cron.d", shellcore.MountCaps{Writable: true}, cb, "bash"))
	mfs.Register(wasm.NewJSMount("/etc", shellcore.MountCaps{Writable: true}, cb, "yaml"))
	mfs.Register(wasm.NewJSMount("/log", shellcore.MountCaps{Writable: false}, cb, ""))
	mfs.Register(wasm.NewJSMount("/tmp", shellcore.MountCaps{Writable: true}, cb, ""))
	sharedFS = mfs
	return mfs
}

// buildShell wires a *shell.Shell for one core against the shared FS.
// The range/dashboard/chart hooks fire JS callbacks; each pane gets its
// own callbacks object (own write target) so output lands in its own
// xterm, but they share one server-side FS and coordinator.
func buildShell(cb js.Value, apps []string, mfs *shellcore.MountFS) *shell.Shell {
	s := &shell.Shell{
		Apps:     apps,
		FS:       mfs,
		Customer: readCustomer(cb),
		Range:    "1d..",
		Dashboard: func(name string) {
			cb.Call("dashboardNavigate", name)
		},
		Chart: func(r shell.ChartRequest) {
			spec := map[string]any{
				"watch": r.Watch,
				"kind":  r.Kind,
				"title": r.Title,
				"limit": r.Limit,
			}
			if r.Lines != nil {
				items := make([]any, len(r.Lines))
				for i, ln := range r.Lines {
					items[i] = ln
				}
				spec["lines"] = items
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

// wasmInitPane(instance, width, height, callbacks) boots one pane.
func wasmInitPane(_ js.Value, args []js.Value) any {
	instance := args[0].String()
	width := args[1].Int()
	height := args[2].Int()
	cb := args[3]

	apps := readServices(cb)
	core := wasm.Boot(wasm.BootOpts{
		Instance:  instance,
		Apps:      apps,
		Width:     width,
		Height:    height,
		Callbacks: cb,
	})
	reportCompletions(core, cb)

	s := buildShell(cb, apps, buildSharedFS(cb))
	s.ConfigureCore(core)

	rsStart, rsEnd, _ := timerange.ParseRange(s.Range)
	cb.Call("onRangeChange", s.Range, timerange.FormatRangePlain(rsStart, rsEnd))

	core.Redraw()
	return nil
}

// wasmInit(width: number, height: number, callbacks: object)
// callbacks: write, startQuery, startFollow, cancelQuery, notifyQuit,
// fsReadFile, fsWriteFile, fsReadDir, fsRemove, fsGetWatchResults,
// onRangeChange, commandCompleted, loadHistory, saveHistory, getUser,
// getCustomer, getServices.
func wasmInit(_ js.Value, args []js.Value) any {
	width := args[0].Int()
	height := args[1].Int()
	jsCallbacks = args[2]

	apps := readServices(jsCallbacks)

	// The single-pane shell is just the "" instance of the pane registry:
	// booting under the empty id keeps the legacy singleton exports
	// (wasmHandleInput, …) pointed at it. It shares the same builders as
	// the multi-pane path so behavior can't drift between the two.
	core := wasm.Boot(wasm.BootOpts{
		Apps:      apps,
		Width:     width,
		Height:    height,
		Callbacks: jsCallbacks,
	})
	reportCompletions(core, jsCallbacks)

	s := buildShell(jsCallbacks, apps, buildSharedFS(jsCallbacks))
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
	return nil
}

func readServices(cb js.Value) []string {
	fn := cb.Get("getServices")
	if fn.IsUndefined() || fn.IsNull() {
		return nil
	}
	val := cb.Call("getServices")
	services := make([]string, 0, val.Length())
	for i := 0; i < val.Length(); i++ {
		services = append(services, val.Index(i).String())
	}
	return services
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
// arrives) and failures alike — so the host can track progress (the
// onboarding tour) from what the user actually ran and whether it
// succeeded, rather than inferring it from the pipeline text the server
// sees. Called once per core, for the single-pane shell and every pane.
func reportCompletions(core *shellcore.ShellCore, cb js.Value) {
	ch := shellcore.Subscribe[shellcore.CommandCompleted](core)
	go func() {
		for ev := range ch {
			if fn := cb.Get("commandCompleted"); fn.Type() == js.TypeFunction {
				cb.Call("commandCompleted", ev.Pipeline, ev.ExitCode)
			}
		}
	}()
}
