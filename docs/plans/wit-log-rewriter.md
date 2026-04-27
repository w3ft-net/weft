# Plan: `wit log <path>` swallowed by RewritePaths

## Status

**Resolved — ready to land.** Captured during the multi-customer
demo verification: `weft sh -c 'wit log /bin/scan'` fails with
`wit: unknown subcommand "/log"`. The bug is in warp's path
rewriter, hit only via `ExecNonInteractive` (`shell/shell.go:523`,
the only `RewritePaths` call site in weft). The other four wit
subcommands (`show`, `diff`, `checkout`, `amend`) escape because
their keywords don't collide with a `/...` namespace prefix.

Interactive `weft` shell is **not** affected — the prompt path
sends the line straight through `ActionQuery` to the coordinator
without invoking `RewritePaths`. The bug only fires in `weft sh -c`.

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

Encode `wit`'s **grammar** at the rewriter call site — register
the explicit keyword set, and have the rewriter check `Words[1]`
against that set before applying its default path rules. The
registration is a parameter to `RewritePaths`, not a method on
`ShellCore`, because the only call site (`ExecNonInteractive`)
holds no `ShellCore` instance.

```go
// In warp/pkg/shellcore/rewrite.go
type RewriteOption func(*rewriteConfig)

// WithSubcommandBuiltin declares that `name` is a command with a
// fixed subcommand grammar. The rewriter checks Words[1] against
// `subs` — if Words[1] is one of the keywords, it is left alone
// (it is grammar, not a path). Words[2:] are still walked.
func WithSubcommandBuiltin(name string, subs ...string) RewriteOption

func RewritePaths(line string, ds bash.DirStack, opts ...RewriteOption) (string, error)
```

Weft's call site:

```go
shellcore.RewritePaths(weftPipeline, shellcore.NewTransientDirStack(),
    shellcore.WithSubcommandBuiltin("wit",
        "log", "show", "diff", "checkout", "amend"))
```

Implementation:

1. **warp/pkg/shellcore/rewrite.go** — add `RewriteOption`,
   `rewriteConfig`, `WithSubcommandBuiltin`. Convert
   `rewriteSourceArgs` to a closure-returning factory that
   captures the config; if `Words[0]` has a registered subcommand
   set and `Words[1]` is in it, walk starts at `i := 2`. Otherwise
   walk starts at `i := 1` (current behavior).
2. **weft/shell/shell.go** — pass `WithSubcommandBuiltin("wit", …)`
   to the existing `RewritePaths` call in `ExecNonInteractive`.

This stays weft-agnostic in warp (the registry is just `(name,
keyword-set)` data; warp doesn't import wit semantics) and is a
single-call-site change on the weft side.

### Why grammar, not "first arg is a name"

A weaker hook ("skip Words[1] for any registered name") would
also fix the bug, but it pretends user-supplied names like watch
identifiers (`chart <watch>`, `weft.latest <watch>`) need
grammar-level protection. They don't — if a user names a watch
`log`, `weft.latest log` colliding with `/log` is user error, not
a tooling bug. Subcommand keywords *are* grammar. Only register
the grammar.

### Why not extend BuiltinFunc itself

`BuiltinFunc` is for **top-level** commands typed at the prompt —
the embedder owns the entire dispatch. `wit` doesn't need that:
its execution happens via `interp.RegisterCommand` against the
bash interpreter (warp/pkg/bash), because `wit` is invoked from
inside a pipeline / sub-shell / etc. as well as bare. The new
hook is *purely* about the rewriter, decoupled from execution.

### Why not a ShellCore method

The plan originally proposed `(*ShellCore).RegisterSubcommandBuiltin`,
mirroring the existing `RegisterBuiltin` / `RegisterPipeStage`
methods. That doesn't work: `RewritePaths` is the only path with
the bug, and it's called as a package-level function from
`ExecNonInteractive`, which deliberately holds no `ShellCore`. A
method on `ShellCore` would not reach the bug. The variadic-option
parameter keeps the registry local to each call without globals
or a shared instance.

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

```
warp/
  pkg/shellcore/
    rewrite.go              # RewriteOption + WithSubcommandBuiltin;
                            # closure-returning visitor
    rewrite_test.go         # subcommand-keyword tests
weft/
  shell/shell.go            # WithSubcommandBuiltin("wit", "log", ...)
  shell/wit_log_test.go     # ExecNonInteractive round-trip assertion
```

Estimated diff: ~40 lines warp + ~3 lines weft + tests.

## Test plan

- **warp unit, positive:** with `WithSubcommandBuiltin("wit",
  "log", ...)`, `wit log /bin/scan` round-trips unchanged.
- **warp unit, Words[2:] still rewriting:** with the same
  registration, `wit log api.log` from `cwd=/log/acme` rewrites
  to `wit log /log/acme/api.log`.
- **warp unit, regression:** *without* registration,
  `wit log /bin/scan` becomes `wit /log /bin/scan` (documents
  that the bug exists when the grammar isn't passed in — i.e.,
  the registration is load-bearing, not redundant).
- **weft integration:** new `shell/wit_log_test.go` runs
  `ExecNonInteractive("wit log /bin/scan")` against a stub
  `Query` and asserts the pipeline reaching `Query` is exactly
  `wit log /bin/scan` (not `wit /log /bin/scan`).
- **demo verification:** the multi-customer demo's authorship
  step in `docs/plans/multi-customer-demo-seed.md` walks
  `wit log /bin/scan`. Once landed, that step works under
  `weft sh -c` as well as interactively.

## Resolved decisions

These were open questions before the design was tightened to
"register grammar, not name-shape":

- **Auto-register `RegisterBuiltin` names as subcommand-shaped?**
  No. `chart <watch>` and `range <duration>` take user-supplied
  arguments, not subcommand keywords. They have no fixed
  grammar to register. Auto-imply would give them protection
  they don't deserve.
- **Cover `weft.*` (`weft.latest`, `weft.avg`, …)?** No. They
  take a user-supplied watch name. If a customer names a watch
  `log` and runs `weft.latest log`, the rewriter mangling it
  is the same class of error as naming a file `--help` —
  user-side, not a tooling bug.
- **Web shell parity?** Not today. `cmd/wasm/main.go` does not
  call `RewritePaths`; the web shell sends pipelines straight
  to the coordinator over HTTP. If the web shell ever adopts
  client-side path rewriting, register `wit`'s grammar there
  the same way.
