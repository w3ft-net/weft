//go:build js && wasm

// Command wasm builds the shellcore WASM binary for use with xterm.js.
// It exports functions to JavaScript via syscall/js for bridging
// between the browser and the Go shell state machine.
//
// Build: GOOS=js GOARCH=wasm go build -o frontend/public/shell.wasm ./cmd/wasm/
package main

import (
	"strings"
	"syscall/js"
	"time"
	"unicode/utf8"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/chart"
)

var core *shellcore.ShellCore

// jsCallbacks holds the JavaScript callback functions.
var jsCallbacks js.Value

func main() {
	// Export functions to JavaScript global scope.
	js.Global().Set("wasmInit", js.FuncOf(wasmInit))
	js.Global().Set("wasmHandleInput", js.FuncOf(wasmHandleInput))
	js.Global().Set("wasmHandleQueryResult", js.FuncOf(wasmHandleQueryResult))
	js.Global().Set("wasmHandleFollowRow", js.FuncOf(wasmHandleFollowRow))
	js.Global().Set("wasmHandleFollowDone", js.FuncOf(wasmHandleFollowDone))
	js.Global().Set("wasmResize", js.FuncOf(wasmResize))
	js.Global().Set("wasmSpinnerTick", js.FuncOf(wasmSpinnerTick))
	js.Global().Set("wasmSetInput", js.FuncOf(wasmSetInput))
	js.Global().Set("wasmGetInput", js.FuncOf(wasmGetInput))

	// Keep the Go program alive.
	select {}
}

// wasmInit(apps: string[], width: number, height: number, callbacks: object)
// callbacks must have: writeOutput(string), startQuery(string), startFollow(string),
//
//	cancelQuery(), notifyQuit()
//
// Optional callback fields: getUser() returns the username for the prompt.
func wasmInit(_ js.Value, args []js.Value) any {
	appsJS := args[0]
	width := args[1].Int()
	height := args[2].Int()
	jsCallbacks = args[3]

	apps := make([]string, appsJS.Length())
	for i := 0; i < appsJS.Length(); i++ {
		apps[i] = appsJS.Index(i).String()
	}

	core = shellcore.NewShellCore(
		apps, width, height,
		func(b []byte) {
			jsCallbacks.Call("write", string(b))
		},
		func(a shellcore.Action) {
			switch a := a.(type) {
			case shellcore.ActionQuery:
				jsCallbacks.Call("startQuery", a.Pipeline)
			case shellcore.ActionFollow:
				jsCallbacks.Call("startFollow", a.Pipeline)
			case shellcore.ActionCancel:
				jsCallbacks.Call("cancelQuery")
			case shellcore.ActionVimSave:
				// Write through the JS FS adapter (which calls the API).
				fs := core.GetFS()
				if fs != nil {
					fs.WriteFile(a.Path, []byte(a.Content))
				}
			case shellcore.ActionSetRange:
				jsCallbacks.Call("onRangeChange", shellcore.FormatRangePlain(a.Start, a.End))
			case shellcore.ActionQuit:
				jsCallbacks.Call("notifyQuit")
			}
		},
	)

	// Set up JS-backed virtual filesystem.
	jsfs := &jsFS{callbacks: jsCallbacks}
	core.SetFS(jsfs)

	// Wire weft's chart View, builtin, and pipe stage. The jsFS
	// satisfies chart.WatchResultsProvider (GetWatchResults), so the
	// chart code can fetch /cron.d/<watch> results via the same JS
	// callback path the rest of the FS uses.
	chart.Register(core, jsfs)

	// Set up history persistence via localStorage.
	loadFn := jsCallbacks.Get("loadHistory")
	if !loadFn.IsUndefined() && !loadFn.IsNull() {
		core.SetHistoryStore(&jsHistoryStore{callbacks: jsCallbacks})
	}

	// Set user + customer from JS callbacks if available — both feed
	// the prompt prefix as `<user>@<customer>`. JS supplies them from
	// AuthContext (email-local-part for user, claim["customer"] for
	// customer) so the web shell shows the same identity as the rest
	// of the app.
	getUserFn := jsCallbacks.Get("getUser")
	if !getUserFn.IsUndefined() && !getUserFn.IsNull() {
		user := jsCallbacks.Call("getUser").String()
		if user != "" && user != "undefined" {
			core.SetUser(user)
		}
	}
	getCustomerFn := jsCallbacks.Get("getCustomer")
	if !getCustomerFn.IsUndefined() && !getCustomerFn.IsNull() {
		customer := jsCallbacks.Call("getCustomer").String()
		if customer != "" && customer != "undefined" {
			core.SetCustomer(customer)
		}
	}

	// Default range: last 24 hours.
	rangeStart := time.Now().Add(-24 * time.Hour)
	rangeEnd := time.Now()
	core.SetRange(rangeStart, rangeEnd)
	jsCallbacks.Call("onRangeChange", shellcore.FormatRangePlain(rangeStart, rangeEnd))

	// Render initial prompt.
	core.Redraw()

	return nil
}

