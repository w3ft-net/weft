# The Warp Interpreter

Weft's shell is built on **warp**, a bash/lua interpreter that
lives as a sibling repo at `../warp`. Weft doesn't fork warp — it
imports it as a Go module and extends it at well-defined hook
points: a virtual filesystem, custom commands, and an AST rewriter
that intercepts log queries before they execute.

This section explains warp's architecture, then traces exactly how
weft wires into it.

## Warp's structure

Warp lives at `github.com/graybeardcorp/warp`. The `go.mod` replace
directive points it at `../warp` so changes to both repos can be
developed in lockstep.

Key packages under `warp/pkg/`:

- **`bash/`** — the core interpreter. Parses shell scripts into
  an AST, executes them with variable expansion, redirects, pipes,
  control flow, and function definitions. This is where the
  `Interpreter` type lives.
- **`shellcore/`** — the interactive shell layer. Terminal
  handling, syntax highlighting, prompt rendering, key bindings,
  help provider interface, view registration. Weft's shell UI is
  built on `shellcore.ShellCore`.
- **`syntax/`** — AST node types for bash, lua, awk, sed, jq.
- **`awk/`, `sed/`, `jq/`, `lua/`** — tool interpreters that run
  natively (no exec, no subprocess).

### The Interpreter

`bash.Interpreter` (defined in `bash/exec.go`) is the VM that
executes parsed ASTs. Its public surface:

```go
interp := bash.NewInterpreter()
interp.SetStdin(r)
interp.SetStdout(w)
interp.SetStderr(w)
interp.SetFS(myFS)                       // virtual filesystem
interp.RegisterCommand("mycmd", fn)       // custom builtin
script, _ := bash.Parse("cat foo | grep bar")
interp.ExecScript(script)
```

Key hook points weft uses:

- **`SetFS(fs bash.FS)`** — replaces the default (real) filesystem
  with a virtual one. The `bash.FS` interface requires `ReadFile`,
  `WriteFile`, `ReadDir`, `Stat`, and related methods.
- **`RegisterCommand(name, fn)`** — adds a command that the
  interpreter calls instead of looking for an external binary.
- **`BinScriptTransform`** — a callback invoked on `/bin/` script
  ASTs before execution. Weft uses this to rewrite query pipelines
  inside user-defined scripts.

### Streaming execution

`bash.RunStreaming(script, stdin, stdout, stderr)` runs a pipeline
with io.Pipe plumbing. The compute pool uses this to run
post-pushdown stages against merged shard output.

## How weft configures warp

The integration point is `coordinator/coordinator.go`, in the
`runLocal()` method. This is called for every non-query pipeline
(scripts, file operations, anything that doesn't start with
`cat /log/`). Here's the sequence:

1. **Create a fresh interpreter per request:**
   ```go
   interp := bash.NewInterpreter()
   interp.SetStdin(strings.NewReader(""))
   interp.SetStdout(stdoutW)
   interp.SetStderr(stderrW)
   ```

2. **Register tool commands** (awk, sed, jq as native builtins):
   ```go
   bash.RegisterToolCommands(interp)
   ```

3. **Install the virtual filesystem:**
   ```go
   interp.SetFS(shellfs.New(c.Store, custID, logApps, opts...))
   ```

4. **Register weft-specific commands** (aggregation helpers,
   version history):
   ```go
   wf := &WeftFuncs{Store: c.Store, CustomerID: custID}
   RegisterWeftCommands(interp, wf)
   witFn := &WitFuncs{Store: c.Store, ...}
   RegisterWitCommands(interp, witFn)
   ```

5. **Install the query rewriter** (intercepts `cat /log/...`
   pipelines):
   ```go
   rewriteQueryPipelines(ast, c, tr, customer, ...)
   interp.BinScriptTransform = func(_ string, script *bash.Script) {
       rewriteQueryPipelines(script, c, tr, customer, ...)
   }
   ```

6. **Execute with timeout:**
   ```go
   ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
   interp.ExecScript(ast)
   ```

## ShellFS: the virtual filesystem

