package spanview

import (
	"fmt"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/tracecmd"
)

// Command returns the `trace` command for the two typed shapes that enter the
// SpanView:
//
//   - `trace --spans --id <id>`            — one request's breakdown
//   - `trace --spans [--name <span>]`      — every span name aggregated
//     over the current time range
//
// either optionally followed by a <namespace>[/<service>] scope. Any other
// shape is not the View's: it belongs to the server (see ParseSpansLine for
// which, and how the CLI then renders them). Which lines reach this command is
// decided by the shell's remote hook, not here: the embedder registers
// Claims for "trace" so the other shapes are shipped before this could run.
// Run directly, the command refuses them.
//
// The View is entered on the event loop and outlives the run.
//
// Wire it via Register, or core.RegisterCommand("trace", spanview.Command(query)).
func Command(query QueryFunc) func(cx *shellcore.Cmd) int {
	return func(cx *shellcore.Cmd) int {
		listArgs, ok := ParseSpansLine(append([]string{"trace"}, cx.Args...))
		if !ok {
			fmt.Fprintln(cx.Stderr, shellcore.ErrorText("trace: not a span view: trace "+strings.Join(cx.Args, " ")))
			return 1
		}
		listArgs.Query = query
		if err := cx.OnLoop(func(c *shellcore.ShellCore) error { return c.EnterView(viewName, listArgs) }); err != nil {
			fmt.Fprintln(cx.Stderr, shellcore.ErrorText("span: "+err.Error()))
			return 1
		}
		return 0
	}
}

// Claims reports whether a `trace` line with these arguments (without the
// word itself) is one the SpanView enters. An embedder whose lines are shipped
// to a server registers it as the claim predicate for "trace", so every other
// shape (the recent-traces list, one request's lines, the `--samples` list, a
// grammar error) still goes to the server unchanged.
func Claims(args []string) bool {
	_, ok := ParseSpansLine(append([]string{"trace"}, args...))
	return ok
}

// ParseSpansLine reports whether parts (a command line split into words, with
// parts[0] == "trace") is one of the two shapes Command enters the SpanView
// for, returning the pipeline to run and the status-bar title. Any other
// shape — the recent-traces list, one request's lines, the `--samples` list, a
// piped line (a literal "|" token fails the scope check), or a grammar error —
// returns ok=false.
//
// Exported (not just for Command's own use) so other embedders can reuse the
// exact arg grammar rather than forking it; the grammar itself lives in the
// tracecmd package.
func ParseSpansLine(parts []string) (*ListArgs, bool) {
	if len(parts) == 0 || parts[0] != "trace" {
		return nil, false
	}
	cmd, err := tracecmd.Parse(parts[1:])
	if err != nil {
		return nil, false
	}
	if m := cmd.Mode(); m != tracecmd.ModeBreakdown && m != tracecmd.ModeSpans {
		return nil, false
	}
	return &ListArgs{Pipeline: cmd.String(), Title: cmd.Title()}, true
}

// Register wires spanview into the given shellcore at construction time: the
// View plus the `trace` command covering both span views.
func Register(c *shellcore.ShellCore, query QueryFunc) {
	c.RegisterView(NewView())
	c.RegisterCommand("trace", Command(query))
}
