package coordinator

import (
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/mounts"
	"io"
	"strings"

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shellfs"
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
//
// fail, when non-nil, is called with a query's error when its answer is
// missing rows (missingRows); the run then has no result. Nil (tests)
// reports those like any other query error.
func rewriteQueryPipelines(ctx context.Context, script *bash.Script, c *Coordinator, tr TimeRange, owner shape.Added, userID string, alertCtx *AlertContext, fail func(error)) {
	if script == nil {
		return
	}
	// /bin is resolved through the group's shell filesystem. Absent for
	// tests that build a Coordinator without a store.
	var binFS bash.FS
	if !NoGroup(owner) && c != nil && c.Mounts != nil {
		binFS = shellfs.New(c.Mounts.Config(mounts.Session{Owner: owner}), shellfs.WithAccess(c.mountAccess()))
	}
	bash.Walk(script, func(n bash.Node) bool {
		pipe, ok := n.(*bash.Pipeline)
		if !ok || !isQueryShape(pipe) {
			return true
		}
		// The query ends at the first stage that names a /bin script:
		// compute's stateless worker has no /bin, so shipping the script
		// word there fails with "command not found". Stages from the
		// script on stay as ordinary commands after the callback, and warp
		// resolves them against /bin exactly as it does for
		// `echo x | script`.
		k := firstBinStage(ctx, pipe, binFS)
		rest := append([]bash.Command(nil), pipe.Commands[k:]...)
		// Snapshot the original pipeline before we mutate it. The
		// callback's rendering uses the snapshot — the live AST node
		// has had its Commands replaced and no longer carries the
		// source-command stages.
		snap := *pipe
		snap.Commands = append([]bash.Command(nil), pipe.Commands[:k]...)
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
		case n == 1 && len(rest) == 0:
			// Single-stage pipeline: every redirect on the lone
			// SimpleCommand becomes a CallbackCommand redirect.
			if sc, ok := snap.Commands[0].(*bash.SimpleCommand); ok {
				liftedRedirects = append(liftedRedirects, sc.Redirects...)
				sc.Redirects = nil
			}
		case n >= 1:
			// Multi-stage: stdin redirects belong to the FIRST
			// command, stdout/stderr/append redirects to the LAST
			// (bash's pipeline-output convention). Mid-pipeline
			// redirects on intermediate stages stay where they are
			// — those are a less common shape we don't lift. When
			// stages remain after the query (a /bin script split off
			// above), the last of THEM owns the output redirects and
			// the query's last stage lifts none.
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
			if last, ok := snap.Commands[n-1].(*bash.SimpleCommand); ok && len(rest) == 0 {
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
		pipe.Commands = append([]bash.Command{
			&bash.CallbackCommand{
				Name:      "weft-query",
				Redirects: liftedRedirects,
				Run: func(interp *bash.Interpreter, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
					src := bash.UnparsePipelineExpanded(&snap, interp.ExpandWord)
					// The redirects were lifted off the pipeline above, so the
					// planner never sees them: refuse `tail -f ... > file` here.
					if r, ok := stdoutRedirect(liftedRedirects); ok && planner.SourceFollows(src, interp.DirStack().Cwd()) {
						fmt.Fprintf(stderr, "query: %v\n", planner.FollowRedirectError(interp.ExpandWord(r.Target)))
						return 1
					}
					// runQuery is only callable from this callback in
					// the unified-dispatch model — Execute always hands
					// the pipeline to warp+rewriter, and the rewriter
					// re-enters runQuery for each substituted log
					// pipeline.
					// Resolve relative source paths (`tail gamestate.log`)
					// against the live cwd — read here, not at rewrite time,
					// so an in-line `cd /log/x && tail y.log` sees the post-cd
					// directory. The dirstack was seeded from the courier.
					result, err := c.runQuery(ctx, src, interp.DirStack().Cwd(), tr, owner, userID, alertCtx)
					if err != nil {
						return queryFailed(stderr, err, fail)
					}
					// Draining an already-produced stream: the values (trace
					// ID) of the propagated ctx above are kept, but its
					// cancellation is deliberately detached (renamed to avoid
					// the two meanings colliding).
					drainCtx := context.WithoutCancel(ctx)
					for {
						line, ok, err := result.Stream.Next(drainCtx)
						if err != nil {
							return queryFailed(stderr, err, fail)
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
		}, rest...)
		// Return false so Walk does not descend into the pipeline's
		// replaced commands. The original source-command stages are
		// held alive only through the snapshot captured above.
		return false
	})
}

// queryFailed reports a log query's error and returns its exit status. A
// query whose answer is missing rows fails the whole run through fail: a
// script carrying on with a short answer (counting it, alerting on it,
// writing it to a file) would produce a wrong result that looks right.
func queryFailed(stderr io.Writer, err error, fail func(error)) int {
	if fail != nil && queryUnanswered(err) {
		fail(fmt.Errorf("query: %w", err))
		return 1
	}
	fmt.Fprintf(stderr, "query: %v\n", err)
	return 1
}

// queryUnanswered reports whether err means the query has no answer to give:
// some shard holding rows for it couldn't be routed to or failed, or the org's
// cold scan budget is spent and the query needs cold history. Either way a
// partial answer would look right and be wrong, so a run fails on it.
func queryUnanswered(err error) bool {
	var down *logdb.UnavailableShardsError
	var failed *compute.MissingRowsError
	return errors.As(err, &down) || errors.As(err, &failed) || errors.Is(err, identity.ErrColdScanBudgetExhausted)
}

// firstBinStage returns the index of the first stage after the source
// whose command names a /bin script, or len(pipe.Commands) when there is
// none (or fs is nil). Stage 0 is the source command and never splits.
func firstBinStage(ctx context.Context, pipe *bash.Pipeline, fs bash.FS) int {
	n := len(pipe.Commands)
	if fs == nil {
		return n
	}
	for i := 1; i < n; i++ {
		sc, ok := pipe.Commands[i].(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			continue
		}
		name := sc.Words[0].Literal()
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if _, err := fs.ReadFile(ctx, "/bin/"+name); err == nil {
			return i
		}
	}
	return n
}

// stdoutRedirect returns the first redirect that sends stdout to a
// file (`>`, `>>`, `&>`); `2>` and fd duplication don't count.
func stdoutRedirect(rs []bash.Redirect) (bash.Redirect, bool) {
	for _, r := range rs {
		if (r.Op == ">" || r.Op == ">>" || r.Op == "&>") && r.Fd != 2 {
			return r, true
		}
	}
	return bash.Redirect{}, false
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
	case "trace":
		// trace's argument is an opaque correlation ID, not a filesystem
		// path — skip the IsLocalFSPath check entirely.
		return true
	}
	// A leading command other than cat/head/tail that's still handed an
	// absolute /log/ path directly (real-Unix habit: `grep pattern
	// file` instead of piping through a source stage first) can't
	// itself be a source — grep/awk/jq/etc. are pushdown/compute
	// stages. Route it into the query callback anyway so planner.Plan's
	// parseSource rejects it with a clear "must start with cat/head/
	// tail" error, rather than leaving it untouched here and falling
	// through to plain warp execution, where it reads the literal path
	// off shellfs's LogMount and silently returns zero rows with no
	// error at all.
	//
	// Filesystem-navigation commands (cd, ls, tree, ...) are exempt: a
	// /log/ path there is a directory to browse, which warp+shellfs
	// serve; wrapping it in a query callback made `ls /log/acme` and
	// `cd /log/acme/prod/api` fail.
	if planner.IsFilesystemCommand(sc.Words[0].Literal()) {
		return false
	}
	for _, w := range sc.Words[1:] {
		if strings.HasPrefix(w.Literal(), "/log/") {
			return true
		}
	}
	return false
}

// Compile-time witness that we depend on the planner package. The
// rewrite itself doesn't call into it (classification is purely
// lexical on the command name), but the callback's coord.query does —
// and if the planner API drifts we want to notice at rewrite-file
// scope.
var _ = planner.Plan
