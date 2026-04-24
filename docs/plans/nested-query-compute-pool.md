# Nested query /bin invocations through the compute pool

**Status:** open. v2 of the /bin script body query rewrite (see `script-body-query-detection.md`).

## Problem

The query rewrite in `coordinator/query_rewrite.go` turns interior source-shaped pipelines into `CallbackCommand` nodes bound to `coord.query`. That works for interior queries that run on the coordinator's interpreter. It doesn't reach interior queries buried inside *compute stages* — bash stages of a weft pipeline that run on a compute worker rather than the coordinator.

Motivating shape:

```bash
# /bin/lookup — a user-defined helper that itself queries a log
cat /log/users.log | grep "$1" | awk '{print $2}'

# /bin/audit — calls lookup on ERROR rows from events.log
cat /log/events.log | grep ERROR | lookup "$event_field"
```

When `/bin/audit` runs:

1. `runLocal` applies `BinScriptTransform` when warp loads `/bin/audit`. The rewrite wraps the whole pipeline in a `CallbackCommand` (leading command `cat` is query-shape).
2. The callback calls `coord.query(…)`. The planner splits: `cat /log/events.log | grep ERROR` pushes into shard SQL; `lookup "$event_field"` is a compute stage.
3. The compute pool picks up the row stream plus the stage list `["lookup $event_field"]`. `compute/local.go:110` calls `bash.RunWithParallelism(pipeline, input, parallelism)`.
4. `RunWithParallelism` constructs a fresh `Interpreter` with `NewInterpreter` — no `fs`, no `BinScriptTransform`, no weft context at all. Warp's default FS is an empty `MemFS`.
5. Warp's `execSimple` looks up `lookup`: not a builtin, not a function, not a registered command. PATH resolution asks `interp.fs.ReadFile("/bin/lookup")` → empty MemFS → not found. Exit 127: `bash: lookup: command not found`.

So today a /bin script that calls another /bin script from a compute stage fails loudly with a command-not-found error. That's not silent data loss, but it is a broken feature: users reasonably expect their /bin scripts to compose. The workaround — inline everything into one script — defeats the point of having reusable helpers.

There is also a subtler problem. Even if we hook `ShellFS` into the compute worker so `/bin/lookup` is found, the interior `cat /log/users.log` inside it isn't rewritten (no `BinScriptTransform`). Warp's plain `cat` would try to read `/log/users.log` through `ShellFS`, which returns empty bytes for `/log/*` pseudofiles — silent zero-row result. That's the silent-failure mode the whole original rewrite was supposed to eliminate, resurrected one layer deeper.

## Why it's architecturally non-trivial

"Just wire the compute worker's interpreter the same way the coordinator's is" sounds easy, and at the local-compute-pool level it almost is: `compute/local.go` could accept a tenant context and configure the interpreter accordingly. But the wiring cascades:

- **Compute isn't always local.** There's a `compute/grpc_client.go` and a `compute/service/server.go`. The remote compute path means the interpreter running `lookup "$event_field"` may live in a different process, possibly on a different host. An interior query inside it needs *something* to call `coord.query` from the compute worker's side — either (a) the compute worker calls back across the network to the coordinator, (b) the compute worker talks to shards directly (needs customer auth, retention enforcement, all the things the coordinator currently centralises), or (c) the coordinator pre-plans the interior query and sends the compute worker a resolved result.
- **Security boundary.** Compute workers today are trust-minimised: they receive an SQL result stream and a pipeline string, they run bash on it, they return stdout. Giving them a coord-callback channel gives them the ability to issue queries — which means they must authenticate, rate-limit per tenant, observe retention, etc. The coordinator is the current home for all of that; replicating it on the worker, or proxying through the coordinator, is the actual cost.
- **Failure/retry/timeout posture.** Compute jobs have their own timeout and retry model. An inner `coord.query` call inherits a different model. Composing the two (outer job's deadline vs. inner query's deadline) isn't obvious.
- **Observability.** A user who runs one top-level query would see one request in logs. A nested /bin query fans that into N queries across a pool. Spans/traces need to stitch.

## Design space

### (A) Ban compute-stage /bin invocations at save time

Simplest. Walk each saved script's AST; for any pipeline whose post-source-command stages contain a reference to another /bin script, reject at save time with a clear error:

> `alert: /bin scripts called from a compute stage are not supported. Inline the helper or use a terminal alert instead.`

Pros: zero compute-side work; keeps the coordinator as the sole query-issuing component. Users get a clear error at the moment they save the offending script, not a runtime failure buried in a shard trace.

Cons: ergonomic cliff. The user types `cat /log/… | my_helper` and has to discover "no, you can't do that here." Forces them to inline, duplicating logic.

Detection: at save time we know (for each customer) the set of /bin script names. Any identifier in a compute-stage position that matches a /bin script name is a banned shape. The check has to account for scripts saved later; the simpler rule is "any identifier in a compute-stage position that isn't a well-known bash builtin or warp tool command." That errs toward restrictive.

### (B) Make the compute pool coordinator-aware

Wire `ShellFS` and `BinScriptTransform` into the interpreter `compute/local.go` constructs. Introduce a `ComputeContext` that carries `customerID`, `tr`, `coord`, `alertCtx`. The job runner threads it through to the per-stage `RunWithParallelism` call (or a new variant that takes it).

For the local compute path this is ~30 lines: `compute.LocalComputePool.NewJob` gets new parameters, the interpreter setup copies what `runLocal` does, `BinScriptTransform` captures the same context as in `runLocal`.

