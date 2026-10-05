# Cron truncation lint

## Problem

A cron pipeline that puts a row cap on its source stage — `tail -n N`,
`head -n N`, or a `/bin/` script that wraps one (`log-pattern-count`
expands to `tail -n $2 ... | grep -c "$3"`) — silently drops anything
past N. Each cron fire is already bounded to a non-overlapping
epoch-aligned window of exactly one interval (`CronSlot.Window()` in
`coordinator/cron_slot.go`), so there's no
*duplication* across ticks, but within one tick the cap is still a
cap: if a log source produces more than N matching rows in one
schedule interval, the excess is dropped before any downstream
`grep -c` / `wc -l` / `uniq -c` ever sees it, and the cron's count is
silently wrong. `stream-errors` (`tail -n 5000 ... 'stream error'`
every 5m), `christen-failures` (`tail -n 1000 ...` every 5m), and any
future cron shaped like them are exposed to this today, with nothing
that would tell the person saving it.

Confirmed with Paul (2026-09-19):
- **Purely static** — pattern-match the pipeline shape, no query
  against real log volume. Simpler, and accepted false-negative risk
  (a generous-looking cap that's still too small for actual traffic
  won't be caught) in exchange for zero added save-time query cost.
- **Warn, never block** — this is a heads-up, not a validation error.
  The save always succeeds.
- **One shared detector**, not three copies. `store.SaveCronWatch`
  already *is* the single call site for two of the three surfaces,
  and the third (MCP) funnels through the same HTTP endpoint as the
  second.
- (2026-09-19, revised) **No truncation-specific plumbing at all.**
  Paul's follow-up: don't build a `warnings []string` channel that
  exists only to carry this one check. Build a generic non-blocking-
  message mechanism that `SaveCronWatch` (or any validator) can use
  for *any* reason, present or future, and have all three surfaces
  render "whatever came back" through one generic path — no surface
  should have a branch that says "if this is the truncation warning,
  do X." The truncation lint becomes the first, not the only, user of
  that mechanism. See "Generic warning mechanism" below — this
  replaces the `warnings []string` design in the previous revision of
  this doc.

## Detection logic

Reuse `planner.Plan()` — already a pure, DB-free function used
throughout the planner test suite — to get the pipeline's
`QueryPlan{Limit, TailMode, SourceCmd}`. `Limit > 0` means a
head/tail (or NR-window) cap froze the query's LIMIT
(`limitFrozen` in `planner/sql.go`).

A frozen LIMIT alone isn't sufficient to warn on — plenty of crons
cap intentionally as a "give me the last N samples" query, where
truncation is the *point*, not a bug:

```
tail -n 5 /log/weft/coordinator/runtime.log | jq -r .fds_open | sort -rn | head -1
```

The distinguishing signal is whether the pipeline **aggregates across
the capped window** (every row within the window should count) versus
**samples the most recent few** (only the tail of the window matters
by design). Concretely:

- **Warn** when `Limit > 0` and the pipeline contains a
  window-spanning aggregate stage: `wc -l`, `grep -c`, `uniq -c`, a
  `/bin/` script known to wrap one of these (start with
  `log-pattern-count`; the list is a lint-owned allowlist, not
  pipeline-introspected — see Open questions), or a `jq` reduce/length
  expression.
- **Don't warn** when the pipeline's last stage is a recency-select —
  `head -1`/`head -N` or `sort | head` immediately after the capped
  source, no counting stage in between.
- **Don't warn** when `Limit == 0` (no cap — `cat`, or a fully
  pushed-down filtered query with no head/tail).

Bias the ambiguous cases toward *not* warning. A missed truncation
risk costs nothing new (today warns on none of them); a noisy false
positive on a legitimate sampling cron teaches people to ignore the
warning, which costs the whole feature.

Warning text names the offending cap and stage concretely, e.g.:

> `tail -n 1000` caps this cron at 1000 matching rows per run (every
> 5m — see schedule). If `stream error` occurs more than 1000 times
> in a 5-minute window, the excess is dropped before `grep -c` counts
> it. Consider raising `-n` or narrowing the match earlier.

## Generic warning mechanism

