# Plan: log-query redirects via warp

## Status

Done. Warp's `CallbackCommand` gained a `Redirects` field;
`coordinator/query_rewrite.go` lifts redirects from the displaced
pipeline onto the substituting `CallbackCommand` and strips them
from the snap before re-rendering, so warp's own redirect machinery
(via shellfs's writer) handles the FS write. `writeQueryResultToFS`
is deleted and `QueryPlan.RedirectTarget` is no longer consulted
by the dispatch path.

## Why

Today the coordinator special-cases `cat /log/foo.log > /tmp/notes`:

```go
// coordinator/coordinator.go (c.query)
plan, err := planner.Plan(pipeline)
// ... plan + execute ...
if plan.RedirectTarget != "" && custID != 0 {
    if err := c.writeQueryResultToFS(result, plan, custID, userID); err != nil {
        return nil, fmt.Errorf("redirect: %w", err)
    }
    result.Rows = nil
    result.Text = ""
}
```

This duplicates work warp+shellfs already does. For non-log shapes
(`echo hi > /tmp/notes`, `cat /tmp/foo > /tmp/bar`) the same `>`
just works — the bash AST carries the redirect, warp executes the
pipeline, and shellfs serves the redirect target. No coord-side
write code involved.

The reason the log-query path is special: `rewriteQueryPipelines`
in `coordinator/query_rewrite.go` replaces the entire matched
`bash.Pipeline.Commands` with a single `bash.CallbackCommand` that
invokes `c.query` for the embedded log query. `CallbackCommand`
has no `Redirects` field, so when the original pipeline's last
`SimpleCommand` carried a `> /tmp/...` redirect, the rewrite
silently drops it.

```
original AST:    Pipeline{ cat /log/foo.log, awk ..., grep ... > /tmp/errs }
                                                              ^^^^^^^^^^^^^
                                                          (redirect on last SC)

after rewrite:   Pipeline{ CallbackCommand("weft-query") }
                                                          ← redirect lost
```

Warp's redirect machinery (`pkg/bash/exec.go:481-540`) reads
`cmd.Redirects` on the command being executed. When the command is
a `CallbackCommand` with no Redirects field, no redirects fire.
Coord compensates by re-extracting the target inside the planner
(`extractTrailingFSRedirect`) and calling `writeQueryResultToFS`
out-of-band after `executeQuery` returns.

The cleanup: warp learns that callbacks can also have redirects;
the rewrite transfers the last command's redirects to the
callback; coord drops the workaround.

## What changes

### Warp side (`~/dev/warp/pkg/bash`)

1. Add `Redirects []*Redirect` to `CallbackCommand` in
   `pkg/bash/ast.go`.

   ```go
   type CallbackCommand struct {
       Name      string
       Run       func(interp *Interpreter, stdin io.Reader,
                      stdout io.Writer, stderr io.Writer) int
       Redirects []*Redirect  // NEW — same shape SimpleCommand uses
   }
   ```

2. Update `pkg/bash/exec.go` to honor `cmd.Redirects` on
   `CallbackCommand` the same way it does on `SimpleCommand`. The
   logic at `exec.go:481-540` (file redirects, append vs overwrite,
   redirect capture) is already factored — call it for callbacks
   too.

3. Tests in `pkg/bash/exec_test.go`: a callback with a `>`
   redirect writes its stdout to the redirect target's
   shellfs writer instead of the surrounding pipeline's stdout.
   Same for `>>`.

### Weft side (`~/dev/weft`)

1. `coordinator/query_rewrite.go` — when wrapping a matched
   pipeline, transfer the last command's Redirects to the
   CallbackCommand:

   ```go
   bash.Walk(script, func(n bash.Node) bool {
       pipe, ok := n.(*bash.Pipeline)
       if !ok || !isQueryShape(pipe) { return true }
       snap := *pipe

       // NEW: capture trailing redirects from the last SimpleCommand
       // so warp's exec machinery applies them to the callback's
       // stdout. Without this, `cat /log/foo > /tmp/notes` silently
       // drops the redirect (the coord-side workaround in c.query
       // is what we're deleting in this commit).
       var redirs []*bash.Redirect
       if last, ok := pipe.Commands[len(pipe.Commands)-1].(*bash.SimpleCommand); ok {
           redirs = last.Redirects
       }

       pipe.Commands = []bash.Command{
           &bash.CallbackCommand{
               Name:      "weft-query",
               Run:       /* unchanged */,
               Redirects: redirs,
           },
       }
       return false
   })
   ```

2. `planner/planner.go` — delete:
   - `QueryPlan.RedirectTarget`
   - `QueryPlan.RedirectAppend`
   - `extractTrailingFSRedirect`
   - The `redirTarget, redirAppend` extraction in `Plan`

3. `coordinator/coordinator.go` — delete:
   - `c.writeQueryResultToFS`
   - The `if plan.RedirectTarget != ""` branch in `c.query`
   - Imports of `shellfs.Option` etc. that only that branch used

4. Update `coordinator/cat_redirect_tmp_test.go`: the existing
   `TestCoordinatorQuery_LogQueryRedirectToTmp` tests the right
   behavior (the file is written) but goes through the deleted
   path. With warp handling redirects, the test should pass
   unchanged once the rewrite carries the redirect through. Verify.

### Planner test updates

Several planner tests assert the post-extraction shape (`extractTrailingFSRedirect` returns `(queryPart, target, append, nil)`).
These get deleted along with the function.

## Why this routes through warp cleanly

The `cat /log/foo.log > /tmp/notes` pipeline reaches `runLocal`
because the original pipeline (with redirect on the AST) is fed
into warp. `rewriteQueryPipelines` matches the `cat /log/foo.log`
inner pipeline, transfers the redirect onto the `CallbackCommand`,
and runs warp. Warp:

1. Calls the callback's Run function with stdin/stdout/stderr.
   Callback executes the log query and writes `result.Text` (or
   joined Rows) to its stdout.
2. The callback's stdout is wrapped by warp's redirect machinery
   (because we set `CallbackCommand.Redirects`). Warp routes those
   bytes to the redirect target via shellfs.
3. Final result: `/tmp/notes` contains the rendered query, no
   coord-side write.

For redirects on non-source positions (`cat /log/a > /tmp/b |
grep ERROR > /tmp/c` — multi-redirect pipelines), the same
machinery handles them: each command's redirects are honored
independently. The `> /tmp/b` on `cat /log/a` becomes a callback
redirect; the `> /tmp/c` on `grep` is a plain SimpleCommand
redirect that warp handles natively.

## Testing

Coverage cases (existing + new):

- `TestCoordinatorQuery_LogQueryRedirectToTmp` — already exists.
  Tests `cat /log/vimgym.log | awk '$LEVEL == "ERROR"' > /tmp/errs`.
  Should pass unchanged after the rewrite carries the redirect.

- New: `TestCoordinatorQuery_LogQueryAppendRedirect` —
  `cat /log/foo.log >> /tmp/notes` over an existing file. Asserts
  the contents are appended, not replaced.

- New: `TestCoordinatorQuery_MultipleRedirectsInPipeline` —
  `cat /log/a > /tmp/b | grep ERROR > /tmp/c`. Asserts both files
  exist with the right content.

- New: `TestCoordinatorQuery_LogQueryRedirectToBin` —
  `cat /log/foo.log > /bin/script` (writing into the user's bin
  dir). Asserts shellfs handles the namespace correctly via warp.

Warp-side tests in `pkg/bash`:

- `TestCallbackCommand_RedirectToFile` — callback writes "hello\n"
  to stdout; pipeline carries `> /tmp/x`; verify the FS sees
  "hello\n".

- `TestCallbackCommand_AppendRedirect` — same, but with `>>`.

- `TestCallbackCommand_NoRedirectStillWorks` — callback with empty
  Redirects field; behavior identical to today.

## Estimated effort

- Warp: ~150 lines + tests. Half a day.
- Weft: ~200 lines deleted, ~10 lines added in query_rewrite.go.
  Half a day including running existing tests.
- Total: ~1d.

Cross-repo: warp commit lands first (additive, no breakage). Weft
follows once warp's commit is pinned via `go.mod` replace.

## Non-goals

- **Streaming the redirect.** Today and after this change, the
  redirect target is written as one blob (whether via
  `writeQueryResultToFS` or via warp's redirect capture buffer).
  Streaming the write — appending to `/tmp/notes` as each line is
  produced — is a separate concern and likely won't matter until
  someone redirects a `tail -f` stream.

- **Pipe-targeted redirects** (`cat /log/foo.log >&2`). Stderr
  redirection to fd 2 is rare for log queries and warp's existing
  fd handling covers it. No special weft work.

- **Heredoc / process-substitution.** Not used in any current weft
  pipeline. Out of scope.

## Cross-link

- Phase 3 commit `d5b30cc` (planner Plan strict + Local fields
  removed) where this carryover was first noted.
- `end-to-end-streaming.md` — once redirects flow through warp,
  the streaming path's "needsBuffering" check can drop the
  redirect precondition. Independent landing order; either first.
