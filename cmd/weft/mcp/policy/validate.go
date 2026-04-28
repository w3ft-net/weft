package policy

import (
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
)

// Validate parses src through warp's bash parser and walks the AST,
// returning every off-policy node. See the package doc for the full
// contract.
func Validate(src string, p Policy) (*bash.Script, []Violation) {
	script, err := bash.Parse(src)
	if err != nil {
		return nil, []Violation{{Kind: "parse_error", Detail: err.Error()}}
	}

	var vs []Violation

	// Defensive recover: a malformed AST (warp's parser sometimes
	// accepts inputs that yield nil typed pointers — see e.g. a bare
	// trailing `|`) can panic inside bash.Walk before our callback
	// gets a chance to short-circuit. Treat any such panic as a parse
	// error rather than crashing the MCP server.
	defer func() {
		if r := recover(); r != nil {
			vs = append(vs, Violation{Kind: "parse_error", Detail: "malformed AST"})
		}
	}()

	bash.Walk(script, func(n bash.Node) bool {
		// A nil typed pointer satisfies an interface case but
		// dereferencing fields will panic. Treat any such node as a
		// parse error and stop descending.
		if isNilNode(n) {
			vs = append(vs, Violation{Kind: "parse_error", Detail: "malformed AST"})
			return false
		}
		switch x := n.(type) {

		case *bash.Script:
			if p.MaxScriptCommands > 0 && len(x.Commands) > p.MaxScriptCommands {
				vs = append(vs, Violation{
					Kind:   "too_many_commands",
					Detail: itoa(len(x.Commands)) + " > max " + itoa(p.MaxScriptCommands),
				})
			}

		case *bash.List:
			if !p.AllowPipelineChain && len(x.Rest) > 0 {
				op := x.Rest[0].Op
				vs = append(vs, Violation{
					Kind:   "chain_forbidden",
					Detail: op + " not allowed",
				})
			}

		case *bash.Pipeline:
			if p.MaxPipelineLen > 0 && len(x.Commands) > p.MaxPipelineLen {
				vs = append(vs, Violation{
					Kind:   "pipeline_too_long",
					Detail: itoa(len(x.Commands)) + " > max " + itoa(p.MaxPipelineLen),
				})
			}

		case *bash.SimpleCommand:
			checkSimple(x, p, &vs)

		case *bash.Subshell:
			if !p.AllowSubshells {
				vs = append(vs, Violation{Kind: "subshell_forbidden"})
			}

		case *bash.Group:
			if !p.AllowGroups {
				vs = append(vs, Violation{Kind: "group_forbidden"})
			}

		case *bash.IfClause, *bash.ForClause, *bash.WhileClause, *bash.CaseClause:
			if !p.AllowControlFlow {
				vs = append(vs, Violation{Kind: "control_flow_forbidden"})
			}

		case *bash.FuncDef:
			if !p.AllowFunctions {
				vs = append(vs, Violation{Kind: "funcdef_forbidden"})
			}

		case *bash.CommandSub:
			if !p.AllowCommandSub {
				vs = append(vs, Violation{
					Kind:   "command_sub_forbidden",
					Detail: "$(...) / backticks not allowed",
				})
			}
			// Walk descends into the body regardless — surfacing every
			// off-policy verb inside a substitution is more useful for
			// the model than a single opaque "command_sub_forbidden".
		}
		return true
	})
	return script, vs
}

// checkSimple inspects a single SimpleCommand against the policy:
// command name allowlist, redirects, assignments, and (when the
// matched rule supplies one) per-argument path validation.
func checkSimple(c *bash.SimpleCommand, p Policy, vs *[]Violation) {
	if len(c.Assigns) > 0 && !p.AllowAssignments {
		*vs = append(*vs, Violation{
			Kind:   "assignment_forbidden",
			Detail: c.Assigns[0].Name + "=…",
		})
	}
	for _, r := range c.Redirects {
		if !redirectAllowed(r.Op, p.AllowRedirects) {
			*vs = append(*vs, Violation{
				Kind:   "redirect_forbidden",
				Detail: r.Op + " " + r.Target.Literal(),
			})
		}
	}
	if len(c.Words) == 0 {
		return
	}
	name := c.Words[0].Literal()
	rule, ok := p.AllowedCommands[name]
	if !ok {
		*vs = append(*vs, Violation{Kind: "command_forbidden", Detail: name})
		return
	}
	if rule.ValidateArg == nil {
		return
	}
	for i := 1; i < len(c.Words); i++ {
		w := wordPath(c.Words[i])
		if strings.HasPrefix(w, "-") && w != "-" {
			// Skip the argument that follows a flag with a value.
			if rule.FlagsWithValue[w] && i+1 < len(c.Words) {
				i++
			}
			continue
		}
		if err := rule.ValidateArg(w); err != nil {
			*vs = append(*vs, Violation{
				Kind:   "path_forbidden",
				Detail: name + " " + w + ": " + err.Error(),
			})
		}
	}
}

// redirectAllowed reports whether redirect op is permitted by the
// policy's RedirectPolicy. Stdin-only mode permits the read-side
// operators only; everything else (including >, >>, &>, >&, <<, <<-)
// is rejected when not RedirectsStdinOnly.
func redirectAllowed(op string, mode RedirectPolicy) bool {
	if mode == RedirectsStdinOnly {
		switch op {
		case "<", "<<<":
			return true
		}
	}
	return false
}

// wordPath renders a Word for the purposes of path validation. It
// is similar to bash.Word.Literal() but preserves SpecialVar and
// ParamExpansion segments inside DoubleQuotedPart (which Literal()
// silently drops). That matters for script policies, where the
// path validator needs to recognise "$1" / "$@" as positionals
// regardless of whether the user wrote them quoted or not.
//
// CommandSubs inside a word are intentionally elided: those nodes
// are flagged separately by the AST walk, and including their text
// here could mislead the path validator into accepting an opaque
// expression that the cmd-sub check has already rejected.
func wordPath(w bash.Word) string {
	var sb strings.Builder
	wordPathInto(&sb, w.Parts)
	return sb.String()
}

func wordPathInto(sb *strings.Builder, parts []bash.WordPart) {
	for _, p := range parts {
		switch v := p.(type) {
		case *bash.LiteralPart:
			sb.WriteString(v.Value)
		case *bash.SingleQuotedPart:
			sb.WriteString(v.Value)
		case *bash.DoubleQuotedPart:
			wordPathInto(sb, v.Parts)
		case *bash.SpecialVar:
			sb.WriteByte('$')
			sb.WriteString(v.Name)
		case *bash.ParamExpansion:
			sb.WriteByte('$')
			sb.WriteString(v.Name)
		}
	}
}

// isNilNode reports whether n is a nil typed pointer. The bash.Walk
// type-switch case matches the type even when the pointer is nil,
// and dereferencing will then panic — so we screen nodes here.
func isNilNode(n bash.Node) bool {
	switch v := n.(type) {
	case *bash.Script:
		return v == nil
	case *bash.List:
		return v == nil
	case *bash.Pipeline:
		return v == nil
	case *bash.SimpleCommand:
		return v == nil
	case *bash.Subshell:
		return v == nil
	case *bash.Group:
		return v == nil
	case *bash.IfClause:
		return v == nil
	case *bash.ForClause:
		return v == nil
	case *bash.WhileClause:
		return v == nil
	case *bash.CaseClause:
		return v == nil
	case *bash.FuncDef:
		return v == nil
	case *bash.CommandSub:
		return v == nil
	}
	return false
}

// itoa is a small allocation-free integer formatter for Violation
// detail strings.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
