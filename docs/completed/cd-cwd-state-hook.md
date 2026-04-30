# cd/pushd/popd/pwd as warp builtins, backed by an embedder-owned DirStack

## The mismatch

Interactive shell:
```
weft> cd /log/acme/prod
weft> cat api.log | head -1
```
Works. Today, `cd` is handled directly inside shellcore (`shellcore/core.go:590-607`):
shellcore mutates `c.cwd`, and on the next line `shellcore.resolveQueryPaths`
prepends cwd to bare names *before* the pipeline is forwarded to the planner.
The planner sees `cat /log/acme/prod/api.log`.

Non-interactive (`weft sh -c`):
```
weft sh -c "cd /log/acme/prod && cat api.log | head -1"
```
Errors with `unknown source: api`. `ExecNonInteractive` (`shell/shell.go:470`)
sends the whole pipeline straight to `s.Query`, which calls `planner.Plan`.
The planner runs at parse time, *before* warp executes anything; it sees
`api.log`, extracts `app="api"`, and the customer doesn't own that namespace.

**Verdict:** not a regression. Interactive works because shellcore is the
stateful layer that sits *between* user input and the planner. `&&` packs the
`cd` and the source command into a single planner invocation, so there's no
between-the-commands moment for the rewrite to land.

## Why the planner can't just "do cd later"

The planner extracts source apps at parse time so it can:
- build pushdown predicates (`app = '...'` / `app IN (...)`),
- pick the right shards via `Registry.MatchingShards`,
- gate the request through the per-customer namespace allowlist
  (`coordinator/coordinator.go:462-474`).

All of that has to happen before warp executes a single command. So the cwd
needed by `extractApp` must be known at parse time, not at execute time. We
can't move source resolution into warp's executor without giving up SQL
pushdown.

## The design: warp owns the builtins, the embedder owns the state

Two pieces, separable:

### 1. Builtins in warp

Move `cd`, `pwd`, `pushd`, `popd` (and likely `dirs`) into warp as first-class
builtins, registered through the existing `BuiltinFunc` table
(`pkg/bash/builtin.go:13-58`). Warp already has stub `cd`/`pwd` that just
poke a `PWD` var (`builtin.go:310-325`); these get replaced.

Each builtin owns:
- bash-compatible argument parsing (`cd -L`/`-P`, `pushd -n`, `popd +N`/`-N`,
  `dirs -p`/`-v`/`-c`),
- bash-compatible error messages and exit codes,
- delegation to the embedder's `DirStack` for all state reads and writes.

They remain overridable like any warp builtin: an embedder can register a
replacement, a user can shadow with `function cd { ... }`, or a script can
bypass with `command`. That property matters — weft's interactive shell may
want a thin wrapper over the warp builtin (e.g. to update prompt cache,
re-render after cd) without losing the bash semantics.

`pwd -L` vs `-P` is moot for weft (no symlinks); warp can implement both
correctly for vimgym, and weft's DirStack will return the same string for
either form.

### 2. DirStack interface in warp

Warp defines:

```go
// DirStack is the embedder-owned source of truth for the working directory
// and dirstack. All warp builtins (cd, pushd, popd, pwd, dirs) read and
// write through this interface; warp itself caches nothing.
type DirStack interface {
    Cwd() string                              // pwd's output
    Chdir(path string) error                  // cd; path is what the user typed
    Push(path string) error                   // pushd
    Pop() (popped string, err error)          // popd
    Stack() []string                          // dirs; first entry is current Cwd
}
```

Warp ships a default in-memory `DirStack` for embedders that don't care to
override. The interpreter exposes a setter:

```go
func (i *Interpreter) SetDirStack(d DirStack)
```

When `SetDirStack` is unset, warp uses the in-memory default (initialized
from `os.Getwd()` for CLI-style embedders, or `"/"` for VFS embedders).

