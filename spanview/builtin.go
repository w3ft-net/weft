package spanview

import (
	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/tracecmd"
)

// Builtin returns a shellcore.BuiltinFunc that intercepts two typed
// shapes before they'd otherwise dispatch as a normal query pipeline,
// entering the SpanView instead:
//
//   - `trace --spans --id <id>`            — one request's breakdown
//   - `trace --spans [--name <span>]`      — every span name aggregated
//     over the current time range
//
// either optionally followed by a <namespace>[/<service>] scope. Any other
// line returns false so it falls through to the shell's normal pipeline
// dispatch unchanged: see ParseSpansLine for which, and how the CLI then
// renders them.
//
// Wire it via core.RegisterBuiltin("trace", spanview.Builtin(query)).
func Builtin(query QueryFunc) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, parts []string) bool {
		listArgs, ok := ParseSpansLine(parts)
		if !ok {
			return false
		}
		listArgs.Query = query
		if err := c.EnterView(viewName, listArgs); err != nil {
			c.AppendScrollback(shellcore.ErrorText("span: "+err.Error()) + "\n")
		}
		return true
	}
}

// ParseSpansLine reports whether parts (a builtin dispatch's
// whitespace-split command line) is one of the two shapes Builtin enters
// the SpanView for, returning the pipeline to run and the status-bar
// title. Any other shape — the recent-traces list, one request's lines,
// the `--samples` list, a piped line (shellcore hands the whole line's
// fields here, so a literal "|" token fails the scope check), or a grammar
// error — returns ok=false.
//
// Exported (not just for Builtin's own use) so other embedders can reuse
// the exact arg grammar rather than forking it; the grammar itself lives in
// the tracecmd package.
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

// Register wires spanview into the given shellcore at construction
// time — the View plus the `trace` builtin covering both span views.
func Register(c *shellcore.ShellCore, query QueryFunc) {
	c.RegisterView(NewView())
	c.RegisterBuiltin("trace", Builtin(query))
}
