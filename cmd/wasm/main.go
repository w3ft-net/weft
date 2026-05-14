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
	"syscall/js"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/warp/pkg/shellcore/wasm"
	"github.com/kevlarcon/weft/shell"
)

var (
	jsCallbacks js.Value
	// weftShell is set in wasmInit and read by handleWeftAction so the
	// ActionSetRange path can read the latest spec (mutated by
	// rangeBuiltinFor before dispatch). WASM is single-threaded so no
	// guarding is needed.
	weftShell *shell.Shell
)

func main() {
	js.Global().Set("wasmInit", js.FuncOf(wasmInit))
	// All other wasm* exports (HandleInput, Resize, SpinnerTick,
	// SetInput, GetInput, HandleQueryResult, HandleFollowRow,
	// HandleFollowDone) are registered by wasm.Boot.
	select {}
}

// wasmInit(width: number, height: number, callbacks: object)
// callbacks: write, startQuery, startFollow, cancelQuery, notifyQuit,
// fsReadFile, fsWriteFile, fsReadDir, fsRemove, fsGetWatchResults,
// onRangeChange, builtinExecuted, loadHistory, saveHistory, getUser,
// getCustomer, getApps.
func wasmInit(_ js.Value, args []js.Value) any {
	width := args[0].Int()
	height := args[1].Int()
	jsCallbacks = args[2]

	apps := readApps(jsCallbacks)

	core := wasm.Boot(wasm.BootOpts{
		Apps:      apps,
		Width:     width,
		Height:    height,
		Callbacks: jsCallbacks,
		OnAction:  handleWeftAction,
	})

	// Build the JS-backed MountFS. SetFS in ConfigureCore picks this
	// up and auto-derives the PathClassifier from it.
	mfs := shellcore.NewMountFS()
	mfs.Register(wasm.NewJSMount("/bin", shellcore.MountCaps{Kind: shellcore.MountLocal, Writable: true}, jsCallbacks, "bash"))
	mfs.Register(wasm.NewJSMount("/cron.d", shellcore.MountCaps{Kind: shellcore.MountLocal, Writable: true}, jsCallbacks, "bash"))
	mfs.Register(wasm.NewJSMount("/etc", shellcore.MountCaps{Kind: shellcore.MountLocal, Writable: true}, jsCallbacks, "yaml"))
	mfs.Register(wasm.NewJSMount("/log", shellcore.MountCaps{Kind: shellcore.MountQuery, Writable: false}, jsCallbacks, ""))
	mfs.Register(wasm.NewJSMount("/tmp", shellcore.MountCaps{Kind: shellcore.MountLocal, Writable: true}, jsCallbacks, ""))

	// Default time range: last 24h. Resolution happens server-side on each
	// request — the client just ships the spec verbatim, so a tab left
	// open all day never queries against a stale window.
	s := &shell.Shell{
		Apps:     apps,
		FS:       mfs,
		Customer: readCustomer(jsCallbacks),
		Range:    "1d..",
		// Browser shell has no local Query/Follow: the JS network layer
		// (frontend/src/wasm/network.ts) owns those fetches and is driven
		// by warp's startQuery/startFollow JS callbacks.
		// Browser `dashboard <name>` routes to React-router navigation;
		// no in-terminal view. Bare `dashboard` sends "" so the JS side
		// can decide between the index page or a usage hint.
		Dashboard: func(name string) {
			jsCallbacks.Call("dashboardNavigate", name)
		},
		// Browser `chart <name>` and `... | chart` open a modal in
		// React instead of rendering ASCII into xterm. The JS side
		// owns the actual tile rendering (re-using dashboard
		// components); we just hand over the parsed parameters.
		Chart: func(r shell.ChartRequest) {
			spec := map[string]any{
				"watch": r.Watch,
				"kind":  r.Kind,
				"title": r.Title,
				"limit": r.Limit,
			}
			if r.Lines != nil {
				// js.Value can't marshal a []string directly through
				// the map — it wants an explicit []any.
				items := make([]any, len(r.Lines))
				for i, ln := range r.Lines {
					items[i] = ln
				}
				spec["lines"] = items
			}
			jsCallbacks.Call("chartOpen", spec)
		},
	}
	weftShell = s
	s.ConfigureCore(core)

	// Mirror the initial range to JS — the CLI's tty prompt shows the
	// window inline; the browser displays it in a separate header chip via
	// the onRangeChange callback. We pass both the raw spec (which the JS
	// network layer reuses on every fetch) and the formatted label for the
	// chip. Format with a fresh ParseRange snapshot — the duration label
	// is stable across the lifetime of a relative spec.
	rsStart, rsEnd, _ := shellcore.ParseRange(s.Range)
	jsCallbacks.Call("onRangeChange", s.Range, shellcore.FormatRangePlain(rsStart, rsEnd))

	core.Redraw()
	return nil
}

func readApps(cb js.Value) []string {
	fn := cb.Get("getApps")
	if fn.IsUndefined() || fn.IsNull() {
		return nil
	}
	val := cb.Call("getApps")
	apps := make([]string, 0, val.Length())
	for i := 0; i < val.Length(); i++ {
		apps = append(apps, val.Index(i).String())
	}
	return apps
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

// handleWeftAction dispatches shellcore Actions warp's bridge doesn't
// own. ActionQuery / ActionFollow / ActionCancel / ActionVimSave /
// ActionQuit are dispatched by warp directly and never reach here.
func handleWeftAction(a shellcore.Action) bool {
	switch a := a.(type) {
	case shellcore.ActionSetRange:
		// rangeBuiltinFor already mutated weftShell.Range to the new
		// spec; pass it to JS alongside the formatted label so the
		// network layer ships the latest spec on subsequent fetches.
		jsCallbacks.Call("onRangeChange", weftShell.Range, shellcore.FormatRangePlain(a.Start, a.End))
		return true
	case shellcore.ActionBuiltinExecuted:
		if fn := jsCallbacks.Get("builtinExecuted"); !fn.IsUndefined() && !fn.IsNull() {
			jsCallbacks.Call("builtinExecuted", a.Pipeline)
		}
		return true
	}
	return false
}

