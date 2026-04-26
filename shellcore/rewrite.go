package shellcore

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
)

// RewritePaths takes a pipeline string, applies cd / pushd / popd
// against ds in source order, and rewrites bare-name path arguments to
// absolute pseudofile paths so the planner sees fully-resolved
// references regardless of where in the line the cd appeared.
//
// Returns the rewritten pipeline. If parsing fails, returns the
// original line and the parse error — callers may fall back to passing
// the line through unmodified, since malformed pipelines will fail
// downstream with the planner's own error message.
//
// ds is a bash.DirStack the caller owns. RewritePaths mutates it as it
// walks the AST: dirstack builtins encountered in single-command
// pipelines apply to ds. For an interactive shell using shellcore's
// dirStack, this means cd in a `weft sh -c` chain leaves the same
// post-state as if it had been typed at the prompt.
//
// The visitor only rewrites arguments that, after lexical resolution
// against ds.Cwd(), classify as PathQuery (under /log/). Other args —
// flags, /etc paths, /bin paths, literal data — pass through.
func RewritePaths(line string, ds bash.DirStack) (string, error) {
	ast, err := bash.Parse(line)
	if err != nil {
		return line, fmt.Errorf("parse: %w", err)
	}
	bash.RewriteAST(ast, ds, "/", rewriteSourceArgs)
	return bash.Unparse(ast), nil
}

// rewriteSourceArgs is the visitor used by RewritePaths. For each
// SimpleCommand it visits, it walks Words[1:] and replaces arguments
// that resolve (lexically, against cwd) to a PathQuery with the
// absolute form. Flag-shaped args (leading -) and arguments that don't
// classify as PathQuery are left untouched.
//
// The dirstack builtins themselves (cd / pushd / popd / pwd / dirs)
// are skipped — their path arg is handled by RewriteAST when the
// builtin applies to the transient DirStack, and rewriting cd's arg
// would interfere with the lexical normalisation that DirStack
// performs.
func rewriteSourceArgs(cmd *bash.SimpleCommand, cwd string) {
	if len(cmd.Words) == 0 {
		return
	}
	name := cmd.Words[0].Literal()
	switch name {
	case "cd", "pushd", "popd", "pwd", "dirs":
		return
	}
	for i := 1; i < len(cmd.Words); i++ {
		raw := cmd.Words[i].Literal()
		if raw == "" || strings.HasPrefix(raw, "-") {
			continue
		}
		resolved := joinAgainst(cwd, raw)
		if ClassifyPath(resolved) != PathQuery {
			continue
		}
		cmd.Words[i] = bash.Word{Parts: []bash.WordPart{&bash.LiteralPart{Value: resolved}}}
	}
}

// joinAgainst is the lexical "join path against cwd" used by the
// rewriter. Mirrors ShellCore.resolvePath but takes cwd as a parameter
// so it can be called without an enclosing ShellCore (e.g. in
// non-interactive mode).
func joinAgainst(cwd, path string) string {
	if !strings.HasPrefix(path, "/") {
		if cwd == "/" {
			path = "/" + path
		} else {
			path = cwd + "/" + path
		}
	}
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "/"
	}
	return "/" + strings.Join(out, "/")
}
