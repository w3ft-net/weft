# /bin script body query detection

**Status:** landed.

Today `expandBinPipeline` in `coordinator/bin_expand.go` splices a /bin script's body into the caller's pipeline — but only if the body is a single pipeline of simple commands. Any body with a top-level `&&`, `||`, or `;`, a `for`/`while`/`if`, or a pipeline whose leading command is a compound command, bails out and the planner never sees the `cat /log/...` hidden inside. The invocation becomes a Local plan and `runLocal` hands the whole thing to the warp bash interpreter, which reads `/log/` files through `shellfs.ReadFile` (which returns empty bytes — `/log/*` is a planner-intercepted pseudofile, not real data) and the query quietly evaluates to nothing.

The goal is to make that caveat go away. A body like

```
echo "starting scan"
cat /log/$1 | awk '$LEVEL == "ERROR"' | wc -l
echo "finished"
```

invoked as `scan api.log` should dispatch one shard query for the middle pipeline (awk pushed down), run the two `echo`s locally, and stitch stdout together in body-list order. More generally: every pipeline inside a script body whose leading command is a pseudofile source should become its own query job at execution time. Non-query pipelines keep running locally through the bash interpreter. The user-facing promise becomes: write bash normally; we push down what we can.

## Why this matters

- **Scripts are the canonical unit of reuse.** Customers save `/bin/scan`, `/bin/daily`, `/bin/errors_by_svc`. The moment they add an `echo` preamble, an `|| alert` wrapper, a `for` loop, or a `local count=$(...)`, the whole body silently loses pushdown — and the script that worked fine at 10 MB of logs melts down at 10 GB.
- **The failure mode is silent.** No error, no warning. The query just returns empty because `runLocal`'s FS-backed `cat /log/api.log` reads an empty pseudofile.
- **The fix is a local change.** No schema, no migration, no protocol version. An AST pass at script-load time and a new AST node type in warp.

## Hard constraint: warp stays weft-agnostic

Warp (github.com/graybeardcorp/warp) is a shared library — weft and vimgym both link it, and any other kevlarcon tool might later. Warp's API carries zero weft vocabulary: no "query," no "planner," no "pseudofile," no "source command." The extension points it grows for this work — `CallbackCommand` and `BinScriptTransform` — are generic, usable for anything an embedder wants to do with /bin script bodies.

## Design

### The shape: AST substitution

The coordinator replaces every query-shaped pipeline inside a /bin script body with a `CallbackCommand` AST node that closes over the original pipeline + the coordinator + the request scope. Warp walks the rewritten tree and dispatches each callback exactly once as part of normal pipeline execution.

**Rewrites fire at two points:**
1. On the top-level AST inside `runLocal`, covering pipelines the caller typed directly (a safety net — the splicer usually handles this case).
2. Via `interp.BinScriptTransform`, on each /bin/<name> script body as warp parses it at invocation time. This is where the interesting cases live: loops, command substitutions, function bodies, compound scripts.

Both paths call the same rewriter.

### Why not a per-pipeline hook

A prior iteration of this plan proposed a `PipelineInterceptor` interface: warp offers every pipeline to the embedder, who decides whether to claim it. That was implemented, then discarded. The reasons:

- **Classification is a compile-time property, not a runtime event.** Whether `cat /log/api.log | grep ERROR` is a weft query is fixed once the script is loaded. Making it a per-execution decision imposed cost for nothing.
- **Rendering a pipeline to source for classification re-runs cmdsubs.** `local count=$(cat /log/foo.log)` fired the inner `cat` pipeline twice — once when the outer pipeline's hook rendered `local count=$(cat /log/foo.log)` to classify, once when warp's normal word expansion evaluated the cmdsub. A cheap name-gate on the embedder side patched the symptom but left the principle wrong: the hook was inviting embedders to *do work to decide*, when the real goal is for the decision to be made in advance.

### Why not a pre-planner AST-walk

`expandBinPipeline` already does pre-planner AST rewriting — it splices single-pipeline bodies into the caller's pipeline. Extending it to walk every pipeline inside a body and plan each one would be zero-warp-change, but it breaks on loops (`for app in api web; do cat /log/$app.log; done`) and command substitutions with dynamic args: you can't plan `cat /log/$app.log` before `$app` has a value.

### Why not a synthetic-command rewrite

Rewriting each query pipeline into `__weft_query "<pipeline-source>"` and registering `__weft_query` via warp's existing `RegisterCommand` was tempting — zero new warp concepts. It fails on shell quoting: once a pipeline like `cat /log/api.log | awk '$LEVEL == "ERROR"'` is flattened into a double-quoted string argument, warp can't distinguish `$LEVEL` inside a single-quoted awk program (no expansion) from a loop variable (expansion required). You lose the AST's part-level classification.

