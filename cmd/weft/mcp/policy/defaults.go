package policy

import (
	"fmt"
	"strings"
)

// DefaultReadPolicy is the policy applied to pipelines that the MCP
// server's `weft.run` tool runs on behalf of an LLM. It is the most
// restrictive of the built-in policies:
//
//   - only pure log-analysis verbs are allowed;
//   - every positional path argument must live under /log/;
//   - no redirects, command substitution, control flow, subshells,
//     groups, function definitions, assignments, or && / || chains;
//   - pipelines are capped at 16 commands.
//
// This is the policy to use whenever the input string came from a
// language model that is also reading log content.
func DefaultReadPolicy() Policy {
	return Policy{
		AllowedCommands: map[string]CommandRule{
			"cat":   {ValidateArg: underLog},
			"head":  {ValidateArg: underLog, FlagsWithValue: map[string]bool{"-n": true, "-c": true}},
			"tail":  {ValidateArg: underLog, FlagsWithValue: map[string]bool{"-n": true, "-c": true}},
			"ls":    {ValidateArg: underLog},
			"grep":  {FlagsWithValue: map[string]bool{"-A": true, "-B": true, "-C": true, "-e": true, "-f": true, "-m": true}},
			"awk":   {FlagsWithValue: map[string]bool{"-F": true, "-v": true, "-f": true}},
			"sed":   {FlagsWithValue: map[string]bool{"-e": true, "-f": true}},
			"jq":    {FlagsWithValue: map[string]bool{"--arg": true, "--argjson": true, "--slurpfile": true, "-f": true}},
			"sort":  {FlagsWithValue: map[string]bool{"-k": true, "-t": true}},
			"uniq":  {FlagsWithValue: map[string]bool{"-f": true, "-s": true}},
			"wc":    {},
			"cut":   {FlagsWithValue: map[string]bool{"-d": true, "-f": true, "-c": true}},
			"tr":    {},
			"chart": {FlagsWithValue: map[string]bool{"-t": true, "-n": true}},

			// Cron-result aggregators. These read history from a named
			// watch — args are (watchName, duration) or
			// (watchName, duration, p) — and never touch the /log/
			// filesystem, so no path validator is needed. Useful both
			// inside cron alert pipelines (e.g.
			// `weft.latest coord-heartbeat-age | alert above 90`) and
			// as one-shot reads via weft.run.
			"weft.latest":     {},
			"weft.avg":        {},
			"weft.max":        {},
			"weft.min":        {},
			"weft.sum":        {},
			"weft.stddev":     {},
			"weft.first":      {},
			"weft.count":      {},
			"weft.percentile": {},
			"weft.delta":      {},
			"weft.slope":      {},
			"weft.rate":       {},
		},
		AllowFunctions:     false,
		AllowControlFlow:   false,
		AllowSubshells:     false,
		AllowGroups:        false,
		AllowCommandSub:    false,
		AllowAssignments:   false,
		AllowRedirects:     RedirectsForbidden,
		AllowPipelineChain: false,
		MaxPipelineLen:     16,
	}
}

// DefaultCronPolicy is the policy applied to the pipeline body of
// a cron proposal. It is DefaultReadPolicy plus the `alert` pipe
// stage, which crons use to register an alert condition.
//
// Crons that do *not* alert (pure scheduled jobs) pass this policy
// trivially — they look the same as a one-shot read pipeline.
// Crons that do alert end with `| alert <condition> [-k …] [-t …]
// [--notify …]`, all of which are vetted as flag values rather than
// positional paths so the alert key (which can be a JSON path like
// `.latency_ms`) doesn't trip the path validator.
func DefaultCronPolicy() Policy {
	p := DefaultReadPolicy()
	p.AllowedCommands["alert"] = CommandRule{
		FlagsWithValue: map[string]bool{
			"-k": true, "-t": true,
			"--tag":    true,
			"--notify": true,
		},
	}
	return p
}

// underLog is the path validator used by the read policy: every
// positional path argument must be rooted at /log/ and must not
// contain '..' segments. Bare names (no slash) are also accepted —
// they are typically pseudofile shortcuts the shell expands itself
// (e.g. `cat billing.log` resolves under /log via shellfs).
//
// Positional parameters ($1, $@, etc.) are rejected here because a
// one-shot pipeline from the LLM has no positionals to expand —
// they are only meaningful inside a saved bin/ script, which uses
// underLogOrPositional instead.
func underLog(arg string) error {
	if strings.Contains(arg, "..") {
		return fmt.Errorf("'..' not allowed in path")
	}
	if strings.HasPrefix(arg, "$") {
		return fmt.Errorf("positional parameters not allowed in one-shot pipelines")
	}
	if !strings.HasPrefix(arg, "/") {
		// Relative names (no leading /) are resolved by the shell's
		// pseudofile layer; we don't second-guess them here.
		return nil
	}
	if !strings.HasPrefix(arg, "/log/") && arg != "/log" {
		return fmt.Errorf("path must be under /log/")
	}
	return nil
}

// underLogOrPositional is the path validator used by the script
// policy: same as underLog, but positional parameters and parameter
// expansions are accepted because saved scripts in bin/ are invoked
// with arguments. The runtime is still responsible for refusing to
// open paths outside /log/ once the positional has been expanded.
func underLogOrPositional(arg string) error {
	if strings.Contains(arg, "..") {
		return fmt.Errorf("'..' not allowed in path")
	}
	if strings.HasPrefix(arg, "$") {
		return nil
	}
	if !strings.HasPrefix(arg, "/") {
		return nil
	}
	if !strings.HasPrefix(arg, "/log/") && arg != "/log" {
		return fmt.Errorf("path must be under /log/")
	}
	return nil
}

// DefaultScriptPolicy is the policy applied to the body of a
// bin/<name> script proposed by the LLM. It permits:
//
//   - the same verb allowlist as the read policy, plus echo;
//   - if/for/while/case control flow;
//   - $1/$2/$@ positionals as path arguments.
//
// It still forbids subshells, groups, function definitions,
// command substitution, redirects, and && / || chains. Saved scripts
// are pipelines that take positional inputs — anything more elaborate
// belongs in real Go code, not a model-authored script body.
func DefaultScriptPolicy() Policy {
	p := DefaultReadPolicy()
	p.AllowControlFlow = true
	// Override path validators to also accept positionals.
	for name, rule := range p.AllowedCommands {
		if rule.ValidateArg != nil {
			rule.ValidateArg = underLogOrPositional
			p.AllowedCommands[name] = rule
		}
	}
	// echo is the canonical "say something to stdout" verb in scripts.
	p.AllowedCommands["echo"] = CommandRule{}
	// Scripts are typically short — a few statements at most.
	p.MaxScriptCommands = 32
	return p
}