// jsFS implements shellcore.FS and chart.WatchResultsProvider
// by calling JS callbacks that proxy to the API.
type jsFS struct {
	callbacks js.Value
}

func (f *jsFS) ReadFile(path string) ([]byte, error) {
	result := f.callbacks.Call("fsReadFile", path)
	if result.IsNull() || result.IsUndefined() {
		return nil, jsError("file not found: " + path)
	}
	if err := jsResultError(result); err != nil {
		return nil, err
	}
	return []byte(result.Get("content").String()), nil
}

func (f *jsFS) WriteFile(path string, data []byte) error {
	result := f.callbacks.Call("fsWriteFile", path, string(data))
	if result.IsNull() || result.IsUndefined() {
		return nil
	}
	return jsResultError(result)
}

func (f *jsFS) ReadDir(path string) ([]string, error) {
	result := f.callbacks.Call("fsReadDir", path)
	if result.IsNull() || result.IsUndefined() {
		return nil, jsError("directory not found: " + path)
	}
	if err := jsResultError(result); err != nil {
		return nil, err
	}
	entries := result.Get("entries")
	names := make([]string, entries.Length())
	for i := 0; i < entries.Length(); i++ {
		names[i] = entries.Index(i).String()
	}
	return names, nil
}

func (f *jsFS) Remove(path string) error {
	result := f.callbacks.Call("fsRemove", path)
	if result.IsNull() || result.IsUndefined() {
		return nil
	}
	return jsResultError(result)
}

func (f *jsFS) GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error) {
	result := f.callbacks.Call("fsGetWatchResults", name, limit)
	if result.IsNull() || result.IsUndefined() {
		return nil, jsError("no results")
	}
	if err := jsResultError(result); err != nil {
		return nil, err
	}
	items := result.Get("results")
	if items.IsUndefined() || items.IsNull() {
		return nil, nil
	}
	var out []chart.WatchResultValue
	for i := 0; i < items.Length(); i++ {
		item := items.Index(i)
		out = append(out, chart.WatchResultValue{
			Value: item.Get("stdout").String(),
			RanAt: item.Get("ran_at").String(),
		})
	}
	return out, nil
}

// jsResultError extracts the "error" field from a JS result object.
// Returns nil if the field is missing, undefined, null, or empty.
func jsResultError(result js.Value) error {
	errVal := result.Get("error")
	if errVal.IsUndefined() || errVal.IsNull() {
		return nil
	}
	s := errVal.String()
	if s == "" {
		return nil
	}
	return jsError(s)
}

// wasmHandleInput(data: string)
// data is raw terminal input from xterm.js onData.
func wasmHandleInput(_ js.Value, args []js.Value) any {
	data := args[0].String()
	events := parseXtermInput(data)
	for _, ev := range events {
		core.HandleEvent(ev)
	}
	return nil
}

// wasmHandleQueryResult(text: string, errMsg: string)
func wasmHandleQueryResult(_ js.Value, args []js.Value) any {
	text := args[0].String()
	errMsg := args[1].String()

	ev := shellcore.QueryResultEvent{Text: text}
	if errMsg != "" {
		ev.Err = jsError(errMsg)
	}
	core.HandleEvent(ev)
	return nil
}

// wasmHandleFollowRow(raw: string)
func wasmHandleFollowRow(_ js.Value, args []js.Value) any {
	raw := args[0].String()
	core.HandleEvent(shellcore.FollowRowEvent{Raw: raw})
	return nil
}

// wasmHandleFollowDone(errMsg: string)
func wasmHandleFollowDone(_ js.Value, args []js.Value) any {
	errMsg := args[0].String()
	ev := shellcore.FollowDoneEvent{}
	if errMsg != "" {
		ev.Err = jsError(errMsg)
	}
	core.HandleEvent(ev)
	return nil
}

