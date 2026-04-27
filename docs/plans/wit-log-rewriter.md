# Plan: `wit log <path>` swallowed by RewritePaths

## Status

**Proposed.** Captured during the multi-customer demo verification:
`wit log /bin/scan` and `wit log /cron.d/error-rate` both fail with
`wit: unknown subcommand "/log"`. The bug is in warp's path
rewriter. The other four wit subcommands (`show`, `diff`,
`checkout`, `amend`) work fine — only `log` is hit because of a
naming collision with the customer's `/log` namespace root.

## The bug

`warp/pkg/shellcore/rewrite.go:rewriteSourceArgs` walks every
`SimpleCommand` in a parsed pipeline and rewrites `Words[1:]`
arguments that, when joined against `cwd`, classify as `PathQuery`.
The skip list is fixed: `cd`, `pushd`, `popd`, `pwd`, `dirs`. Every
other command — including embedder-registered builtins — gets its
positional args walked.

For `wit log /bin/scan` typed at the default cwd (`/`):

| arg | joined against `/` | ClassifyPath | rewriter action |
|---|---|---|---|
| `log` | `/log` | PathQuery (the customer's log namespace root) | **rewrite** to `/log` |
| `/bin/scan` | `/bin/scan` | PathLocal | leave alone |

The line arriving at the bash interpreter is `wit /log /bin/scan`.
Wit's argv-0 dispatcher sees `Words[1] == "/log"` and rejects with
`unknown subcommand "/log"`.

The other wit subcommands escape because of luck — `/show`,
`/diff`, `/checkout`, and `/amend` aren't real `/log/...`-shaped
paths and `ClassifyPath` returns `PathLocal` or `PathOther`,
leaving them untouched.

## Why a hardcoded skip in warp doesn't fit

Warp is shared with vimgym. The user's standing rule: keep warp's
API weft-agnostic; no query/planner/pseudofile vocabulary in its
type system. `wit` is a weft-specific command name; baking
`"wit"` into `rewriteSourceArgs`'s skip list pins weft semantics
into a shared library. The fix has to register `wit`'s grammar
*from weft's side*.

A purely-positive allowlist on the warp side ("only rewrite for
`cat`, `head`, `tail`, `less`, `vi`, `wc`, `stat`") also fails:
user-defined `/bin/<name>` scripts that take a pseudofile arg
(today's `mem_history /log/acme/prod/api.log`) wouldn't be in the
allowlist either. `expandBinPipeline` runs in the coordinator
*after* shell-layer `RewritePaths` (shell.go:516 → coordinator's
query() → expand at line 387), so by the time the script body
is inlined the path arg is already a bare name and the planner
rejects it. Today's rewriter handles those scripts correctly only
because it's permissive — restricting it breaks them.

## Existing extension surface

Warp already has the right shape sitting in `extension.go`:

- `RegisterBuiltin(name string, fn BuiltinFunc)` — adds an
  embedder-supplied top-level command handler. Used by weft to
  register `chart`, `range`, and could register `wit`. The
  doc-comment even names `wit log` as a representative example.
- `RegisterPipeStage(name, parse, render)` — pairs a parse + render
  for a `... | name` terminal stage.

Neither of these informs the rewriter today. `RewritePaths` is a
visitor over the parsed AST and doesn't consult the embedder
builtin registry.

## Recommendation

Extend warp's extension surface with a third hook that *only*
informs the rewriter:

```go
// RegisterSubcommandBuiltin marks `name` as a command whose first
// positional argument is a subcommand keyword (not a path). The
// rewriter skips rewriting Words[1] for these commands; Words[2:]
// are still walked and rewritten as normal.
//
// Use this for embedder commands shaped like `git log <path>`:
// the second word is a fixed keyword, the third+ are real paths.
func (c *ShellCore) RegisterSubcommandBuiltin(name string)
```

Implementation:

1. **warp/pkg/shellcore/extension.go** — add the registry
   (`embedderSubcommandBuiltins map[string]struct{}`) and the
   register/unregister functions.
2. **warp/pkg/shellcore/rewrite.go** — `rewriteSourceArgs` consults
   the registry. If `Words[0]` is a registered subcommand-shaped
   builtin, walk starts at `i := 2` instead of `i := 1`.
3. **weft/shell/shell.go** (or wherever else weft instantiates
   ShellCore) — call `core.RegisterSubcommandBuiltin("wit")` once
   at construction.

This stays weft-agnostic in warp (the registry is just a name set;
warp doesn't import wit semantics) and is a 2-line registration on
the weft side.

### Why not extend BuiltinFunc itself

`BuiltinFunc` is for **top-level** commands typed at the prompt —
the embedder owns the entire dispatch. `wit` doesn't need that:
its execution still happens via `interp.RegisterCommand` against
the bash interpreter (warp/pkg/bash), because `wit` is invoked
from inside a pipeline / sub-shell / etc. as well as bare. The
new hook is *purely* about the rewriter, decoupled from execution.

If a future weft command genuinely needs both ("custom prompt
dispatch + first-arg-is-subcommand"), it registers with both hooks.
They compose.

### Why not move RewritePaths after /bin expansion

Tempting — once `mem_history /log/...` has been inlined to its
`tail -n 360 /log/...` body, `tail` is a known path-taker and the
rewriter could be more conservative. But `expandBinPipeline` lives
in the coordinator (it needs the customer's ShellFS to resolve
`/bin/<name>`), and `RewritePaths` lives in the shell (it needs to
fold a transient dir-stack so `cd /log/acme && cat api.log`
rewrites to `cat /log/acme/api.log` before the planner sees it).
Moving the dirstack rewrite into the coordinator means duplicating
the dirstack across coordinator instances or RPC-ing it with every
query — fixing wit by punching a much bigger hole.

## What needs editing

This plan does not intend to land changes itself; it captures the
shape so the next session can do it discretely.

```
warp/
  pkg/shellcore/
    extension.go            # RegisterSubcommandBuiltin + registry
    extension_test.go       # round-trip + collision with builtins
    rewrite.go              # rewriteSourceArgs reads the registry
    rewrite_test.go         # `wit log /bin/scan` round-trip
weft/
  shell/shell.go            # core.RegisterSubcommandBuiltin("wit")
  shell/wit_log_test.go     # ExecNonInteractive("wit log /bin/scan")
```

Estimated diff: ~50 lines warp + ~5 lines weft + tests.

## Test plan

- **warp unit:** the rewriter leaves `Words[1]` alone for a
  registered subcommand-shaped builtin while still rewriting
  `Words[2:]`. Counter-test: an unregistered command name still
  has `Words[1]` rewritten (current behaviour preserved).
- **warp unit (regression):** unregistration removes the rewriter
  exception.
- **weft integration:** `weft sh -c 'wit log /bin/scan'` returns
  the three-version history we saw at session start (`Alice fix
  the awk filter` / `Bob tighten error pattern` / `Alice initial
  pass`). Add as a non-interactive test alongside the existing
  shell tests.
- **demo verification:** the multi-customer demo's "Authorship
  plumbing" step in `docs/plans/multi-customer-demo-seed.md` walks
  `wit log /bin/scan`. Once landed, that step actually runs.

## Open questions

- **Should `RegisterBuiltin` imply `RegisterSubcommandBuiltin`?**
  Today `chart <watch>` is registered via `RegisterBuiltin` and
  takes a watch-name first arg, not a path. If the watch name
  ever collided with a customer namespace root (`chart log`?),
  we'd hit the same bug. Auto-registering BuiltinFunc-registered
  names as subcommand-shaped is conservative and probably right.
- **Is there any other weft command with the same shape?** `weft.*`
  — `weft.latest`, `weft.avg`, etc. — take a watch name as first
  arg. If a customer has a watch literally named `log`, the same
  rewriter would mangle `weft.latest log` → `weft.latest /log`.
  Today nothing protects against that. Worth grepping the weft
  surface for other "command + keyword + path" shapes when the
  fix lands.
- **Does this affect the web shell?** The web shell also calls
  `RewritePaths` (via shellcore on the wasm side). The same
  `RegisterSubcommandBuiltin("wit")` registration on web-shell
  startup applies; symmetry with the CLI is the easy path.
