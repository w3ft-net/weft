package coordinator

import (
	"context"
	"fmt"
	"io"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/weft/planner"
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
func rewriteQueryPipelines(script *bash.Script, c *Coordinator, tr TimeRange, customer string, custID string, userID string, alertCtx *AlertContext) {
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
		// Lift redirects off the displaced commands onto the
		// substituted callback. Bash semantics put pipeline-output
		// redirects on the LAST command (`cat | grep > out` redirects
		// grep's stdout, which IS the pipeline output) and pipeline-
		// input redirects on the FIRST. Since the entire pipeline
		// becomes one CallbackCommand, both classes have to migrate
		// onto it or warp's interpreter loses the binding when the
		// original SimpleCommand nodes are dropped.
		// After lifting, ALSO strip the lifted redirects from the
		// snap's SimpleCommands. The snap is used to re-render the
		// pipeline as a source string for runQuery; leaving the
		// redirect in the rendered string would have the planner
		// see it twice (once via the lifted CallbackCommand redirect
		// that warp will honor, once as plan.RedirectTarget on the
		// re-parsed pipeline). Stripping keeps redirect handling
		// solely on the CallbackCommand.
		var liftedRedirects []bash.Redirect
		switch n := len(snap.Commands); {
		case n == 1:
			// Single-stage pipeline: every redirect on the lone
			// SimpleCommand becomes a CallbackCommand redirect.
			if sc, ok := snap.Commands[0].(*bash.SimpleCommand); ok {
				liftedRedirects = append(liftedRedirects, sc.Redirects...)
				sc.Redirects = nil
			}
		case n > 1:
			// Multi-stage: stdin redirects belong to the FIRST
			// command, stdout/stderr/append redirects to the LAST
			// (bash's pipeline-output convention). Mid-pipeline
			// redirects on intermediate stages stay where they are
			// — those are a less common shape we don't lift.
			if first, ok := snap.Commands[0].(*bash.SimpleCommand); ok {
				keep := first.Redirects[:0:0]
				for _, r := range first.Redirects {
					if isStdinRedirect(r.Op) {
						liftedRedirects = append(liftedRedirects, r)
					} else {
						keep = append(keep, r)
					}
				}
				first.Redirects = keep
			}
			if last, ok := snap.Commands[n-1].(*bash.SimpleCommand); ok {
				keep := last.Redirects[:0:0]
				for _, r := range last.Redirects {
					if !isStdinRedirect(r.Op) {
						liftedRedirects = append(liftedRedirects, r)
					} else {
						keep = append(keep, r)
					}
				}
				last.Redirects = keep
			}
		}
		pipe.Commands = []bash.Command{
			&bash.CallbackCommand{
				Name:      "weft-query",
				Redirects: liftedRedirects,
				Run: func(interp *bash.Interpreter, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
					src := bash.UnparsePipelineExpanded(&snap, interp.ExpandWord)
					// runQuery is only callable from this callback in
					// the unified-dispatch model — Execute always hands
					// the pipeline to warp+rewriter, and the rewriter
					// re-enters runQuery for each substituted log
					// pipeline.
					// Resolve relative source paths (`tail gamestate.log`)
					// against the live cwd — read here, not at rewrite time,
					// so an in-line `cd /log/x && tail y.log` sees the post-cd
					// directory. The dirstack was seeded from the courier.
					result, err := c.runQuery(src, interp.DirStack().Cwd(), tr, customer, custID, userID, alertCtx)
					if err != nil {
						fmt.Fprintf(stderr, "query: %v\n", err)
						return 1
					}
					ctx := context.Background()
					for {
						line, ok, err := result.Stream.Next(ctx)
						if err != nil {
							fmt.Fprintf(stderr, "query: %v\n", err)
							return 1
						}
						if !ok {
							break
						}
						w := stdout
						if line.Stderr {
							w = stderr
						}
						_, _ = io.WriteString(w, line.Raw)
						_, _ = io.WriteString(w, "\n")
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

// isStdinRedirect reports whether the redirect operator reads input
// (and thus, on a multi-stage pipeline, lives on the FIRST command).
// Stdout/stderr-flavored ops live on the LAST command in bash's
// pipeline-output convention. The split matters when lifting
// redirects off a displaced pipeline onto a single CallbackCommand —
// stdin ops come from the head of the pipeline, the rest from the
// tail.
func isStdinRedirect(op string) bool {
	switch op {
	case "<", "<<", "<<<":
		return true
	}
	return false
}

// isQueryShape recognises pipelines whose leading command is one of
// weft's pseudofile source commands. This is a cheap syntactic check
// — the planner remains the authority on whether a given source
// string is actually a valid query; the callback's coord.query call
// re-classifies and returns an error if the pipeline turns out not
// to be one. Kept in sync with planner/parse.go's parseSource: any
// source the planner recognises must appear here.
//
// Reads against non-log shellfs namespaces (/bin, /cron.d, /etc,
// /tmp) are explicitly excluded — Plan routes those to Local mode
// so warp+shellfs serve them, and wrapping them in a query callback
// here would just funnel them straight back into runLocal and
// infinite-loop.
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
		for _, w := range sc.Words[1:] {
			if planner.IsLocalFSPath(w.Literal()) {
				return false
			}
		}
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