// wasmResize(cols: number, rows: number)
func wasmResize(_ js.Value, args []js.Value) any {
	cols := args[0].Int()
	rows := args[1].Int()
	core.HandleEvent(shellcore.ResizeEvent{Width: cols, Height: rows})
	return nil
}

// wasmSpinnerTick()
func wasmSpinnerTick(_ js.Value, _ []js.Value) any {
	core.SpinnerTick()
	return nil
}

// wasmSetInput(text: string)
func wasmSetInput(_ js.Value, args []js.Value) any {
	text := args[0].String()
	core.SetInput(text)
	return nil
}

// wasmGetInput() string
func wasmGetInput(_ js.Value, _ []js.Value) any {
	return core.GetInput()
}

// ── xterm.js input parsing ──────────────────────────────────

// parseXtermInput converts raw xterm.js terminal input into shellcore KeyEvents.
// xterm.js delivers escape sequences for special keys.
func parseXtermInput(data string) []shellcore.KeyEvent {
	var events []shellcore.KeyEvent
	i := 0
	for i < len(data) {
		// CSI sequences: ESC [ ...
		if data[i] == '\x1b' && i+2 < len(data) && data[i+1] == '[' {
			consumed, ev := parseCSI(data[i+2:])
			if ev != nil {
				events = append(events, *ev)
			}
			i += 2 + consumed
			continue
		}

		// Bare ESC.
		if data[i] == '\x1b' {
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyEsc})
			i++
			continue
		}

		switch data[i] {
		case '\r', '\n':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyEnter})
			i++
		case '\t':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyTab})
			i++
		case '\x03': // Ctrl+C
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlC})
			i++
		case '\x04': // Ctrl+D
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlD})
			i++
		case '\x01': // Ctrl+A
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlA})
			i++
		case '\x05': // Ctrl+E
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlE})
			i++
		case '\x0b': // Ctrl+K
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlK})
			i++
		case '\x12': // Ctrl+R
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlR})
			i++
		case '\x15': // Ctrl+U
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlU})
			i++
		case '\x17': // Ctrl+W
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlW})
			i++
		case '\x7f': // Backspace (most terminals)
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyBackspace})
			i++
		case '\x08': // Backspace (alternative)
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyBackspace})
			i++
		default:
			// Printable character (may be multi-byte UTF-8).
			r, size := utf8.DecodeRuneInString(data[i:])
			if r != utf8.RuneError {
				events = append(events, shellcore.KeyEvent{Rune: r})
			}
			i += size
		}
	}
	return events
}

// parseCSI parses a CSI sequence (after ESC [). Returns bytes consumed and event.
func parseCSI(data string) (int, *shellcore.KeyEvent) {
	if len(data) == 0 {
		return 0, nil
	}

	switch data[0] {
	case 'A':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyUp}
	case 'B':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyDown}
	case 'C':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyRight}
	case 'D':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyLeft}
	case 'H': // Home
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyHome}
	case 'F': // End
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyEnd}
	case 'Z': // Shift+Tab
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyShiftTab}
	}

	// Sequences like ESC [ N ~ (PgUp, PgDn, Delete, Insert).
	if len(data) >= 2 && data[1] == '~' {
		switch data[0] {
		case '2':
			return 2, nil // Insert — ignored
		case '3':
			return 2, &shellcore.KeyEvent{Key: shellcore.KeyDelete}
		case '5':
			return 2, &shellcore.KeyEvent{Key: shellcore.KeyPgUp}
		case '6':
			return 2, &shellcore.KeyEvent{Key: shellcore.KeyPgDown}
		}
	}

	return 1, nil
}

// jsHistoryStore implements shellcore.HistoryStore via JS localStorage callbacks.
type jsHistoryStore struct {
	callbacks js.Value
}

func (s *jsHistoryStore) Load() ([]string, error) {
	result := s.callbacks.Call("loadHistory")
	if result.IsNull() || result.IsUndefined() {
		return nil, nil
	}
	text := result.String()
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var entries []string
	for _, line := range lines {
		if line != "" {
			entries = append(entries, line)
		}
	}
	return entries, nil
}

func (s *jsHistoryStore) Save(entries []string) error {
	text := strings.Join(entries, "\n") + "\n"
	s.callbacks.Call("saveHistory", text)
	return nil
}

// jsError implements the error interface for a JS error message.
type jsError string

func (e jsError) Error() string { return string(e) }
