package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shellcore"
)

// QueryResult is the output of a query — either result rows or text.
type QueryResult struct {
	Rows []*pb.ResultRow
	Text string
}

// QueryFunc is the function signature the shell uses to execute queries.
type QueryFunc func(pipeline string, start, end time.Time) (*QueryResult, error)

// FollowFunc is the function signature for tail -f streaming.
type FollowFunc func(ctx context.Context, pipeline string, onRow func(*pb.ResultRow)) error

// Shell is the weft interactive shell.
type Shell struct {
	Query  QueryFunc
	Follow FollowFunc
	Start  time.Time
	End    time.Time
	Apps   []string
	FS     shellcore.FS
	User   string // username shown in prompt (e.g. "paul")
}

// Run starts the interactive shell.
func (s *Shell) Run() error {
	// Put terminal in raw mode.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Switch to alt screen and show cursor.
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25h")
	defer os.Stdout.WriteString("\x1b[?1049l") // restore main screen on exit

	w, h, _ := term.GetSize(int(os.Stdout.Fd()))
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}

	runner := &shellRunner{shell: s}
	runner.core = shellcore.NewShellCore(
		s.Apps, w, h,
		func(b []byte) { os.Stdout.Write(b) },
		func(a shellcore.Action) { runner.handleAction(a) },
	)
	if s.FS != nil {
		runner.core.SetFS(s.FS)
	}
	if s.User != "" {
		runner.core.SetUser(s.User)
	}
	runner.core.SetRange(s.Start, s.End)

	// Handle window resize.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	go func() {
		for range sigCh {
			w, h, _ := term.GetSize(int(os.Stdout.Fd()))
			if w > 0 && h > 0 {
				runner.core.HandleEvent(shellcore.ResizeEvent{Width: w, Height: h})
			}
		}
	}()

	// Logo dissolve animation.
	runner.dissolve(w, h)

	// Show initial prompt.
	runner.core.Redraw()

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
				runner.core.SpinnerTick()
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
		events := parseTermInput(buf[:n])
		for _, ev := range events {
			runner.core.HandleEvent(ev)
		}
		if runner.core.Quitting() {
			break
		}
	}

	return nil
}

// shellRunner holds mutable state for the running shell.
type shellRunner struct {
	shell        *Shell
	core         *shellcore.ShellCore
	followCancel context.CancelFunc
}

func (r *shellRunner) handleAction(a shellcore.Action) {
	switch a := a.(type) {
	case shellcore.ActionQuery:
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
	case shellcore.ActionSetRange:
		r.shell.Start = a.Start
		r.shell.End = a.End
	case shellcore.ActionQuit:
		// Handled by checking core.Quitting() in the read loop.
	}
}

func (r *shellRunner) runQuery(pipeline string) {
	weftPipeline, termCmd := splitTerminalStage(pipeline)

	result, err := r.shell.Query(weftPipeline, r.shell.Start, r.shell.End)

	ev := shellcore.QueryResultEvent{Err: err}
	if err == nil {
		if termCmd != "" {
			var text string
			if result.Text != "" {
				text = result.Text
			} else {
				var sb strings.Builder
				for _, row := range result.Rows {
					sb.WriteString(row.Raw)
					sb.WriteByte('\n')
				}
				text = sb.String()
			}
			pipeToTerminal(text, termCmd)
		} else if result.Text != "" {
			ev.Text = result.Text
		} else {
			for _, row := range result.Rows {
				ev.Rows = append(ev.Rows, shellcore.OutputRow{Raw: row.Raw})
			}
		}
	}

	r.core.HandleEvent(ev)
}

func (r *shellRunner) runFollow(pipeline string) {
	ctx, cancel := context.WithCancel(context.Background())
	r.followCancel = cancel

	if r.shell.Follow != nil {
		r.shell.Follow(ctx, pipeline, func(row *pb.ResultRow) {
			r.core.HandleEvent(shellcore.FollowRowEvent{Raw: row.Raw})
		})
	}

	r.core.HandleEvent(shellcore.FollowDoneEvent{})
}

// ── Logo dissolve ───────────────────────────────────────────

func (r *shellRunner) dissolve(w, h int) {
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

// ── Terminal input parsing ──────────────────────────────────

func parseTermInput(data []byte) []shellcore.KeyEvent {
	var events []shellcore.KeyEvent
	s := string(data)
	i := 0
	for i < len(s) {
		// CSI sequences: ESC [ ...
		if s[i] == '\x1b' && i+2 < len(s) && s[i+1] == '[' {
			consumed, ev := parseCSI(s[i+2:])
			if ev != nil {
				events = append(events, *ev)
			}
			i += 2 + consumed
			continue
		}
		// Bare ESC.
		if s[i] == '\x1b' {
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyEsc})
			i++
			continue
		}
		switch s[i] {
		case '\r', '\n':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyEnter})
		case '\t':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyTab})
		case '\x01':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlA})
		case '\x03':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlC})
		case '\x04':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlD})
		case '\x05':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlE})
		case '\x0b':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlK})
		case '\x12':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlR})
		case '\x15':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlU})
		case '\x17':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyCtrlW})
		case ' ':
			events = append(events, shellcore.KeyEvent{Rune: ' '})
		case '\x7f', '\x08':
			events = append(events, shellcore.KeyEvent{Key: shellcore.KeyBackspace})
		default:
			if s[i] >= 32 && s[i] < 127 {
				events = append(events, shellcore.KeyEvent{Rune: rune(s[i])})
			}
		}
		i++
	}
	return events
}

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
	case 'H':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyHome}
	case 'F':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyEnd}
	case 'Z':
		return 1, &shellcore.KeyEvent{Key: shellcore.KeyShiftTab}
	}
	if len(data) >= 2 && data[1] == '~' {
		switch data[0] {
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

// ── Terminal helpers ────────────────────────────────────────

func splitTerminalStage(pipeline string) (weftPart, termPart string) {
	terminalCommands := map[string]bool{"less": true, "more": true}
	idx := strings.LastIndex(pipeline, "|")
	if idx == -1 {
		return pipeline, ""
	}
	lastStage := strings.TrimSpace(pipeline[idx+1:])
	cmd := strings.Fields(lastStage)
	if len(cmd) > 0 && terminalCommands[cmd[0]] {
		return strings.TrimSpace(pipeline[:idx]), lastStage
	}
	return pipeline, ""
}

func pipeToTerminal(text, cmdStr string) {
	parts := strings.Fields(cmdStr)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "pager error: %v\n", err)
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

// ExecNonInteractive runs a single command through the shell and prints to w.
// Supports all builtins (ls, cat /etc/, range, etc.) as well as queries.
func (s *Shell) ExecNonInteractive(line string, w io.Writer) error {
	line = strings.TrimSpace(line)

	// Handle range locally (modifies shell state).
	if line == "range" {
		fmt.Fprint(w, shellcore.FormatRangePlain(s.Start, s.End))
		return nil
	}
	if strings.HasPrefix(line, "range ") {
		start, end, err := shellcore.ParseRange(strings.TrimPrefix(line, "range "))
		if err != nil {
			return err
		}
		s.Start = start
		s.End = end
		fmt.Fprint(w, shellcore.FormatRangePlain(start, end))
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

	// Query pipeline.
	weftPipeline, _ := splitTerminalStage(line)
	result, err := s.Query(weftPipeline, s.Start, s.End)
	if err != nil {
		return err
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
			sb.WriteString(e)
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
	}

	return false, ""
}
