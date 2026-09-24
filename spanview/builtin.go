package spanview

import (
	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// Builtin returns a shellcore.BuiltinFunc that intercepts `trace <id>
// --spans` typed at the prompt, before it would otherwise dispatch as a
// normal query pipeline — entering the SpanView instead of the flat
// gap-histogram rendering a bare `trace <id>` gets. Any other line
// returns false so it falls through to the shell's normal pipeline
// dispatch unchanged: plain `trace <id>` (no --spans among parts), and
// a piped `trace <id> --spans | grep ...` too — shellcore hands the
// whole line's fields here including a literal "|" token, so that case
// fails the "exactly one non-flag argument" check below and ships to
// the server like any other pipeline, --spans stripped there by
// planner/parse.go same as a one-shot query.
//
// Wire it via core.RegisterBuiltin("trace", spanview.Builtin(query)).
func Builtin(query QueryFunc) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, parts []string) bool {
		traceID, ok := parseSpansTraceLine(parts)
		if !ok {
			return false
		}
		if err := c.EnterView(viewName, &ListArgs{TraceID: traceID, Query: query}); err != nil {
			c.AppendScrollback(shellcore.ErrorText("span: "+err.Error()) + "\n")
		}
		return true
	}
}

// parseSpansTraceLine reports whether parts (a builtin dispatch's
// whitespace-split command line) is exactly `trace <id> --spans` (the
// flag can appear in either position), returning the trace id. Any
// other shape — plain `trace <id>`, wrong arity, or a piped line
// (shellcore hands the whole line's fields here, so a literal "|"
// token lands in the non-flag remainder and fails the arity check) —
// returns ok=false.
func parseSpansTraceLine(parts []string) (traceID string, ok bool) {
	if len(parts) == 0 || parts[0] != "trace" {
		return "", false
	}
	var spansOnly bool
	var rest []string
	for _, p := range parts[1:] {
		if p == "--spans" {
			spansOnly = true
			continue
		}
		rest = append(rest, p)
	}
	if !spansOnly || len(rest) != 1 {
		return "", false
	}
	return rest[0], true
}

// Register wires spanview into the given shellcore at construction
// time — the View plus the `trace ... --spans` builtin.
func Register(c *shellcore.ShellCore, query QueryFunc) {
	c.RegisterView(NewView())
	c.RegisterBuiltin("trace", Builtin(query))
}
