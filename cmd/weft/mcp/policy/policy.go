// Package policy validates pipelines and scripts before they are
// executed by the MCP server. The MCP server lets an LLM run weft
// pipelines on the user's behalf, but the LLM is also reading log
// content that may be attacker-controlled — so a pipeline arriving
// from a tool call cannot be trusted to be benign.
//
// The validator parses input through warp's bash parser and walks
// the AST, emitting a Violation for every node that falls outside
// the supplied Policy. The same parsed *bash.Script is returned so
// the caller can hand it to the executor without re-parsing (which
// would otherwise leave a window where a re-lex disagrees with the
// validator).
package policy


// Policy describes what a validated pipeline is allowed to contain.
// A zero Policy rejects everything; use DefaultReadPolicy or
// DefaultScriptPolicy as a starting point.
type Policy struct {
	// AllowedCommands maps a command name (the literal first word of a
	// SimpleCommand) to a CommandRule. A command not present in the map
	// is rejected.
	AllowedCommands map[string]CommandRule

	// AllowFunctions controls whether FuncDef nodes are permitted.
	AllowFunctions bool

	// AllowControlFlow controls whether if/for/while/case nodes are
	// permitted.
	AllowControlFlow bool

	// AllowSubshells controls whether ( ... ) compound commands are
	// permitted.
	AllowSubshells bool

	// AllowGroups controls whether { ...; } compound commands are
	// permitted.
	AllowGroups bool

	// AllowCommandSub controls whether $(...) and `...` substitutions
	// are permitted. For untrusted-input policies this should always be
	// false — the inner pipeline is also validated, but allowing
	// substitution lets the model build path arguments from runtime
	// command output, which defeats path scoping.
	AllowCommandSub bool

	// AllowAssignments controls whether NAME=value prefixes on a
	// command (or bare assignment statements) are permitted.
	AllowAssignments bool

	// AllowRedirects controls how redirections are handled. The
	// untrusted-input default is RedirectsForbidden.
	AllowRedirects RedirectPolicy

	// AllowPipelineChain controls whether && / || are permitted
	// between pipelines in a List.
	AllowPipelineChain bool

	// MaxPipelineLen caps the number of commands in a single pipeline.
	// 0 means no cap.
	MaxPipelineLen int

	// MaxScriptCommands caps the number of top-level Lists in the
	// script. 0 means no cap.
	MaxScriptCommands int
}

// CommandRule describes how arguments to one allowed command are
// checked.
type CommandRule struct {
	// ValidateArg, if non-nil, is called for every positional
	// (non-flag) argument. Returning a non-nil error rejects the
	// argument with a path_forbidden violation.
	ValidateArg func(arg string) error

	// FlagsWithValue lists flag names whose following token is a
	// value, not a positional. Used to skip "--key VAL" pairs when
	// scanning positionals.
	FlagsWithValue map[string]bool
}

// RedirectPolicy controls how redirection operators are handled.
type RedirectPolicy int

const (
	// RedirectsForbidden rejects every redirection.
	RedirectsForbidden RedirectPolicy = iota
	// RedirectsStdinOnly permits < and <<<, rejects everything else.
	RedirectsStdinOnly
)

// Violation is one off-policy node found during validation.
type Violation struct {
	// Kind is a stable identifier (e.g. "command_forbidden",
	// "command_sub_forbidden", "path_forbidden"). Callers can switch
	// on Kind to render structured errors.
	Kind string

	// Detail is a human-readable description (e.g. the offending
	// command name, the redirect operator, the rejected path).
	Detail string
}

// Validate is implemented in validate.go.
