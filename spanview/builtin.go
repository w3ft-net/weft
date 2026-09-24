package spanview

import (
	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// Builtin returns a shellcore.BuiltinFunc that intercepts three typed
// shapes before they'd otherwise dispatch as a normal query pipeline,
// entering the SpanView instead:
//
//   - `trace <id> --spans`     — Phase 1, per-trace rollup
//   - `trace spans`            — Phase 2, every span name aggregated
//     over the current time range
//   - `trace spans <name>`     — Phase 2, filtered to one span name
//
// Any other line returns false so it falls through to the shell's
// normal pipeline dispatch unchanged: plain `trace <id>` (no --spans
// among parts), and a piped `trace <id> --spans | grep ...` too —
// shellcore hands the whole line's fields here including a literal "|"
// token, so that case fails the arity check below and ships to the
// server like any other pipeline, --spans stripped there by
// planner/parse.go same as a one-shot query.
//
// Wire it via core.RegisterBuiltin("trace", spanview.Builtin(query)).
func Builtin(query QueryFunc) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, parts []string) bool {
		listArgs, ok := parseSpansLine(parts)
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

// parseSpansLine reports whether parts (a builtin dispatch's
// whitespace-split command line) matches one of Builtin's three
// trigger shapes, returning the pipeline to run and the status-bar
// title. Any other shape — plain `trace <id>`, wrong arity, or a piped
// line (shellcore hands the whole line's fields here, so a literal "|"
// token lands in the non-flag remainder and fails the arity checks
// below) — returns ok=false.
func parseSpansLine(parts []string) (*ListArgs, bool) {
	if len(parts) == 0 || parts[0] != "trace" {
		return nil, false
	}
	rest := parts[1:]

	// `trace spans` / `trace spans <name>` — Phase 2. Checked first:
	// "spans" here is the sub-verb, not a trace id, and never carries
	// --spans alongside it.
	if len(rest) >= 1 && rest[0] == "spans" {
		switch len(rest) {
		case 1:
			return &ListArgs{Pipeline: "trace spans", Title: "all spans"}, true
		case 2:
			name := rest[1]
			return &ListArgs{Pipeline: "trace spans " + name, Title: name}, true
		default:
			return nil, false
		}
	}

	// `trace <id> --spans` — Phase 1. The flag can appear in either
	// position relative to the id.
	var spansOnly bool
	var idParts []string
	for _, p := range rest {
		if p == "--spans" {
			spansOnly = true
			continue
		}
		idParts = append(idParts, p)
	}
	if !spansOnly || len(idParts) != 1 {
		return nil, false
	}
	traceID := idParts[0]
	return &ListArgs{Pipeline: "trace " + traceID, Title: traceID}, true
}

// Register wires spanview into the given shellcore at construction
// time — the View plus the `trace` builtin covering both span-display
// phases.
func Register(c *shellcore.ShellCore, query QueryFunc) {
	c.RegisterView(NewView())
	c.RegisterBuiltin("trace", Builtin(query))
}