### The chosen shape

```go
// warp/pkg/bash/ast.go
type CallbackCommand struct {
    Name string
    Run  func(interp *Interpreter, stdin io.Reader, stdout, stderr io.Writer) int
}
func (*CallbackCommand) node()    {}
func (*CallbackCommand) command() {}

// warp/pkg/bash/exec.go — execCommand gains one case
case *CallbackCommand:
    if c.Run != nil {
        interp.exitCode = c.Run(interp, interp.stdin, interp.stdout, interp.stderr)
    }

// warp/pkg/bash/exec.go — new Interpreter field, applied after Parse
// and before execScript in the /bin resolution path.
type Interpreter struct {
    // ...existing fields...
    BinScriptTransform func(name string, script *Script)
}
```

```go
// weft/coordinator/query_rewrite.go
func rewriteQueryPipelines(script *bash.Script, c *Coordinator, tr TimeRange, customer string, alertCtx *AlertContext) {
    bash.Walk(script, func(n bash.Node) bool {
        pipe, ok := n.(*bash.Pipeline)
        if !ok || !isQueryShape(pipe) {
            return true
        }
        snap := *pipe
        pipe.Commands = []bash.Command{
            &bash.CallbackCommand{
                Name: "weft-query",
                Run: func(interp *bash.Interpreter, _ io.Reader, stdout, stderr io.Writer) int {
                    src := bash.UnparsePipelineExpanded(&snap, interp.ExpandWord)
                    result, err := c.query(src, tr, customer, alertCtx)
                    if err != nil {
                        fmt.Fprintf(stderr, "query: %v\n", err)
                        return 1
                    }
                    io.WriteString(stdout, result.Text)
                    io.WriteString(stderr, result.Stderr)
                    return 0
                },
            },
        }
        return false
    })
}

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
```

### Two structural guarantees

- **Variables resolve from the live interp.** The callback captures `snap` (the original pipeline AST) and calls `interp.ExpandWord` at Run time — so `$1` inside a function, `$app` inside a loop, and `$(pick_today)` in an arg all resolve against whatever interp state is current when warp reaches the callback. The embedder never does expansion before it needs to.
- **Recursion is bounded.** `c.query` inside the callback re-enters `expandBinPipeline` + `planner.Plan` + `executeQuery`. For a source pipeline the plan is non-Local, so `executeQuery` takes the shard-query path, not back to `runLocal`. In the rare case where `c.query` plans Local, `runLocal` fires again with a new AST that has no interior source pipelines to rewrite, bounding callback depth to AST depth.

## Handling specific shapes