This is the actual prerequisite piece of infrastructure — the
truncation lint is just its first caller. Today `SaveCronWatch`'s
contract is a plain `error`: nil means clean success, non-nil means
reject. There's no way to say "succeeded, but here's something to
tell the user." Rather than bolt on a second, check-specific return
value, extend what `error` can mean, so every surface keeps a single
generic "got an error, render it" path and the *only* new concept is
whether that error is blocking.

```go
// store/warning.go — no new package; store is already the common
// import for every save/validate call site (mounts, api, mcp).

// Warning is a non-blocking, user-facing message. A function that
// returns one (possibly joined with others via errors.Join) means
// the operation succeeded — the caller should still show the message,
// but must not treat it as failure.
type Warning struct{ msg string }

func NewWarning(format string, args ...any) *Warning {
    return &Warning{msg: fmt.Sprintf(format, args...)}
}
func (w *Warning) Error() string { return w.msg }

// Split walks err (a plain error, a *Warning, or an errors.Join tree
// mixing both) into blocking errors and warning messages. Callers
// reject the operation iff len(blocking) > 0, and display warnings
// unconditionally (empty when there are none).
func Split(err error) (blocking []error, warnings []string) {
    if err == nil {
        return nil, nil
    }
    if joined, ok := err.(interface{ Unwrap() []error }); ok {
        for _, e := range joined.Unwrap() {
            b, w := Split(e)
            blocking = append(blocking, b...)
            warnings = append(warnings, w...)
        }
        return blocking, warnings
    }
    var warn *Warning
    if errors.As(err, &warn) {
        return nil, []string{warn.Error()}
    }
    return []error{err}, nil
}
```

`SaveCronWatch` changes not at all in signature — still `(...) error`
— only in what it can now return:

```go
func (s *Store) SaveCronWatch(...) error {
    if err := ValidateWatchPipeline(pipeline); err != nil {
        return err // hard error, unchanged behavior
    }
    if err := s.db /* ... existing upsert ... */; err != nil {
        return err
    }
    if msgs := planner.LintTruncationRisk(pipeline); len(msgs) > 0 {
        warnings := make([]error, len(msgs))
        for i, m := range msgs {
            warnings[i] = NewWarning(m)
        }
        return errors.Join(warnings...) // save already happened; this only carries messages
    }
    return nil
}
```

Every caller does the same two-line dance, regardless of what
produced the error:

```go
err := store.SaveCronWatch(...)
blocking, warnings := store.Split(err)
if len(blocking) > 0 {
    // reject — show errors.Join(blocking...).Error()
}
// show `warnings` (may be empty) — save succeeded either way
```

This is also a real, standing bug fix, not just plumbing for the new
check: **`api/handlers.go`'s `handleSaveWatch` currently discards
`SaveCronWatch`'s error entirely**, replacing it with a bare
`http.Error(w, "internal error", http.StatusInternalServerError)` —
so today, a bash syntax error typed into a cron pipeline via the web
UI or MCP already returns a useless generic 500 instead of the real
"bash syntax error: ..." message `CompileWatchPipeline` produced.
Adopting `Split` here fixes that for free: blocking errors get their
real text and a `400`, not a mystery `500`.

### Surface 1 — CLI shell (`mounts/cron.go` → `shell/vim_popup.go`)

