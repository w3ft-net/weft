package shell

import (
	"context"
	"fmt"
	"github.com/graybeardcorp/warp-lua/luashell"
	"io"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// This file is how a line typed into the weft shell reaches the server.
//
// The core runs every line through a local pkg/bash interpreter. A Root
// ParseFn (bash.RemoteRewrite) replaces each run of server commands with one
// CallbackCommand that ships its text to the server (Shell.Query, or
// Shell.Follow for `tail -f`) and writes the answer to stdout. The server is a
// real pkg/bash interpreter that owns all shell semantics, so a line made only
// of server commands goes out exactly as typed (Script.Source).
//
// Client commands stay on the client wherever they sit in the line: `| chart`,
// `| less`, `range 1h; query ...`, `query ... | chart | grep x`. They run
// locally on what comes back, which is why a stage can open a modal or a
// pager. Which side a command is on is decided by classify, below.
//
// The callback runs on the core's run goroutine (see shellcore.SetPost): it
// must not touch the core, it blocks on Query, and it gives up when the run's
// context is cancelled. What a local stage pipes into a server segment is not
// sent: the server's Query takes only text.

// installRemote switches the core onto remote execution. It is called by
// ConfigureCore when the Shell has a Query.
func (s *Shell) installRemote(core *shellcore.Shell) {
	// The client interpreter only ever runs the callback and a trailing
	// client stage, so almost nothing needs to exist in it. The allow-list
	// also gates the core's own `lua`, `psql` and `fc` clients, which weft
	// has always offered.
	cfg := core.Config()
	cfg.AllowedCommands = []string{"less", "lua", "fc"}
	cfg.Setup = func(_ context.Context, interp *bash.Interpreter, _ io.Writer, _ io.Reader) {
		luashell.Register(interp)
	}
	core.SetConfig(cfg)
	core.UseParse(func(o bash.ParseOrigin, script *bash.Script) {
		// Only what the user typed: scripts and evals the interpreter reads
		// later run wherever they run.
		if o.Kind != bash.ParseRoot {
			return
		}
		bash.RemoteRewrite(s.classify(core, isSoleCommand(script)), s.ship)(o, script)
	})
}

// isSoleCommand is whether script is exactly one simple command: no pipe, no
// chain, no second statement.
func isSoleCommand(script *bash.Script) bool {
	if len(script.Commands) != 1 {
		return false
	}
	l := script.Commands[0]
	if l == nil || len(l.Rest) != 0 || l.First == nil || len(l.First.Commands) != 1 {
		return false
	}
	_, ok := l.First.Commands[0].(*bash.SimpleCommand)
	return ok
}

// classify says which side runs a command: the client for what the core
// handles (registered commands, pipe stages, `less`), the server for
// everything else. A command the embedder registered a Claim for is the
// client's only as a line by itself (sole) and only for arguments its claim
// accepts; piped or chained, its rows are the server's to shape.
func (s *Shell) classify(core *shellcore.Shell, sole bool) bash.Classify {
	local := core.Classify()
	return func(name string, args []string) bash.Side {
		if local(name, args) == bash.Remote {
			return bash.Remote
		}
		if s.claimFns[name] != nil && !(sole && s.claims(name, args)) {
			return bash.Remote
		}
		return bash.Local
	}
}

// ship sends text, one segment of a line, to the server and writes the answer
// to stdout. It is the bash.ShipFn for RemoteRewrite.
func (s *Shell) ship(ctx context.Context, text string, stack []string, _ io.Reader, stdout, stderr io.Writer) bash.ShipResult {
	if isFollowPipeline(text) && s.Follow != nil {
		fmt.Fprintln(stdout, shellcore.DimText("[streaming — Ctrl+C to stop]"))
		return bash.ShipResult{Exit: s.runFollow(ctx, text, stdout, stderr)}
	}

	// Query cannot be interrupted, so wait for it in a goroutine and
	// stop waiting when the run is cancelled. The buffered channel
	// lets the late answer go nowhere.
	type answer struct {
		res *QueryResult
		err error
	}
	rng := s.Range // read now: a `range` typed after a Ctrl+C must not race it
	ch := make(chan answer, 1)
	go func() {
		var res *QueryResult
		var err error
		if s.QueryCtx != nil {
			res, err = s.QueryCtx(ctx, text, rng, stack)
		} else {
			res, err = s.Query(text, rng, stack)
		}
		ch <- answer{res, err}
	}()
	var a answer
	select {
	case a = <-ch:
	case <-ctx.Done():
		return bash.ShipResult{Exit: 130}
	}

	if a.err != nil {
		fmt.Fprintln(stderr, shellcore.ErrorText(shellcore.CleanError(a.err.Error())))
		return bash.ShipResult{Exit: 1}
	}
	res := a.res
	if res == nil {
		return bash.ShipResult{}
	}
	s.writeResult(text, res, stdout)
	// The server may have changed directory; RemoteRewrite applies this stack
	// before the next command, and the core when the run completes, so the
	// next prompt shows it.
	return bash.ShipResult{DirStack: res.FinalDirStack}
}

// writeResult renders a query result: one-time notices first, then the
// output as text, a trace table, or raw rows.
func (s *Shell) writeResult(pipeline string, res *QueryResult, w io.Writer) {
	for _, n := range res.Notices {
		io.WriteString(w, RenderQueryNotice(n))
	}
	if res.Text != "" {
		io.WriteString(w, res.Text)
		return
	}
	if len(res.Rows) == 0 {
		return
	}
	width := s.TermWidth
	if s.Width != nil {
		width = s.Width()
	}
	if rendered, ok := RenderTraceIfApplicable(pipeline, res, width); ok {
		io.WriteString(w, strings.Join(rendered, "\n")+"\n")
		return
	}
	for _, row := range res.Rows {
		io.WriteString(w, row.Raw+"\n")
	}
}

func (s *Shell) runFollow(ctx context.Context, head string, stdout, stderr io.Writer) int {
	err := s.Follow(ctx, head, s.Range, func(row Row) {
		io.WriteString(stdout, row.Raw+"\n")
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, shellcore.ErrorText(shellcore.CleanError(err.Error())))
		return 1
	}
	if ctx.Err() != nil {
		return 130
	}
	return 0
}

// RenderQueryNotice formats a notice into a labeled block ready to be shown
// in the terminal. ANSI color picked by Kind; Body is left as-is. The
// trailing rule separates it from the output that follows.
func RenderQueryNotice(n QueryResultNotice) string {
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