`shellfs/shellfs.go` implements `bash.FS` backed by PostgreSQL.
It presents this tree to the shell:

```
/log/<ns>/<svc>/<kind>.log — read-only pseudofiles (shard queries)
/bin/<name>            — user-defined bash/lua scripts
/cron.d/<name>         — scheduled watch definitions
/etc/notify.yaml       — notification channel config
/etc/dashboards/*.yaml — named dashboards
/tmp/*                 — per-customer scratch files (48h TTL)
```

The `ShellFS` struct holds the customer ID, subscription tier,
available log apps, and user identity. Every operation checks
permissions — namespace-level reads and per-resource writes.

Key methods:

- `ReadFile(path)` — returns file contents from the store, or a
  pseudofile marker for `/log/` paths (the actual log data flows
  through the query path, not through FS reads).
- `WriteFile(path, data)` — validates content (e.g., dashboard
  YAML must parse, cron schedule must be valid) and saves to the
  store with version history.
- `ReadDir(path)` — lists directory contents, merging store
  queries with static entries.

## Query rewriting

The most intricate part of the warp integration is query
rewriting. When the user types:

```
cat /log/myco/api/main.log | grep ERROR | awk '{print $3}' | wc -l
```

Weft doesn't actually read a file and pipe it through grep. The
AST rewriter (`coordinator/query_rewrite.go`) walks the parsed
pipeline, detects that it starts with `cat`/`head`/`tail` on a
`/log/` path, and replaces the entire pipeline with a
`bash.CallbackCommand` that calls `coordinator.runQuery()`.

The callback:

1. Sends the pipeline text to the **planner**
   (`planner/planner.go`), which analyzes each stage and decides
   what can be pushed down into shard-level SQL.
2. Pushable stages (grep, awk field equality, head/tail limits)
   become `WHERE` clauses and `LIMIT`s in `ShardSQL`.
3. Non-pushable stages (sort, wc, complex awk) stay in
   `ComputeStages`.
4. The coordinator fans out `ShardSQL` to matching shards, merges
   the results, and pipes them through the compute stages via
   `bash.RunStreaming`.

The `BinScriptTransform` hook ensures this rewriting also applies
inside `/bin/` scripts — user-defined scripts that contain log
queries get the same pushdown treatment.

### What the planner recognizes

The planner walks stages left to right, accumulating SQL:

| Stage | Pushdown rule |
|---|---|
| `grep PATTERN` | `WHERE raw ILIKE '%PATTERN%'` |
| `grep -v PATTERN` | `WHERE raw NOT ILIKE '%PATTERN%'` |
| `awk 'LEVEL == "ERROR"'` | `WHERE level = 'ERROR'` |
| `awk 'HOST ~ "api-"'` | `WHERE instance LIKE 'api-%'` |
| `jq 'select(._env == "prod")'` | `WHERE env = 'prod'` |
| `head -n 100` | `LIMIT 100` (ASC order) |
| `tail -n 50` | `LIMIT 50` (DESC order) |

Once the planner encounters a stage it can't push (e.g., `sort`,
`uniq -c`), everything from that point on becomes a compute stage.
The boundary between pushed and computed is permanent — stages
don't get reordered.

Key planner files:

- `planner/planner.go` — the `Plan()` function, entry point.
- `planner/grep.go` — grep predicate extraction.
- `planner/awk.go` — awk field/meta-variable extraction.

## Custom commands

Weft registers two families of custom commands on the interpreter:

### Aggregation helpers (`coordinator/weftcmds.go`)

These read cron results from the store and compute aggregates:

- `weft.latest(cron)` — most recent result as a string.
- `weft.avg(cron, duration)` — mean of numeric results.
- `weft.max`, `weft.min`, `weft.sum`, `weft.stddev` — similar.
- `weft.count`, `weft.percentile`, `weft.delta`, `weft.slope`,
  `weft.rate` — more exotic aggregations.

These are available everywhere: in the interactive shell, in
`/bin/` scripts, in cron bodies, and in Lua scripts.