`CronMount.WriteFile` keeps returning a plain `error` (it already
propagates `SaveCronWatch`'s error up to vim's `OnSave` today — that
part isn't broken). `installVimPopupHooks`'s `OnSave` becomes the one
place that calls `store.Split`:

```go
OnSave: func(path, content string) error {
    fs := core.GetFS()
    var saveErr error
    if fs != nil {
        saveErr = fs.WriteFile(path, []byte(content))
    }
    blocking, warnings := store.Split(saveErr)
    if len(blocking) > 0 {
        return errors.Join(blocking...) // unchanged: vim blocks, shows StatusMsg, skips popup
    }
    core.Dispatch(shellcore.ActionVimSave{Path: path, Content: content})
    if isVersionedPath(path) {
        queuedPath = path
        queuedWarnings = warnings // new closure var, may be empty
    }
    return nil
},
```

`openMessagePopup` prepends any queued warnings as `# WARNING: ...`
comment lines ahead of the existing commit-message header. This is
generic rendering — the popup doesn't know or care whether the
warning came from the truncation lint or anything added later.

Known limitation, not fixed here: warp's vim engine
(`pkg/vim/editor.go`) always overwrites `StatusMsg` to `"written"`
after a successful plain `:w` (no quit), so a warning set on that
path would be clobbered before it's ever seen. Every cron/bin save
already goes through `isVersionedPath` → the post-save popup on
`:wq`, which is the documented way to save one, so this only misses
someone who does `:w` and keeps editing without quitting. Fixing it
properly means changing warp's generic "don't overwrite StatusMsg the
hook already set" behavior — reasonable, but warp is shared with
vimgym (see `project_warp_is_shared` memory), so that's a deliberate,
separate change to propose there, not something to reach into
opportunistically from here.

### Surface 2 — HTTP API (`api/handlers.go`)

```go
err := s.store.SaveCronWatch(...)
blocking, warnings := store.Split(err)
if len(blocking) > 0 {
    http.Error(w, errors.Join(blocking...).Error(), http.StatusBadRequest)
    return
}
json.NewEncoder(w).Encode(struct {
    Warnings []string `json:"warnings,omitempty"`
}{Warnings: warnings})
```

Required follow-up, not optional: the frontend (`frontend/src/lib/
api.ts`'s `saveWatch`) discards the response body today and needs a
small change to read `warnings` and show them (toast/banner), and to
surface the now-real error body instead of whatever generic message
it shows on non-200 today. Otherwise this surface silently has no
lint despite the backend change.

### Surface 3 — MCP (`cmd/weft/mcp/tools_propose.go`,
`cmd/weft/mcp/tools_apply.go`, `shell/backend_http.go`)

`HTTPBackend.SaveWatch` already round-trips through the same
`PUT /api/watches/{name}` as surface 2 — it just needs to parse the
(now-meaningful) response body on both the success and error paths
and hand it back as plain text, generically, to `proposals_apply`'s
result. No MCP-side knowledge of "truncation" needed; it's just
relaying whatever `warnings`/error text the HTTP layer produced.

`weft.cron_propose` is the one place that isn't downstream of
`SaveCronWatch` — it validates before staging, not saving. Call
`planner.LintTruncationRisk` (or, more generally, whatever produces
`Warning`s at save time) directly there too, so an agent reviewing
`weft.proposals_diff` sees the same warning before anything is
applied, in the same generic shape it'll see again at apply time.

## Where the detector itself lives

Pure function, no `store.Store`/DB dependency:

```go
// planner/lint.go
func LintTruncationRisk(lp *LoweredPlan) []string
```

Takes a `*planner.LoweredPlan`, not a raw pipeline string — a
correction from the first draft of this doc, made during
implementation. `planner.Plan()` alone can't see through a `/bin/`
script invocation (`log-pattern-count coordinator/main 5000 '...'`
isn't a recognized source command, so bare `Plan()` returns
`ErrNotPlannable` and the lint would find nothing on exactly the
crons that motivated it). `Lower()` already does the bin-expansion
`SaveCronWatch`'s callers were computing anyway (via
`LowerForStorage`) before calling `Plan()` — so the lint reuses that
same expansion for free instead of needing its own resolver or an
allowlist of known wrapper names. `store.SaveCronWatch` unmarshals
the `loweredPlan []byte` it's already storing and calls
`LintTruncationRisk` on the result; if lowering failed
(`loweredPlan` is nil — an unsupported pipeline shape), the lint
silently no-ops rather than blocking a save that already succeeded.
`store` imports `planner` to do this (confirmed no import cycle:
`planner` does not import `store`).

## Test plan

- `planner/lint_test.go`: table-driven positive cases (`log-pattern-
  count`-style, raw `tail -n N | grep -c` shapes) and negative cases
  (`tail -n 5 | jq ... | sort -rn | head -1`, uncapped `cat`
  pipelines).
- Regression fixture: run every pipeline currently in prod's
  `weft_crons_list` through the linter and pin the expected
  positive/negative set (`stream-errors`, `christen-failures`,
  `christen-failure-rate`, `compute-exit-127` expected positive;
  `coord-fds-high`, `coord-goroutine-high`, `coordinator-restart`
  expected negative) — this both documents intent and catches
  accidental over/under-firing on real schedules as the heuristic
  evolves.