For the remote compute path (if we still have one in production — worth confirming before committing), the wiring is heavier. The compute worker needs:
- A coordinator endpoint address.
- An authentication token scoped to the originating request.
- A way to report progress / errors / retries.

Pros: users get composition for free. The mental model is consistent with the rewrite's existing promise.

Cons: the compute worker gains the ability to make queries, which is a meaningful expansion of its trust surface. And in the remote case, a worker pulling from the coordinator re-introduces every distributed-systems concern the coordinator already solved once.

### (C) Pre-resolve nested /bin calls to inline source at rewrite time

When the rewrite walks a /bin script body and sees a stage like `lookup "$1"`, detect that `lookup` is another /bin script and inline its source into the AST. Repeat until no /bin-script stages remain. Effectively, the rewrite becomes a whole-program inliner.

Pros: no runtime callback across the coordinator/compute boundary. All interior queries end up in the coordinator's interpreter where `BinScriptTransform` already handles them. Compute workers keep their current narrow trust.

Cons: source inlining is hairy — variable scoping (does `local` in a /bin script leak when inlined?), recursion (can /bin scripts call each other transitively? — yes, today they can), quoting fidelity (rendering an inlined script back to shell source has the same gotchas as `UnparsePipelineExpanded`). Probably feasible only for a restricted subset.

### (D) Route nested /bin calls through a narrow coord RPC

Define a single RPC from compute workers back to the coordinator: `RunCallback(custID, pipelineSource) -> (stdout, stderr, exit)`. Compute workers do NOT get general query capability; they get one narrow "run this pipeline as if the coordinator typed it" entry point, gated by auth token.

The compute worker's interpreter has a `CallbackCommand`-equivalent wired via `BinScriptTransform`, whose body is the RPC. The coordinator handles the actual query, returns the result. Compute worker streams into stdout and keeps processing rows.

Pros: narrow trust expansion (one RPC, well-defined contract). Consistent semantics with the in-process case. Remote compute path works.

Cons: new RPC, new auth path, new failure mode to handle (coord unreachable from compute worker). Non-trivial wire protocol change.

## Recommendation

**Ship (A) first**, as a short-term user-experience improvement: the current failure is command-not-found from a compute worker, which is opaque. A save-time validation error with a message that points at the offending stage is a strictly better experience while we work out the design for real support.

**Then evaluate (D) versus (B).** If the remote compute path is still a live deployment target, (D) is the right answer — it's the only option that keeps the compute worker's trust surface narrow. If compute is always local in production (worth confirming; there's `compute/grpc_client.go` and `compute/service/server.go`), (B) is simpler and cheaper.

**(C) is probably not worth it.** The inliner's edge cases are the same hard problems that made option (b) in the main plan — the synthetic-command rewrite — fail.

## Detection for (A)

The rewrite walker in `coordinator/query_rewrite.go` already identifies query-shape pipelines. Extending it to also inspect the *compute* stages (everything after a query-shape leading command) is a small addition:

```go
// During save-time validation (not rewrite): walk the AST, find query-
// shape pipelines, list their compute stages (Pipeline.Commands[1:]),
// and for each stage check whether its leading word's literal matches
// a known /bin script name for this customer.
```

The customer's /bin script name list changes over time. A script saved today that passes validation might become offending if the user saves a /bin script matching a compute-stage name later. Two options:

1. **Validate on every save** of any /bin script in a customer's namespace. When the user saves a new script, re-validate existing scripts against the updated name set. Scripts that now-offend get flagged (without being unsaved — probably surfaced via a warning banner in the UI; tolerating post-hoc inconsistency is simpler than forcing the user to re-edit old scripts).
2. **Validate only at execution time**, producing a clear error at the point of call rather than the point of save.

Option 2 is closer to the runtime semantics we want and the simpler implementation. Save-time validation can catch the obvious cases (inline `bash_func_that_is_a_bin_script` calls), and execution time catches the rest with the same error message shape.

## What (A) buys us for "free"

(A)'s error message can mention:
> "This script calls `lookup` from a compute stage. `lookup` is a /bin script, and /bin scripts cannot currently be invoked from compute stages. Either inline the logic into this script, or restructure so `lookup` is the terminal stage."

That last hint — "make it the terminal stage" — is a real workaround for many use cases. If the user's `cat /log/events.log | grep ERROR | lookup "$field"` can be restructured as a single-stage query followed by a separate shell invocation of lookup, the user doesn't actually need compute-stage /bin support.

## Files (for option A)

- `store/shell.go` — extend `validateSource` with `validateNoBinCallInComputeStage` (needs access to the customer's /bin namespace; today `validateSource` is pure function of the source — we'd add a Store method `SaveShellFunctionWithContext` or pass a predicate).
- `coordinator/query_rewrite.go` (or a new file) — runtime check that produces a similar error if the save-time check was stale.
- Tests: save a /bin script, save a second script that calls the first from a compute stage, assert the second save fails with a clear message.

## Open questions

- Is the remote compute path still a production deployment, or is everything local now? The answer changes whether (B) is viable.
- Do any existing saved scripts (in production customer accounts) hit this shape? If yes, (A) is a breaking change and needs a migration plan.
- Does /bin-to-/bin calling at the top-level (not in a compute stage) work today? That case goes through warp's PATH resolution on the coordinator's interpreter, which has ShellFS wired. So yes, top-level composition works. It's only compute-stage composition that breaks.