### Version history (`coordinator/wit.go`)

Git-like commands for `/bin/` and `/cron.d/` files:

- `wit log <path>` — list versions (optional `-p` for patches).
- `wit show <path> <ref>` — print a specific version.
- `wit diff <path> <a>..<b>` — unified diff between versions.
- `wit checkout <path> <ref>` — restore a past version.
- `wit amend <path> -m "msg"` — update the HEAD commit message.

### Chart (`chart/builtin.go`)

Registered via `chart.Register(core, watchResultsProvider)`,
which installs three hooks on shellcore:

- A full-screen chart view.
- A `chart` command for the command line.
- A pipe-stage renderer (`... | chart`).

## The interactive shell

`shell/shell.go` sets up the `shellcore.ShellCore` instance with
weft-specific configuration: custom prompt, syntax highlighting
that recognizes `/log/` paths, the help provider, and the chart
views. The `Shell.Run()` method is the REPL entry point.

The shell runs both in the terminal (native Go) and in the browser
(compiled to WebAssembly, rendered in xterm.js via
`frontend/src/pages/Shell.tsx`).

## Execution flow diagram

```
User types: cat /log/myco/api/main.log | grep ERROR | wc -l
    │
    ▼
bash.Parse() → *bash.Script (AST)
    │
    ▼
rewriteQueryPipelines() detects /log/ source
  replaces pipeline with CallbackCommand
    │
    ▼
interp.ExecScript(ast)
  → CallbackCommand fires → coordinator.runQuery()
    │
    ▼
planner.Plan(pipeline, customer, start, end)
  → ShardSQL:      "SELECT ... WHERE raw ILIKE '%ERROR%'
                     AND customer='acme' AND ts >= ..."
  → ComputeStages: ["wc -l"]
    │
    ▼
coordinator.executeQuery()
  → fan out ShardSQL to matching shards (gRPC)
  → merge results by line_num
  → pipe through compute stages via bash.RunStreaming
    │
    ▼
Output: "42\n"
```

---

## Challenge: add a pushdown rule for `cut`

**Goal:** teach the planner to push `cut -d',' -f2` into shard
SQL as a projection (the `{raw_expr}` in
`SELECT line_num, {raw_expr} as raw`), instead of running it as a
compute stage.

### Setup

Read these files first:

- `planner/planner.go` — the `Plan()` function and how it walks
  pipeline stages.
- `planner/grep.go` — an existing pushdown implementation
  (predicate extraction for grep).
- `planner/awk.go` — another pushdown implementation (field
  equality for awk).
- `proto/weftpb/weft.proto` — the `QueryRequest` message and
  how `ShardSQL` is transmitted.

Understand the boundary: a pushdown rule converts a shell command
into SQL. It only fires when the command's arguments are simple
enough to express in SQL. Complex cases fall through to compute.

### Task

1. Create `planner/cut.go`. Implement a function that recognizes
   `cut -d<delim> -f<N>` where `<delim>` is a single character
   and `<N>` is a single integer field number.

2. When recognized, rewrite the raw expression in the SQL from
   `raw` to a DuckDB `string_split` projection:
   ```sql
   string_split(raw, ',')[2] as raw
   ```
   (adjusting the delimiter and field number from the arguments).

3. Add the call to `Plan()` in the stage-walking loop, right
   after the existing grep/awk checks. If `cut` is recognized,
   update the projection; if not, treat it as a compute stage.

4. Write `planner/cut_test.go` with cases for:
   - Simple `cut -d',' -f2` → SQL projection.
   - Multi-field `cut -f1,3` → falls through to compute (too
     complex for v1).
   - No delimiter (default tab) → SQL projection with `E'\t'`.
   - `cut` after a non-pushable stage → compute (the boundary
     rule).

### What you'll learn

- How the planner's stage-walking loop works.
- The difference between predicates (WHERE) and projections
  (SELECT expression) in pushdown.
- How to add a new file to an existing Go package and wire it
  into the main logic.
- The DuckDB SQL dialect for string operations.
