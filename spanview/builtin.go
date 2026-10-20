package spanview

import (
	"strings"

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
// whitespace-split command line) matches one of Builtin's three
// trigger shapes, returning the pipeline to run and the status-bar
// title. Any other shape — plain `trace <id>`, wrong arity, or a piped
// line (shellcore hands the whole line's fields here, so a literal "|"
// token lands in the non-flag remainder and fails the arity checks
// below) — returns ok=false.
//
// Exported (not just for Builtin's own use) so shell's traceRequestBuiltin
// (shell/shell.go, the browser's `Trace` hook — see
// docs/plans/browser-span-view.md) can reuse this exact arg grammar for
// the rollup/aggregate shapes rather than forking it. shell imports this
// package directly since it pulls in nothing beyond shellcore/theme,
// both already part of shell's WASM-safe import graph.
func ParseSpansLine(parts []string) (*ListArgs, bool) {
	if len(parts) == 0 || parts[0] != "trace" {
		return nil, false
	}
	rest := parts[1:]

	if len(rest) >= 1 && rest[0] == "spans" {
		var name string
		byService := false
		for i := 1; i < len(rest); i++ {
			switch {
			case rest[i] == "--by" && i+1 < len(rest) && rest[i+1] == "service":
				byService = true
				i++
			case strings.HasPrefix(rest[i], "-") || name != "":
				return nil, false
			default:
				name = rest[i]
			}
		}
		pipeline, title := "trace spans", "all spans"
		if name != "" {
			pipeline, title = pipeline+" "+name, name
		}
		if byService {
			pipeline += " --by service"
			title += " by service"
		}
		return &ListArgs{Pipeline: pipeline, Title: title}, true
	}

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
	return &ListArgs{Pipeline: "trace " + traceID + " --spans", Title: traceID}, true
}

// Register wires spanview into the given shellcore at construction
// time — the View plus the `trace` builtin covering both span-display
// phases.
func Register(c *shellcore.ShellCore, query QueryFunc) {
	c.RegisterView(NewView())
	c.RegisterBuiltin("trace", Builtin(query))
}