**Path resolution boundary.** Open question worth pinning down: does warp
normalize the argument (`.`, `..`, `~`, relative→absolute against current
`Cwd()`) before calling `Chdir`, or does the embedder do that? I lean toward
warp handling tilde and `.`/`..` normalization but leaving "absolute" to the
embedder, because what counts as absolute is embedder-specific (weft's
`/log/...` namespace tree, vimgym's repo-rooted paths). The argument arrives
at `Chdir` already lexically clean, the embedder validates and either
accepts or returns an error that warp surfaces as `cd: <path>: <reason>`.

**Embedders that weft and vimgym want:**

- weft (`shellcore`): the DirStack validates against the pseudofile
  filesystem (`shellFS.ReadDir`) and updates the prompt's cached cwd. This
  replaces the inline `cd` handling at `shellcore/core.go:590-607`.
- vimgym: a DirStack that tracks both the editor's current buffer dir and
  the repo root, with Chdir bounded by the workspace.

## Parse-time rewriting still needs an AST hook

DirStack is a *runtime* contract. It doesn't fix the `weft sh -c "cd X &&
cat foo"` case on its own, because the planner runs over the whole input
before warp executes anything — `Chdir` never gets called in time.

So we still need a parse-time rewrite step. The cleanest shape uses warp's
AST and a *transient* DirStack:

```go
// In warp:
func RewriteAST(ast *bash.Pipeline, ds DirStack, visit func(cmd *bash.SimpleCommand, cwd string)) error
```

The embedder calls `RewriteAST` with:
- a transient DirStack initialized from the real one's `Cwd()`/`Stack()`,
- a visitor that, for each `SimpleCommand`, gets the *simulated* cwd at
  that point in the pipeline.

The visitor mutates the AST in place (rewrites bare-name args to absolute
paths). When the visitor sees a `cd`/`pushd`/`popd`, it calls the
corresponding method on the transient DirStack so subsequent commands see
the right cwd. The real DirStack is untouched — when warp later executes
the rewritten pipeline, it runs the *real* `cd` against the *real*
DirStack, and end state matches.

This is the original plan's "weft-side pre-pass," generalized:
- The AST visitor is warp's job (one stable visitor for both projects).
- DirStack semantics are shared with runtime, so the simulation is
  faithful.
- Embedder-specific logic (which args are paths, what counts as bare-name)
  stays in the visitor closure.

## Migration in weft

1. Land `DirStack` + builtin upgrades in warp; ship default in-memory impl
   so existing warp consumers don't break.
2. In shellcore, implement `DirStack` against the pseudofile FS. Wire it
   into the warp interpreter (`shell/backend_*.go`). Delete the inline
   `cd` handling at `core.go:590-607`; warp's builtin now does it.
3. Lift `resolveQueryPaths` out of shellcore into a shared
   `planner.RewritePaths(ast, ds)` that uses `bash.RewriteAST`. Both the
   interactive path (one statement at a time) and `ExecNonInteractive`
   call it before handing the AST to the planner.
4. Once both paths use the same rewrite, the `&&` bug disappears
   automatically — the visitor walks across the chain folding `cd`/`pushd`/
   `popd` into the transient DirStack as it goes.

## What this buys us

- One source of truth for cwd state (the embedder), one set of builtin
  semantics (warp).
- `pushd`/`popd`/`dirs` available everywhere warp runs, free.
- Lua and bash overrides keep working — DirStack sits *under* the builtin,
  so a user override that calls `command cd` still routes through it.
- Vimgym gets the same plumbing without copy-pasting weft's logic.
- The `&&` non-interactive bug becomes a side effect of the larger fix
  rather than a one-off pre-pass.

## Scope and order

- **Warp PR**: DirStack interface + default impl, full
  cd/pushd/popd/pwd/dirs builtins, `RewriteAST` visitor, tests against
  default DirStack. ~2-3 days.
- **Weft PR (depends on warp PR)**: shellcore DirStack impl, swap inline
  `cd` for warp builtin, lift `resolveQueryPaths` into
  `planner.RewritePaths`, call from both interactive and non-interactive
  paths, regression tests for the `&&` case. ~1 day.
- **Vimgym**: opportunistic — adopt when it next needs cwd-aware
  resolution, no urgency.

## Decisions

- **Path normalization**: warp does lexical (`~`, `.`, `..`); embedder does
  semantic (validation, what counts as "absolute"). Argument arrives at
  `Chdir` lexically clean.
- **Default warp DirStack is string-only** — never calls `os.Chdir`, never
  touches a real filesystem. Embedders that want fs effects wrap the
  default and add their own behavior.
- **`cd -` (OLDPWD) is out of scope** for the first pass. Skip the `Prev()`
  slot on DirStack; revisit if users ask for it.
- `pushd`/`popd` with no args: bash swaps top two stack entries. Cheap to
  support; do it.
- `dirs -c` (clear stack): cheap, expected. Include.
- Subshells and `( ... )` groups: AST rewrite treats them as opaque. Rare
  in this surface; safe fallback is no rewrite.
