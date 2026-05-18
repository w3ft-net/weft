// Package runner drives the interactive CLI shell loop: raw-mode tty,
// SIGWINCH-driven resize, stdin event loop, the logo-dissolve intro,
// the action handler that fans queries/follows out to the shell.Query
// and shell.Follow callbacks, and the file-backed history store.
//
// All wasm-incompatible plumbing lives here so the shell/ package
// stays buildable under GOOS=js/GOARCH=wasm. The browser embedder
// (cmd/wasm) provides its own input loop via xterm.js + wasm.Boot
// exports.
package runner

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/warp/pkg/shellcore/wasm"
	"github.com/kevlarcon/weft/chart"
	"github.com/kevlarcon/weft/dashboard"
	"github.com/kevlarcon/weft/shell"
)

// Run drives the interactive CLI shell. The caller wires up s.Query,
// s.Follow, s.FS, etc. and then hands the shell over; Run returns
// when the user quits (Ctrl+D, :q, or core.Quitting()).
func Run(s *shell.Shell) error {
	// Put terminal in raw mode.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Run on the main screen so the terminal emulator's native
	// scrollback works (cmd+up in iTerm/Terminal/wezterm/kitty/etc.)
	// — altscreen is the right call for vim/less, which manage their
	// own 1049h/1049l lifecycle in shellcore. Show the cursor (the
	// raw-mode hop hides it on some terminals) and emit a final
	// newline on exit so the post-shell prompt lands on a clean row.
	os.Stdout.WriteString("\x1b[?25h")
	defer os.Stdout.WriteString("\n")

	w, h, _ := term.GetSize(int(os.Stdout.Fd()))
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}

	r := &runner{shell: s}
	r.core = shellcore.NewShellCore(
		s.Apps, w, h,
		func(b []byte) { os.Stdout.Write(b) },
		func(a shellcore.Action) { r.handleAction(a) },
	)
	// History is CLI-specific (file at ~/.weft_history). The wasm
	// bridge installs a JS-backed store in wasm.Boot, so the shared
	// ConfigureCore stays history-agnostic.
	r.core.SetHistoryStore(NewFileHistoryStore())
	s.ConfigureCore(r.core)

	// CLI-side fallback registration for the in-terminal `chart` and
	// `dashboard` renderers. Lives here rather than inside
	// ConfigureCore so `shell/` doesn't pull `dashboard` (and its
	// transitive `store` dep) into the WASM build for code the
	// browser path never runs.
	if s.FS != nil {
		if wr, ok := s.FS.(chart.WatchResultsProvider); ok && s.Chart == nil {
			chart.Register(r.core, wr)
		}
		if dp, ok := s.FS.(dashboard.Provider); ok && s.Dashboard == nil {
			dashboard.Register(r.core, dp)
		}
	}

	// Prefetch directory listings while the logo dissolves.
	if pfs, ok := s.FS.(*shell.ProxyFS); ok {
		go pfs.Prefetch()
	}

	// Handle window resize.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	go func() {
		for range sigCh {
			w, h, _ := term.GetSize(int(os.Stdout.Fd()))
			if w > 0 && h > 0 {
				r.core.HandleEvent(shellcore.ResizeEvent{Width: w, Height: h})
			}
		}
	}()

	// Logo dissolve animation.
	r.dissolve(w, h)

	// Show initial prompt.
	r.core.Redraw()

	// Spinner ticker.
	spinnerDone := make(chan struct{})
	defer close(spinnerDone)
	go func() {
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-spinnerDone:
				return
			case <-ticker.C:
				r.core.SpinnerTick()
			}
		}
	}()

	// Read input.
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			break
		}
		events := wasm.ParseXtermInput(string(buf[:n]))
		for _, ev := range events {
			r.core.HandleEvent(ev)
		}
		if r.core.Quitting() {
			break
		}
	}

	return nil
}

// runner holds mutable state for the running CLI shell.
type runner struct {
	shell        *shell.Shell
	core         *shellcore.ShellCore
	followCancel context.CancelFunc
}

func (r *runner) handleAction(a shellcore.Action) {
	switch a := a.(type) {
	case shellcore.ActionExec:
		go r.runQuery(a.Pipeline)
	case shellcore.ActionFollow:
		go r.runFollow(a.Pipeline)
	case shellcore.ActionCancel:
		if r.followCancel != nil {
			r.followCancel()
			r.followCancel = nil
		}
	case shellcore.ActionVimSave:
		if r.shell.FS != nil {
			r.shell.FS.WriteFile(a.Path, []byte(a.Content))
		}
	case shellcore.ActionQuit:
		// Handled by checking core.Quitting() in the read loop.
	}
}

