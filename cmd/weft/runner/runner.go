// Package runner drives the interactive CLI shell: the logo-dissolve intro, the
// file-backed history store, and weft's wiring of the core. The terminal, the
// event loop and the queue queries and follows post back through (they run on
// goroutines the shellcore starts, see shell.installRemote) are warp/term's.
//
// All wasm-incompatible plumbing lives here so the shell/ package
// stays buildable under GOOS=js/GOARCH=wasm. The browser embedder
// (cmd/wasm) provides its own input loop via xterm.js + wasm.Boot
// exports.
package runner

import (
	"context"
	"os"
	"strings"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/graybeardcorp/warp/pkg/term"
	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/cmd/weft/dashview"
	"github.com/w3ft-net/weft/shell"
	"github.com/w3ft-net/weft/spanview"
	"github.com/w3ft-net/weft/theme"
)

// Run drives the interactive CLI shell. The caller wires up s.Query,
// s.Follow, s.FS, etc. and then hands the shell over; Run returns
// when the user quits (Ctrl+D, :q, or core.Quitting()).
//
// The terminal, the event loop and everything that feeds it (keys, SIGWINCH,
// the spinner tick, the posts queries and follows make from their goroutines)
// are warp/term's. What is left here is weft: building the core, the intro,
// and answering the few actions only the embedder can.
func Run(s *shell.Shell) error {
	return run(context.Background(), s, true)
}

// run is Run with the terminal's inputs replaceable (term.Input and friends)
// and the intro optional, so a test can drive the whole shell.
func run(ctx context.Context, s *shell.Shell, withIntro bool, opts ...term.Option) error {
	r := &runner{shell: s}
	opts = append([]term.Option{term.OnAction(func(_ *term.Host, a shellcore.Action) { r.handleAction(a) })}, opts...)
	return term.Run(ctx, func(core *shellcore.Shell) {
		r.core = core
		// Queries run on goroutines the core starts and return to the loop
		// through Post. The query callback reads the width from its own
		// goroutine; Host.Size is safe for that.
		host := core.Host()
		s.Width = func() int { w, _ := host.Size(); return w }
		r.configure()
		if withIntro {
			r.intro(host)
		}
	}, opts...)
}

// configure wires weft into the core: history, the shell's own commands, and
// the CLI-side renderers.
func (r *runner) configure() {
	s := r.shell
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
		if src, ok := s.FS.(chart.CronSource); ok && s.Chart == nil {
			chart.Register(r.core, src, func() string { return s.Range })
		}
		if dp, ok := s.FS.(dashview.Provider); ok && s.Dashboard == nil {
			dashview.Register(r.core, dp)
		}
	}
	// Span display's interactive rollup View — both shapes of
	// docs/plans/span-display.md (`trace --spans --id <id>` and `trace
	// --spans [--name <span>]`), while a bare `trace` (the recent-traces
	// list) and `trace --id <id>` are yielded to the server (spanview.Claims
	// is the claim predicate registered below) and rendered by
	// RenderTraceIfApplicable (shell.go). Grouped with chart/dashboard above for the same
	// wasm-build reason: this needs shellcore's View machinery, which
	// stays out of shell/'s import graph (shell/ imports spanview only
	// for its ParseSpansLine arg-grammar helper, not the View).
	// spanview.Command already hands us the complete pipeline to run
	// (see spanview/builtin.go's ParseSpansLine) — Query wraps s.Query
	// directly, no prefixing. Gated on s.Trace == nil, matching the
	// s.Chart == nil / s.Dashboard == nil checks above: the browser
	// embedder sets Shell.Trace and takes over all three `trace` shapes
	// itself (see shell.traceCommand), so the CLI's in-terminal
	// View should never register alongside it.
	if s.Query != nil && s.Trace == nil {
		spanview.Register(r.core, func(pipeline string) ([]string, error) {
			result, err := s.Query(pipeline, s.Range, r.core.DirStack())
			if err != nil {
				return nil, err
			}
			lines := make([]string, len(result.Rows))
			for i, row := range result.Rows {
				lines[i] = row.Raw
			}
			return lines, nil
		})
		// The SpanView takes only the two span shapes; every other `trace`
		// line still ships to the server.
		s.Claim("trace", spanview.Claims)
	}
}

// intro runs once, in raw mode, before the first prompt.
func (r *runner) intro(h shellcore.Host) {
	// Run on the main screen so the terminal emulator's native
	// scrollback works (cmd+up in iTerm/Terminal/wezterm/kitty/etc.)
	// — altscreen is the right call for vim/less, which manage their
	// own 1049h/1049l lifecycle in shellcore. Show the cursor: the
	// raw-mode hop hides it on some terminals.
	h.Write([]byte("\x1b[?25h"))

	// Prefetch directory listings while the logo dissolves.
	if pfs, ok := r.shell.FS.(*shell.ProxyFS); ok {
		go pfs.Prefetch()
	}
	w, ht := h.Size()
	r.dissolve(w, ht)
}

// runner holds mutable state for the running CLI shell.
type runner struct {
	shell *shell.Shell
	core  *shellcore.Shell
}

// handleAction answers the few requests only the embedder can fulfil. Running
// a line is not one of them: the core does that itself (shell.installRemote).
func (r *runner) handleAction(a shellcore.Action) {
	switch a.(type) {
	case shellcore.ActionQuit:
		// Handled by checking core.Quitting() in the event loop.
	}
}

// RenderNotice formats a notice into a labeled block ready to be
// shown in the terminal. Kept for callers of this package; the
// implementation lives in shell.RenderQueryNotice.
func RenderNotice(n shell.QueryResultNotice) string { return shell.RenderQueryNotice(n) }

// ── Logo dissolve ───────────────────────────────────────────

func (r *runner) dissolve(w, h int) {
	logo := theme.RenderLogo()
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
