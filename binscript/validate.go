package binscript

import (
	"fmt"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	luaparse "github.com/yuin/gopher-lua/parse"
)

// Validate rejects syntactically-invalid /bin scripts at save time.
// Catching parse errors here surfaces them to the user (in vi's status line
// on :w) instead of letting an unparseable script silently persist and
// then 500 at invocation time.
//
// Bash scripts additionally must satisfy a set of shape rules that would
// otherwise produce confusing runtime behaviour:
//   - ValidateAlertPosition — alerts only at the terminal position.
//   - validateNoFollowMode — `tail -f` is incompatible with the
//     body-runs-to-completion execution model for saved scripts.
//   - validateNoRangeBuiltin — `range` adjusts the request-scoped
//     time window, which a saved script cannot scope properly (its
//     effects would leak to subsequent interior queries in
//     unexpected ways).
func Validate(lang, source string) error {
	switch lang {
	case "bash":
		script, err := bash.Parse(source)
		if err != nil {
			return fmt.Errorf("bash syntax error: %w", err)
		}
		if err := ValidateAlertPosition(script); err != nil {
			return err
		}
		if err := validateNoFollowMode(script); err != nil {
			return err
		}
		if err := validateNoRangeBuiltin(script); err != nil {
			return err
		}
		if err := ValidateWeftFunctionsAreNotPiped(script); err != nil {
			return err
		}
	case "lua":
		if _, err := luaparse.Parse(strings.NewReader(source), "source"); err != nil {
			return fmt.Errorf("lua syntax error: %w", err)
		}
	}
	return nil
}

// ValidateAlertPosition enforces that any `alert` stage in a /bin
// script body appears only as the terminal stage of the body's final
// top-level list, and that list is a bare pipeline (not inside
// if/for/while/case/subshell/group/function). Any other placement is
// rejected with a user-facing error that names the offending location.
//
// Rationale: a body that invokes `alert` mid-flow or inside a loop
// fires the alert N times per script invocation, typically N > the
// user's intent. Constraining alert to "the last thing the body
// does" matches how interactive pipelines use alerts today and
// matches the outer-watch model (one alert dispatch per watch run).
func ValidateAlertPosition(script *bash.Script) error {
	if script == nil || len(script.Commands) == 0 {
		return nil
	}
	// Find every `alert` SimpleCommand anywhere in the tree, along
	// with whether it sits at the allowed position.
	lastIdx := len(script.Commands) - 1
	allowed := make(map[*bash.SimpleCommand]bool)
	if sc := terminalAlertOf(script.Commands[lastIdx]); sc != nil {
		allowed[sc] = true
	}

	var offender *bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if offender != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			return true
		}
		if sc.Words[0].Literal() != "alert" {
			return true
		}
		if !allowed[sc] {
			offender = sc
		}
		return true
	})
	if offender != nil {
		return fmt.Errorf("alert may only appear as the final stage of a script or cron body, not in a leading list, loop body, conditional, or nested command: it fires once per run, wherever it sits (to page several places, point --notify at one notify.yaml alias with several targets)")
	}
	return nil
}

// stdinWeftCommands are the weft:: commands that read standard input; every
// other weft:: function takes its cron name and window as arguments and
// ignores stdin.
var stdinWeftCommands = map[string]bool{"weft::is_gt": true, "weft::is_lt": true}

// ValidateWeftFunctionsAreNotPiped rejects a weft:: function that is not the
// first stage of its pipeline. Those functions read stored cron history, never
// their input, so whatever is piped in is silently thrown away, which is never
// what the author meant. The stdin comparators are the way to test a piped
// value.
func ValidateWeftFunctionsAreNotPiped(script *bash.Script) error {
	if script == nil {
		return nil
	}
	var offender string
	var stage int
	bash.Walk(script, func(n bash.Node) bool {
		if offender != "" {
			return false
		}
		p, ok := n.(*bash.Pipeline)
		if !ok {
			return true
		}
		for i, c := range p.Commands {
			sc, ok := c.(*bash.SimpleCommand)
			if !ok || i == 0 || len(sc.Words) == 0 {
				continue
			}
			name := sc.Words[0].Literal()
			if strings.HasPrefix(name, "weft::") && !stdinWeftCommands[name] {
				offender, stage = name, i+1
				return false
			}
		}
		return true
	})
	if offender != "" {
		return fmt.Errorf("%s is stage %d of a pipeline, but it never reads its input, so what is piped into it is discarded; to test a piped value, use weft::is_gt or weft::is_lt", offender, stage)
	}
	return nil
}

// validateNoFollowMode rejects scripts that use `tail -f` anywhere in
// the body. Follow mode streams indefinitely — it has no natural
// completion point — which conflicts with the body-runs-to-completion
// execution model for saved scripts (where a caller, cron, or HTTP
// request is waiting for a final stdout). Allowing it would mean
// either the script hangs forever or the first ERROR row kills the
// whole stream; neither is what the user expects.
//
// If the user genuinely wants to follow logs, `tail -f` works at the
// interactive shell prompt where there is no completion requirement.
func validateNoFollowMode(script *bash.Script) error {
	if script == nil {
		return nil
	}
	var found *bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if found != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			return true
		}
		if sc.Words[0].Literal() != "tail" {
			return true
		}
		for _, w := range sc.Words[1:] {
			lit := w.Literal()
			if lit == "-f" || lit == "--follow" || strings.HasPrefix(lit, "-f") {
				found = sc
				return false
			}
		}
		return true
	})
	if found != nil {
		return fmt.Errorf("tail -f is not allowed in saved scripts (follow mode streams indefinitely and has no completion point); use `tail -f` interactively at the shell prompt instead")
	}
	return nil
}

// validateNoRangeBuiltin rejects scripts that invoke the `range`
// shell builtin. `range` adjusts the request's time window in-place;
// inside a saved script its effect would silently leak into every
// interior query the script fires afterwards, producing windows the
// caller did not ask for. Saved scripts inherit `tr` from the
// request that invoked them; users who need a different window
// should invoke the script from a shell session where they've
// already set the window, or use explicit time predicates in their
// awk/grep stages.
func validateNoRangeBuiltin(script *bash.Script) error {
	if script == nil {
		return nil
	}
	var found *bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if found != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			return true
		}
		if sc.Words[0].Literal() == "range" {
			found = sc
			return false
		}
		return true
	})
	if found != nil {
		return fmt.Errorf("range is not allowed in saved scripts (it modifies the request-scoped time window and would leak into interior queries); set the window at the call site or use explicit time predicates inside the script")
	}
	return nil
}

// terminalAlertOf returns the `alert` SimpleCommand that is the last
// stage of list's first (and only allowed) pipeline, iff list is a
// bare single-pipeline list (no ListCont continuations). Any other
// shape returns nil, which means no alert in that list is "allowed"
// — including alerts sitting at the end of a compound list like
// `foo && bar | alert`, where the alert's semantics are too
// short-circuit-dependent to reason about cleanly.
func terminalAlertOf(list *bash.List) *bash.SimpleCommand {
	if list == nil || list.First == nil {
		return nil
	}
	if len(list.Rest) != 0 {
		return nil
	}
	pipe := list.First
	if len(pipe.Commands) == 0 {
		return nil
	}
	last := pipe.Commands[len(pipe.Commands)-1]
	sc, ok := last.(*bash.SimpleCommand)
	if !ok || len(sc.Words) == 0 {
		return nil
	}
	if sc.Words[0].Literal() != "alert" {
		return nil
	}
	return sc
}