- **`cat /log/$1.log | grep ERROR` with $1=api.log** — rewrite replaces the pipeline with a `CallbackCommand`. At run time, the callback renders via `interp.ExpandWord`; $1 resolves to `api.log`; `coord.query("cat /log/api.log | grep ERROR")` fires.
- **`for app in api web; do cat /log/$app.log | grep ERROR; done`** — the for-loop body is a script; the rewrite descends into it and replaces the inner pipeline with a `CallbackCommand`. warp's `execFor` sets `app` per iteration and calls the callback; each iteration renders with the current `$app`.
- **`count=$(cat /log/api.log | grep ERROR | wc -l)`** — the cmdsub body is a script; the rewrite descends into it and replaces the inner pipeline. warp's `expandCommandSub` runs the callback with captured stdout; the captured value lands in `count`.
- **`cat /log/api.log | my_custom_bash_fn`** — the whole pipeline gets wrapped in a `CallbackCommand` (leading command is `cat`). Inside the callback, `coord.query` runs `planner.Plan` which pushes the source command but leaves `my_custom_bash_fn` as a compute stage. The compute pool's interpreter resolves `my_custom_bash_fn` against the row stream — same as top-level behaviour today.
- **Alerts in terminal position** — enforced at save time (`validateAlertPosition` in `store/shell.go`). A body with an `alert` stage anywhere other than the terminal pipeline of its final top-level list is rejected on save. The interceptor path (via `coord.query`'s normal `splitAlertStage`) handles terminal alerts naturally.
- **Subshells, groups, conditionals** — `bash.Walk` descends into Subshell.Body, Group.Body, IfClause.Body + ElseBody, WhileClause.Body, CaseItem.Body, and FuncDef.Body. The rewrite reaches every pipeline that can execute.

## v1 scope exclusions

- **Follow mode (`tail -f`) inside a script body.** Follow mode has a distinct coordinator code path (streaming, TailBroker, subscription). The rewrite detects a pipeline starting with `tail`; if the pipeline has `-f`, `coord.query` will error cleanly at the callback. Users get a clear error instead of an empty-read hang.
- **Nested /bin queries through the compute pool.** If a query-shape pipeline's tail is itself a /bin call (compute stage), the compute worker's warp interpreter has no `BinScriptTransform` wired — so interior source pipelines in that nested /bin script fall through to `ShellFS`'s empty-pseudofile behaviour. Known limitation. v2 candidate.
- **Parallel `&` and subshell redirects `( ... ) > file`** — warp doesn't implement these today; nothing to do here.

## Files

**warp (sibling repo):**
- `pkg/bash/ast.go` — add `CallbackCommand` node type.
- `pkg/bash/exec.go` — add `BinScriptTransform` field on `Interpreter`; add `CallbackCommand` case in `execCommand`; apply the transform in the /bin PATH resolution branch; propagate `BinScriptTransform` through `fork()`.
- `pkg/bash/expand.go` — export `ExpandWord` as a public method on `Interpreter` (one-line wrapper around the existing `expandWord`).
- `pkg/bash/unparse.go` — add a `CallbackCommand` case that renders `<callback:Name>` for diagnostics; keep `UnparsePipelineExpanded` as the render helper for embedders that need parseable source.
- `pkg/bash/callback_command_test.go` — tests for the new node: runs once at top level, exit code propagates, bang negation, execution inside $(...), per-iteration inside `for`, ExpandWord reflects live state, unparse placeholder, and the `UnparsePipelineExpanded` round-trip.

**weft:**
- `coordinator/query_rewrite.go` (new) — `rewriteQueryPipelines` walker, `isQueryShape` predicate.
- `coordinator/coordinator.go` — `runLocal` installs the transform and runs the rewrite on its top-level AST; `executeQuery` threads `alertCtx` through so it reaches the callback's capture.
- `coordinator/bin_expand.go` — existing splicer gets one extra guard: fall through to runtime resolution when the body's pipeline contains any non-SimpleCommand (for-loops, subshells, etc.). The splicer only handles flat pipelines of simple commands; anything else is the rewrite pass's territory.
- `store/shell.go` — `validateSource` gains `validateAlertPosition` to enforce terminal-alert-only at save time.
- `coordinator/bin_pushdown_test.go` — trimmed to the SinglePipelineBody splicer guardrail.
- `coordinator/bin_interior_query_test.go` — end-to-end tests: echo-then-query-then-echo, positional arg per invocation, for-loop iteration, command substitution, `&&` short-circuit, function with conditional return (the motivating case), function defined in a separate /bin script.
- `store/shell_validate_test.go` — terminal alert accepted; interior alerts rejected (seven shapes); alert-free bodies with interior queries accepted.
- `docs/manual/scripts.md` — no caveat needed; the manual says "weft optimizes pipelined scripts well" and that's now accurate for multi-list bodies too.

## Risks

- **Expansion fidelity of `UnparsePipelineExpanded`.** The helper renders a pipeline back to source with dynamic parts expanded and single-quoted parts preserved. It needs to produce output that round-trips through `Parse`. The warp test `TestUnparsePipelineExpanded_RoundTrip` covers the common shapes; a test failure there signals a planner-facing regression.
- **Alert context across interior queries in a watch.** A watch's terminal alert is peeled by the outer `coord.query`; interior pipelines with their own `| alert` are rejected at save time. Unlikely to surprise but worth re-verifying when we add tests for watches invoking /bin scripts with interior alerts.
- **Resource amplification from loop bodies.** A `for app in $(ls /log); do ...; done` in a /bin script can fire N shard queries. The rewrite makes this a one-line-discoverable pattern. Per-customer rate limiting still applies; a per-invocation fan-out cap is v1.1 if users hit it.
- **Wall-clock for interior dispatches.** Each callback is a full `coord.query` — shard fan-out, compute pool, alert. Serial over the body. A three-query body runs three times slower than one. Parallelism-at-body-level is a later pass.
- **The compute pool's interpreter has no `BinScriptTransform`.** Noted in scope exclusions (5). A nested /bin query inside a compute-stage /bin hits the empty-pseudofile wall. v2.

## Critical files for implementation

- /Users/paul/dev/warp/pkg/bash/ast.go
- /Users/paul/dev/warp/pkg/bash/exec.go
- /Users/paul/dev/warp/pkg/bash/expand.go
- /Users/paul/dev/warp/pkg/bash/unparse.go
- /Users/paul/dev/weft/coordinator/query_rewrite.go
- /Users/paul/dev/weft/coordinator/coordinator.go
- /Users/paul/dev/weft/coordinator/bin_expand.go
- /Users/paul/dev/weft/store/shell.go
