//go:build js && wasm

// Command wasm builds weft's shellcore WASM binary for use with
// xterm.js. Weft-specific bits live here; cross-cutting plumbing
// lives in warp/pkg/shellcore/wasm and is invoked via wasm.Boot.
//
// Build: GOOS=js GOARCH=wasm go build -o frontend/public/shell.wasm ./cmd/wasm/
package main

import (
	"syscall/js"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/warp/pkg/shellcore/wasm"
	"github.com/kevlarcon/weft/chart"
)

var (
	core         *shellcore.ShellCore
	jsCallbacks  js.Value
)

func main() {
	js.Global().Set("wasmInit", js.FuncOf(wasmInit))
	// All other wasm* exports (HandleInput, Resize, SpinnerTick,
	// SetInput, GetInput, HandleQueryResult, HandleFollowRow,
	// HandleFollowDone) are registered by wasm.Boot — they share
	// the same `active` core inside warp's package and don't need
	// weft-side wiring.
	select {}
}

// wasmInit(width: number, height: number, callbacks: object)
// callbacks: write, startQuery, startFollow, cancelQuery, notifyQuit,
// fsReadFile, fsWriteFile, fsReadDir, fsRemove, fsGetWatchResults,
// onRangeChange, loadHistory, saveHistory, getUser, getCustomer,
// getApps.
func wasmInit(_ js.Value, args []js.Value) any {
	width := args[0].Int()
	height := args[1].Int()
	jsCallbacks = args[2]

	// Apps list is weft-specific — read via callback rather than
	// passed as a direct init arg, so the loadShellWasm signature
	// stays app-agnostic.
	var apps []string
	if appsFn := jsCallbacks.Get("getApps"); !appsFn.IsUndefined() && !appsFn.IsNull() {
		appsJS := jsCallbacks.Call("getApps")
		for i := 0; i < appsJS.Length(); i++ {
			apps = append(apps, appsJS.Index(i).String())
		}
	}

	core = wasm.Boot(wasm.BootOpts{
		Apps:      apps,
		Width:     width,
		Height:    height,
		Callbacks: jsCallbacks,
		OnAction:  handleWeftAction,
	})

	// Chart View + builtin + pipe stage — weft-specific. Uses a
	// dedicated WatchResultsProvider that calls fsGetWatchResults
	// on the same callbacks object the FS proxy uses.
	chart.Register(core, &watchResultsProvider{callbacks: jsCallbacks})

	// Optional customer suffix on the prompt prefix (weft-specific).
	if getCustomerFn := jsCallbacks.Get("getCustomer"); !getCustomerFn.IsUndefined() && !getCustomerFn.IsNull() {
		customer := jsCallbacks.Call("getCustomer").String()
		if customer != "" && customer != "undefined" {
			core.SetCustomer(customer)
		}
	}

	// Default time range: last 24h (weft-specific).
	rangeStart := time.Now().Add(-24 * time.Hour)
	rangeEnd := time.Now()
	core.SetRange(rangeStart, rangeEnd)
	jsCallbacks.Call("onRangeChange", shellcore.FormatRangePlain(rangeStart, rangeEnd))

	core.Redraw()
	return nil
}

// handleWeftAction dispatches shellcore Actions warp's bridge doesn't
// own. ActionQuery / ActionFollow / ActionCancel / ActionVimSave /
// ActionQuit are dispatched by warp directly and never reach here.
func handleWeftAction(a shellcore.Action) bool {
	switch a := a.(type) {
	case shellcore.ActionSetRange:
		jsCallbacks.Call("onRangeChange", shellcore.FormatRangePlain(a.Start, a.End))
		return true
	}
	return false
}

// watchResultsProvider satisfies chart.WatchResultsProvider by
// delegating to the JS-side fsGetWatchResults callback.
type watchResultsProvider struct {
	callbacks js.Value
}

func (w *watchResultsProvider) GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error) {
	result := w.callbacks.Call("fsGetWatchResults", name, limit)
	if result.IsNull() || result.IsUndefined() {
		return nil, jsError("no results")
	}
	if errVal := result.Get("error"); !errVal.IsUndefined() && !errVal.IsNull() {
		if s := errVal.String(); s != "" {
			return nil, jsError(s)
		}
	}
	items := result.Get("results")
	if items.IsUndefined() || items.IsNull() {
		return nil, nil
	}
	out := make([]chart.WatchResultValue, 0, items.Length())
	for i := 0; i < items.Length(); i++ {
		item := items.Index(i)
		out = append(out, chart.WatchResultValue{
			Value: item.Get("stdout").String(),
			RanAt: item.Get("ran_at").String(),
		})
	}
	return out, nil
}

// jsError implements the error interface for a JS-string error.
type jsError string

func (e jsError) Error() string { return string(e) }
