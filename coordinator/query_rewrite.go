package coordinator

import (
	"fmt"
	"io"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/weft/planner"
)

// rewriteQueryPipelines walks a parsed /bin script body and replaces
// every pipeline whose leading command is a weft source command
// (cat/head/tail on /log/...) with a bash.CallbackCommand bound to
// the coordinator. The rewrite runs once before execution; warp then
// walks the tree and dispatches each callback exactly once as part
// of normal pipeline execution.
//
// This replaces an earlier runtime-hook design whose gate ran at
// every pipeline and, by rendering the pipeline to source for
// classification, caused command substitutions to evaluate twice.
// Moving the decision to a pre-execution AST pass removes the
// runtime decision entirely: after rewrite, the AST itself tells
// warp which pipelines belong to weft.
//
// The rewrite descends into CommandSub bodies, subshells, loops,
// conditionals, functions — anywhere warp.Walk reaches — so query
// pipelines anywhere in the tree are substituted regardless of
// nesting depth.
func rewriteQueryPipelines(script *bash.Script, c *Coordinator, tr TimeRange, customer string, alertCtx *AlertContext) {
	if script == nil {
		return
	}
	bash.Walk(script, func(n bash.Node) bool {
		pipe, ok := n.(*bash.Pipeline)
		if !ok || !isQueryShape(pipe) {
			return true
		}
		// Snapshot the original pipeline before we mutate it. The
		// callback's rendering uses the snapshot — the live AST node
		// has had its Commands replaced and no longer carries the
		// source-command stages.
		snap := *pipe
		pipe.Commands = []bash.Command{
			&bash.CallbackCommand{
				Name: "weft-query",
				Run: func(interp *bash.Interpreter, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
					src := bash.UnparsePipelineExpanded(&snap, interp.ExpandWord)
					result, err := c.query(src, tr, customer, alertCtx)
					if err != nil {
						fmt.Fprintf(stderr, "query: %v\n", err)
						return 1
					}
					if result.Text != "" {
						_, _ = io.WriteString(stdout, result.Text)
					}
					if result.Stderr != "" {
						_, _ = io.WriteString(stderr, result.Stderr)
					}
					return 0
				},
			},
		}
		// Return false so Walk does not descend into the pipeline's
		// replaced commands. The original source-command stages are
		// held alive only through the snapshot captured above.
		return false
	})
}

// isQueryShape recognises pipelines whose leading command is one of
// weft's pseudofile source commands. This is a cheap syntactic check
// — the planner remains the authority on whether a given source
// string is actually a valid query; the callback's coord.query call
// re-classifies and returns an error if the pipeline turns out not
// to be one. Kept in sync with planner/parse.go's parseSource: any
// source the planner recognises must appear here.
func isQueryShape(pipe *bash.Pipeline) bool {
	if pipe == nil || len(pipe.Commands) == 0 {
		return false
	}
	sc, ok := pipe.Commands[0].(*bash.SimpleCommand)
	if !ok || len(sc.Words) == 0 {
		return false
	}
	switch sc.Words[0].Literal() {
	case "cat", "head", "tail":
		return true
	}
	return false
}

// Compile-time witness that we depend on the planner package. The
// rewrite itself doesn't call into it (classification is purely
// lexical on the command name), but the callback's coord.query does —
// and if the planner API drifts we want to notice at rewrite-file
// scope.
var _ = planner.Plan