func (r *runner) runQuery(pipeline string) {
	// The shell core already stripped `| less` before dispatching this
	// pipeline — whatever comes back goes straight to the core, which
	// routes into ModeLess when the user typed the pager suffix.
	result, err := r.shell.Query(pipeline, r.shell.Range)

	ev := shellcore.ExecResultEvent{Err: err}
	if err == nil {
		// Notices are prepended to the rendered output as a labeled
		// block. They originate as one-time per-user system messages
		// (e.g. /tmp 48h expiry) and the shell surfaces them once
		// before any command output renders.
		var notices string
		for _, n := range result.Notices {
			notices += RenderNotice(n)
		}
		if result.Text != "" || notices != "" {
			ev.Text = notices + result.Text
		}
		if result.Text == "" && len(result.Rows) > 0 {
			for _, row := range result.Rows {
				ev.Rows = append(ev.Rows, shellcore.ResultRow{Raw: row.Raw})
			}
			// Notices alongside row-shaped output: prepend as a
			// synthetic Text payload that renders before rows.
			if notices != "" {
				ev.Text = notices
			}
		}
	}

	r.core.HandleEvent(ev)
}

// RenderNotice formats a notice into a labeled block ready to be
// shown in the terminal. ANSI color picked by Kind; Body is left
// as-is. Trailing rule provides visual separation from output.
// Exported so tests in this package can pin the formatting contract.
func RenderNotice(n shell.QueryResultNotice) string {
	var color string
	switch n.Kind {
	case "success":
		color = "\033[38;5;2m" // green
	case "error":
		color = "\033[38;5;1m" // red
	default: // warning / unknown
		color = "\033[38;5;3m" // yellow
	}
	const reset = "\033[0m"
	const rule = "─────────────────────────────────────────────────────\n"
	return rule + color + n.Title + reset + "\n" + n.Body + "\n" + rule + "\n"
}

func (r *runner) runFollow(pipeline string) {
	ctx, cancel := context.WithCancel(context.Background())
	r.followCancel = cancel

	if r.shell.Follow != nil {
		r.shell.Follow(ctx, pipeline, r.shell.Range, func(row shell.Row) {
			r.core.HandleEvent(shellcore.FollowRowEvent{Raw: row.Raw})
		})
	}

	r.core.HandleEvent(shellcore.FollowDoneEvent{})
}

// ── Logo dissolve ───────────────────────────────────────────

func (r *runner) dissolve(w, h int) {
	logo := shellcore.RenderLogo()
	logoLines := strings.Split(strings.TrimRight(logo, "\n"), "\n")
	if len(logoLines) == 0 {
		return
	}

	rows := len(logoLines)
	logoWidth := shellcore.VisualWidth(logoLines[0])
	topPad := (h - rows) / 2
	leftPad := (w - logoWidth) / 2
	if topPad < 0 {
		topPad = 0
	}
	if leftPad < 0 {
		leftPad = 0
	}
	padStr := strings.Repeat(" ", leftPad)

	renderFrame := func(phase int) {
		os.Stdout.WriteString("\x1b[H") // home
		for i := 0; i < topPad; i++ {
			os.Stdout.WriteString("\r\n")
		}
		for row, line := range logoLines {
			os.Stdout.WriteString(padStr)
			if phase <= 4 {
				if phase >= row {
					os.Stdout.WriteString(stripWarp(line))
				} else {
					os.Stdout.WriteString(line)
				}
			} else {
				weftPhase := phase - 5
				if weftPhase >= row {
					os.Stdout.WriteString(stripBoth(line))
				} else {
					os.Stdout.WriteString(stripWarp(line))
				}
			}
			os.Stdout.WriteString("\r\n")
		}
	}

	// Clear screen and show full logo.
	os.Stdout.WriteString("\x1b[2J")
	renderFrame(-1) // -1 = full logo, no dissolve
	time.Sleep(400 * time.Millisecond)

	// Dissolve phases.
	for phase := 0; phase < 10; phase++ {
		renderFrame(phase)
		time.Sleep(100 * time.Millisecond)
	}
}

// ── Logo dissolve helpers ───────────────────────────────────

// stripWarp removes warp characters (║) from a logo line, replacing with spaces.
func stripWarp(line string) string {
	var b strings.Builder
	runes := []rune(line)
	i := 0
	for i < len(runes) {
		if runes[i] == '\033' {
			escStart := i
			i++
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
			if i < len(runes) {
				i++ // skip 'm'
			}
			if i < len(runes) && runes[i] == '║' {
				b.WriteByte(' ')
				i++
				if i < len(runes) && runes[i] == '\033' {
					for i < len(runes) && runes[i] != 'm' {
						i++
					}
					if i < len(runes) {
						i++
					}
				}
			} else {
				b.WriteString(string(runes[escStart:i]))
			}
			continue
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

// stripBoth removes both warp (║) and weft (═) characters, leaving only spaces.
func stripBoth(line string) string {
	var b strings.Builder
	runes := []rune(line)
	i := 0
	for i < len(runes) {
		if runes[i] == '\033' {
			for i < len(runes) && runes[i] != 'm' {
				i++
			}
			if i < len(runes) {
				i++
			}
			if i < len(runes) && (runes[i] == '║' || runes[i] == '═') {
				b.WriteByte(' ')
				i++
				if i < len(runes) && runes[i] == '\033' {
					for i < len(runes) && runes[i] != 'm' {
						i++
					}
					if i < len(runes) {
						i++
					}
				}
			}
			continue
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}
