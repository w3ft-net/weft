package shellcore

import (
	"strings"
	"time"
)

// FS is the virtual filesystem interface for the shell.
// ShellFS (server-side) and the WASM API adapter both implement this.
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadDir(path string) ([]string, error) // returns entry names
	Remove(path string) error
}

// PathKind classifies a resolved virtual FS path.
type PathKind int

const (
	PathUnknown  PathKind = iota // not a recognized FS path
	PathLocal                    // local FS object (/bin/, /cron.d/, /etc/) — read/write via FS
	PathQuery                    // query source (/log/) — dispatched to coordinator
)

// ClassifyPath returns the kind of a resolved absolute FS path.
func ClassifyPath(path string) PathKind {
	switch {
	case strings.HasPrefix(path, "/log/") || path == "/log":
		return PathQuery
	case strings.HasPrefix(path, "/bin/") || path == "/bin",
		strings.HasPrefix(path, "/cron.d/") || path == "/cron.d",
		strings.HasPrefix(path, "/etc/") || path == "/etc",
		path == "/":
		return PathLocal
	default:
		return PathUnknown
	}
}

// LangFS is optionally implemented by FS implementations that know
// the scripting language of files (e.g., "bash" or "lua").
type LangFS interface {
	FileLanguage(path string) string
}

// WatchResultValue represents a single cron watch result for dashboard display.
type WatchResultValue struct {
	Value string // stdout, trimmed
	RanAt string // RFC3339
}

// WatchResultsProvider can fetch recent results for a watch.
// Optional — if the FS also implements this, the dashboard can show sparklines.
type WatchResultsProvider interface {
	GetWatchResults(name string, limit int) ([]WatchResultValue, error)
}

// Event is any input event from the renderer to the core.
type Event interface{ shellEvent() }

// SpecialKey identifies non-printable keys.
type SpecialKey int

const (
	KeyNone SpecialKey = iota
	KeyEnter
	KeyBackspace
	KeyDelete
	KeyTab
	KeyShiftTab
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyCtrlA
	KeyCtrlC
	KeyCtrlD
	KeyCtrlE
	KeyCtrlK
	KeyCtrlR
	KeyCtrlU
	KeyCtrlW
	KeyEsc
	KeyPgUp
	KeyPgDown
)

// KeyEvent represents a keystroke.
type KeyEvent struct {
	Key  SpecialKey
	Rune rune
}

func (KeyEvent) shellEvent() {}

// ResizeEvent reports terminal dimensions.
type ResizeEvent struct {
	Width, Height int
}

func (ResizeEvent) shellEvent() {}

// QueryResultEvent delivers results from the network layer.
type QueryResultEvent struct {
	Text string
	Rows []OutputRow
	Err  error
}

func (QueryResultEvent) shellEvent() {}

// FollowRowEvent delivers a single streaming row.
type FollowRowEvent struct {
	Raw string
}

func (FollowRowEvent) shellEvent() {}

// FollowDoneEvent signals end of follow stream.
type FollowDoneEvent struct {
	Err error
}

func (FollowDoneEvent) shellEvent() {}

// OutputRow is a renderer-friendly log row (no protobuf dependency).
type OutputRow struct {
	Raw string
}

// Mode represents the shell's current input mode.
type Mode int

const (
	ModeInput     Mode = iota // accepting user input
	ModeRunning               // query in progress
	ModeFollowing             // tail -f streaming
	ModeSearching             // reverse-i-search
	ModeVim                   // editing a file with the vim emulator
	ModeCapture               // capturing stdin for redirect (cat > /path)
)

// Action is a request from the core to the renderer for async work.
type Action interface{ shellAction() }

// ActionQuery requests execution of a one-shot pipeline.
type ActionQuery struct{ Pipeline string }

func (ActionQuery) shellAction() {}

// ActionFollow requests a tail -f streaming subscription.
type ActionFollow struct{ Pipeline string }

func (ActionFollow) shellAction() {}

// ActionCancel requests cancellation of the current query/follow.
type ActionCancel struct{}

func (ActionCancel) shellAction() {}

// ActionQuit requests the shell to exit.
type ActionQuit struct{}

func (ActionQuit) shellAction() {}

// ActionPipeToTerminal requests piping text to an external terminal command.
type ActionPipeToTerminal struct {
	Text    string
	Command string
}

func (ActionPipeToTerminal) shellAction() {}

// ActionVimSave requests writing a file to the virtual FS.
type ActionVimSave struct {
	Path    string
	Content string
}

func (ActionVimSave) shellAction() {}

// ActionVimClose requests exiting vim mode and returning to shell input.
type ActionVimClose struct{}

func (ActionVimClose) shellAction() {}

// ActionSetRange requests changing the query time range.
type ActionSetRange struct {
	Start time.Time
	End   time.Time
}

func (ActionSetRange) shellAction() {}

// VimFileLoadedEvent delivers file content for the vim editor.
type VimFileLoadedEvent struct {
	Path    string
	Content string
	Err     error
}

func (VimFileLoadedEvent) shellEvent() {}