- `store/warning_test.go`: `Split` on nil, plain error, `*Warning`,
  and `errors.Join` mixes of both.
- `mounts/cron_test.go` / `api/handlers_test.go`: assert a truncation-
  risk save still succeeds (`len(blocking) == 0`) and `warnings` is
  non-empty; assert an existing bash-syntax-error save still rejects
  (`len(blocking) > 0`) with its real message, not a generic one.

## Open questions

- Largely resolved by the generic mechanism, but worth checking at
  implementation time: does `cmd/weft/mcp/policy/` want its own
  `Warning`-producing checks eventually (e.g. a policy rule that's
  advisory rather than allow/deny)? If so it can produce the same
  `store.Warning`/plain-message shape without `policy` importing
  `store` — keep the type in `store` and have `policy` return plain
  strings the caller wraps, same as `planner.LintTruncationRisk`
  does.

## Implementation status (2026-09-19)

Shipped:

- `store.Warning` / `store.Split` (`store/warning.go`) — the generic
  mechanism, with tests.
- `planner.LintTruncationRisk(*LoweredPlan)` (`planner/lint.go`) —
  the detector, with a table test covering the log-pattern-count and
  distinct-count-via-`uniq -c` positive shapes and the recency-select
  negative shapes.
- `store.SaveCronWatch` calls it after a successful save and returns
  the result via the generic `error`/`Warning` channel — no
  signature change.
- All four production callers of `SaveCronWatch` fixed to use
  `store.Split` instead of treating any non-nil error as failure:
  - `shell/vim_popup.go` (CLI shell `/cron.d` vim save) — warnings
    fold into the existing post-save commit-message popup header;
    blocking errors behave exactly as before.
  - `api/handlers.go`'s `handleSaveWatch` (`PUT /api/watches/{name}`)
    — also fixed the standing bug where any error (including a real
    bash-syntax rejection) was discarded and replaced with a generic
    "internal error" 500. Blocking errors now get their real message
    and a 400; a clean/warned save gets a `{"warnings": [...]}` body.
  - `shell/backend_http.go`'s `HTTPBackend.SaveWatch` — relays the
    HTTP response's `warnings` field back as a `store.Warning`,
    which is how `weft.proposals_apply` (MCP) inherits this for free
    — it goes through the same HTTP endpoint as the web UI.
  - `cmd/weft/mcp/tools_apply.go`'s `proposalsApplyHandler` — a
    warning-only `applyOne` result no longer aborts the batch; the
    message is folded into the result text keyed by proposal id.
  - Found during implementation, not one of the three originally
    discussed surfaces, but a real regression risk from the same
    change: `coordinator/service/server.go`'s gRPC `SaveWatch` RPC
    and `api/fs.go`'s generic `PUT /api/fs/write` (any-path file
    editor) both call `SaveCronWatch` too. Both fixed to not treat a
    `Warning` as a rejection; neither has anywhere to display warning
    text today (empty proto response; 204 has no body), so both drop
    the message rather than inventing a display surface for paths
    outside this doc's scope.
- `frontend/src/lib/api.ts`'s `saveWatch` return type corrected to
  carry `warnings?: string[]` (no UI wired up — nothing in
  `frontend/src` currently calls `saveWatch`, so there's no toast/
  banner to build yet; the error path needs no frontend change at
  all, since `apiFetch` already surfaces the real response body
  generically, which is what the `handleSaveWatch` fix above feeds
  it).

Deferred, explicitly not silently dropped:

- **`weft.cron_propose` early preview.** The plan called for linting
  at propose time too, before `proposals_apply` ever calls
  `SaveCronWatch`. `cron_propose` doesn't have access to a
  `BinResolver`/customer/tier (`cmd/weft/mcp`'s `Config` is a bag of
  plain function pointers with none of that), so previewing would
  need a new `Config` hook wired through `cmd/weft/mcp_cmd.go` —
  real but bounded extra work, skipped for this pass. Apply-time
  warnings (shipped) already cover "warn when the cron actually
  saves," which was the core ask; propose-time is a nice-to-have on
  top.
- The `cmd/weft/mcp/policy` open question above (unchanged).
