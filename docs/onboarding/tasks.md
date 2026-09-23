# Onboarding Tasks — a graded ladder

A sequence of starter features for new engineers, ordered easy →
hard. Each task is **a complete, shippable feature carved out of a
larger design plan** — you ship something real and end-to-end, but
you are *not* on the hook for the whole plan. The plan each task
comes from is linked; read it, then build only the slice described
here. The rest of the plan is explicitly out of scope (and noted, so
you know where the edges are).

These complement the embedded "Challenge" sections in the topic docs
([topology](topology.md), [warp](warp.md), [features](features.md),
[docgen](docgen.md)). The webhook-notify challenge in
[features.md](features.md) is a good warm-up before Task 1.

## How to use this ladder

- Work top-to-bottom. Difficulty and breadth grow as you go.
- Each task lists the **plan it's carved from**, what's **in scope**
  (your deliverable), what's **deferred** (the rest of the plan),
  the **files to start from**, the **steps**, and **what you'll
  learn**.
- File paths are starting points, not gospel — the codebase moves.
  Grep to confirm a symbol still lives where the task says before
  you lean on it.
- Every task ends the same way: tests pass, `go build ./...` is
  clean, docs regenerated if you touched `docs/shell` or
  `docs/manual` (`go run ./cmd/docgen`), frontend compiles if you
  touched it (`cd frontend && npm run build`). Open a PR — the diff
  is the proof you understood the subsystem.

## The ladder at a glance

| # | Task | Tier | Plan | Subsystems |
|---|------|------|------|------------|
| 1 | Ban `/bin` calls in compute stages | 🟢 Easy | [nested-query-compute-pool](../plans/nested-query-compute-pool.md) | store, query rewrite |
| 2 | First AWS source preset (VPC Flow Logs) | 🟢 Easy | [aws-source-presets](../plans/aws-source-presets.md) | ingest schema, CLI |
| 3 | Self-healing bucket warming | 🟢 Easy | [bucket-warming-remint](../plans/bucket-warming-remint.md) | shard/bucket FSM |
| 4 | Ad-hoc alert silencing | 🟡 Medium | [alert-silencing](../plans/alert-silencing.md) | store, notifier, shell |
| 5 | Agent-side multiline stitching | 🟡 Medium | [weftd-multiline](../plans/weftd-multiline.md) | weftd tailer |
| 6 | Query performance hints | 🟡 Medium | [perf-hints](../plans/perf-hints.md) | planner, coordinator, CLI |
| 7 | Generic JSON ingest endpoint | 🔴 Hard | [http-ingest](../plans/http-ingest.md) | HTTP ingest, auth, routing |
| 8 | Ingest-time drop rule | 🔴 Hard | [ingest-governance](../plans/compliance/ingest-governance.md) | ingest drain, store, metering |

---

## Task 1 — Ban `/bin` calls in compute stages 🟢

**Carved from:** [nested-query-compute-pool.md](../plans/nested-query-compute-pool.md)
— **option (A)** only.

A `/bin` script that calls *another* `/bin` script from a compute
stage (any stage after the source-shaped leading command) fails at
runtime with an opaque `bash: lookup: command not found` from a
compute worker. The full plan debates how to *support* this; your
job is the short-term UX win: catch it at save time with a clear,
actionable error instead of a buried runtime failure.

**In scope (your feature):**
- Detect, at save time, any saved `/bin` or `/cron.d` script whose
  compute-stage position references a known `/bin` script name for
  that customer.
- Reject the save with the message from the plan: name the offending
  stage, explain `/bin` scripts can't run from compute stages, and
  suggest the "make it the terminal stage" workaround.
- A runtime backstop with the same message for the stale case (a
  script that became offending because a later-saved `/bin` script
  now matches one of its words).

**Deferred (not your problem):** actually *supporting* nested calls —
options (B)/(D) in the plan. (For the record: remote compute *is* the
production topology — `cmd/coordinator` fatals without a `-compute`
addr — so the eventual real fix is option (D), a narrow coord-callback
RPC, not (B). You don't need any of this for Task 1.)

**Start from:**
- `coordinator/query_rewrite.go` — the rewrite walker already
  identifies query-shape pipelines; the detector extends it to
  inspect `Pipeline.Commands[1:]` (the compute stages).
- `store/shell.go` — `validateSource` is where save-time validation
  lives. It's currently a pure function of the source; you'll need
  the customer's `/bin` name set, so thread a predicate or a Store
  method in.

**Steps:**
1. Write a failing test first (house rule): save `/bin/lookup`, then
   save a second script `cat /log/x.log | grep ERROR | lookup "$f"`
   and assert the second save fails with a message naming `lookup`.
2. Add the AST walk that lists compute-stage leading words and
   matches them against the customer's `/bin` names.
3. Wire it into the save path; make the test pass.
4. Add the runtime backstop check and a test for the stale case.

**What you'll learn:** how warp pipelines are parsed into an AST, how
the query rewriter classifies source-shape vs. compute stages, and
where save-time validation hooks into the store.

---

## Task 2 — First AWS source preset: VPC Flow Logs 🟢

**Carved from:** [aws-source-presets.md](../plans/aws-source-presets.md)
— the **loader + the first preset**.

Onboarding an AWS log source today means hand-writing the schema. The
plan ships ~10 canned presets (VPC Flow, CloudTrail, ALB, …). You
build the preset machinery and the *first* preset end to end, which
turns every subsequent preset into a ~half-day drop-in.

**In scope (your feature):**
- The preset loader + catalog API (the spine).
- One working preset: **VPC Flow Logs** (well-documented, fixed
  field order — the easiest first).
- The CLI flag / argument that applies a preset when creating a
  source, plus the catalog listing.
- A regression test that loads the preset and asserts a sample VPC
  Flow line parses into the expected dimensions.
- Docs section for the VPC Flow preset.

**Deferred:** the other ~9 presets (CloudTrail, ALB, S3, WAF, Lambda,
RDS, API Gateway, …), the web dropdown, and the auto-hint banner.
Leave hooks where they slot in.

**Start from:**
- The ingest schema / dimension model (see
  [features.md](features.md) → Meta-variables, and the shipped
  `dimensions-from-files` / `ingest-dimensions` docs in
  `docs/completed/` for how dimensions are declared).
- `api/sources.go` — source creation / schema bootstrap is the
  natural place to accept a `--preset` selection.

**Steps:**
1. Decide the preset file format (YAML mapping log fields →
   dimensions) and where presets live in the repo.
2. Build the loader + a catalog accessor.
3. Write the VPC Flow preset YAML.
4. Add `--preset vpc-flow-logs` (or equivalent) to source creation.
5. Test: a real VPC Flow sample line → expected parsed dimensions.
6. Document it.

**What you'll learn:** the ingest schema/dimension model end to end,
and how a source goes from raw bytes to queryable meta-variables.

---

## Task 3 — Self-healing bucket warming 🟢

**Carved from:** [bucket-warming-remint.md](../plans/bucket-warming-remint.md)
— the whole plan (it's small), your first FSM bug fix.

A bucket's `evaluateWarming` only gates on the "all shards ready"
condition. If the warming cohort *shrinks* below target because a
shard was lost mid-warm, nothing re-mints the missing shard — the
bucket can get stuck. The fix re-mints to refill the cohort.

**In scope (your feature):**
- Add the re-mint branch to `evaluateWarming` so a cohort below
  target gets topped back up (factor out the existing
  `onEnterWarming` mint logic so both paths share it).
- A test that drops a warming shard and asserts a replacement is
  minted.
- Update the break-glass note in `cleanup.md` to reflect that this
  is now self-healing (or remove the manual step).

**Deferred:** nothing — this is a complete, self-contained fix.

**Start from:**
- The bucket FSM (`coordinator/` — find `evaluateWarming` and
  `onEnterWarming`). Read [topology.md](topology.md) and
  `docs/shard-lifecycle.md` first; the FSM diagrams in
  `docs/diagrams/` (`bucket-fsm.png`, `shard-fsm.png`) are worth a
  look.

**Steps:**
1. Reproduce in a test: build a bucket in WARMING, kill one shard
   below target, run an evaluate tick, assert no re-mint (red).
2. Add the re-mint branch; make it green.
3. Confirm `Σ` capacity invariants still hold (don't inflate the
   cohort past target — see the bucket-split cautionary tale).
4. Fix the doc.

**What you'll learn:** how the shard/bucket FSMs converge via
`Evaluate` (there is no separate reconciler — each entity drives its
own convergence), and the capacity-invariance discipline that the
whole fleet depends on.

---

## Task 4 — Ad-hoc alert silencing 🟡

**Carved from:** [alert-silencing.md](../plans/alert-silencing.md)
— **one-shot silences** only.

Customers need to mute alerts during known-noisy windows (a deploy, a
maintenance). The plan covers one-shot *and* recurring cron-scheduled
silences plus a web UI. You build the one-shot path end to end.

**In scope (your feature):**
- The `AlertSilence` model + an "active at now?" resolver (handle the
  one-shot `from`/`until` window; leave the cron-recurrence branch
  stubbed).
- The notifier hook: before dispatch, skip alerts matching an active
  silence (with the small cache the plan describes).
- A CLI shortcut to create an ad-hoc silence ("silence `error-count`
  for 2h").
- Tests: an alert inside a silence window doesn't dispatch; outside,
  it does.

**Deferred:** recurring/cron-scheduled silences, the YAML editor
surface, the web UI (list/extend/cancel), and audit-log entries.

**Start from:**
- `coordinator/notify.go` — the dispatch fan-out (where the skip
  hook goes).
- `coordinator/alert.go` — condition evaluation, so you know exactly
  when dispatch happens.
- `store/` — pattern your model on the existing `NotifyAlias` /
  watch models.

**Steps:**
1. Failing test: alert that should fire is suppressed by an active
   one-shot silence.
2. Model + store CRUD + the active-at-now resolver.
3. Notifier hook + cache; make the test pass.
4. CLI shortcut + its test.

**What you'll learn:** the full alert path (pipeline → condition →
dispatch), and how to insert a cross-cutting suppression gate without
disturbing the fan-out.

---

## Task 5 — Agent-side multiline stitching 🟡

**Carved from:** [weftd-multiline.md](../plans/weftd-multiline.md)
— the **`start_regex` mode** only.

Stack traces and panic dumps arrive as many physical lines but are
one logical event. weftd should stitch them at the agent before they
hit the wire. The plan supports three modes (`start_regex`,
`continuation_regex`, `indent`) plus journal wiring; you build the
most common one.

**In scope (your feature):**
- A `multilineBuffer` in the tailer that, in `start_regex` mode,
  accumulates lines until the next line matches the start pattern (or
  a timeout / size cap fires), then flushes the joined event.
- Config parsing for `start_regex` (compile the regex at load;
  reject bad patterns clearly).
- Guardrails: max buffered lines / bytes and a flush timeout so a
  never-matching stream can't buffer forever.
- One fixture test feeding a multi-line stack trace and asserting one
  stitched event out.

**Deferred:** `continuation_regex` and `indent` modes, journal-tailer
wiring, schema-driven per-format defaults, and the dropped-line
`ingestion_errors` reporting. Structure the buffer so the other two
modes drop in later.

**Start from:**
- `weftd/`'s `tailer.go` (the file-tailing loop) and `SourceConfig`
  (where `Multiline` config attaches). Read
  [ingestion-weftd.md](../ingestion-weftd.md) for the agent's wire
  path.
- Build real binaries, not temp ones:
  `go build -o weftd/weftd ./cmd/weftd/`.

**Steps:**
1. Failing fixture test: a 6-line Go panic → one event.
2. Implement the `start_regex` buffer with flush-on-match.
3. Add the timeout + size guardrails (and tests for each).
4. Wire config parsing.

**What you'll learn:** the weftd tail loop, how the agent frames
events before shipping, and timeout/size-bound buffering patterns.

---

## Task 6 — Query performance hints 🟡

**Carved from:** [perf-hints.md](../plans/perf-hints.md) — the
**rule engine + two rules**.

When a query runs slowly, weft can suggest a faster rewrite (advisory
only — never auto-applied). The plan defines five rule types; you
build the engine and two of them, surfaced to the CLI.

**In scope (your feature):**
- A `hints.Suggest(plan, stats)` engine + a rules registry.
- Two rules: **predicate hoist** (push a filter earlier so more work
  pushes down to SQL) and **source narrow** (suggest a tighter
  `/log/...` path or time range). Pick the two whose signals are
  easiest to read from the existing plan/stats.
- Coordinator wiring: a timer + row counter that threads the
  resulting hints into the response payload.
- CLI rendering of hints as dimmed text below the result.
- A unit test per rule asserting it fires on a matching plan and
  stays silent otherwise.

**Deferred:** the other three rules (reorder, stage-merge,
better-shape), the optional `weft.suggest_query` MCP tool, and any
ranking/threshold tuning.

**Start from:**
- `planner/planner.go` — the `QueryPlan` shape your rules read.
- `coordinator/coordinator.go` — where execution timing and row
  counts are available to attach hints to the response.

**Steps:**
1. Define the hint type + engine signature; one failing rule test.
2. Implement the two rules behind the registry.
3. Thread hints through the response payload.
4. Render them dimmed in the CLI; add the payload test.

**What you'll learn:** how the planner represents a query, what
pushdown actually buys, and how response metadata flows back to the
shell.

---

## Task 7 — Generic JSON ingest endpoint 🔴

**Carved from:** [http-ingest.md](../plans/http-ingest.md) — the
**JSONAdapter** slice.

http-ingest is a pluggable HTTP ingestion pipeline (shared handler +
per-source adapters). The Heroku adapter has shipped; you add the
generic `POST /api/ingest` JSON adapter on top of the existing shared
handler.

**In scope (your feature):**
- `JSONAdapter`: accept both ndjson and a JSON array shape; support
  gzip/deflate request bodies; Bearer auth via the existing
  namespace-scoped `wft_...` token.
- Reuse the shared handler's token check, rate limiting, and
  bytes-in metering — don't reinvent them.
- Schema bootstrap on first batch (same approach as the weftd
  onboarding flow).
- Tests: ndjson body, JSON-array body, gzipped body, bad token →
  401, malformed JSON → clear 4xx.
- Docs + the ~50-line reference Python snippet from the plan.

**Deferred:** the CloudWatch adapter recast (that's the
[cloudwatch-kinesis-subscription](../plans/cloudwatch-kinesis-subscription.md)
plan's territory) and any non-JSON shapes.

**Start from:**
- The shared http-ingest handler and the `Adapter` interface (find
  `ingest_http.go` — the Heroku adapter is your template).
- `api/handlers.go` for the Bearer-auth pattern; `api/sources.go`
  (`~36-54`) for first-batch schema detection.

**Steps:**
1. Failing handler test: ndjson POST with a valid token → rows
   routed to a shard.
2. Implement the adapter against the shared handler interface.
3. Add gzip/deflate decode + the array shape; tests for each.
4. Wire schema bootstrap; add the auth/malformed-body tests.
5. Docs + Python sample.

**What you'll learn:** the HTTP ingest architecture, the
adapter/handler split, token auth, and the schema-bootstrap path —
the same shape every future ingest source reuses.

---

## Task 8 — Ingest-time drop rule 🔴 (capstone)

**Carved from:** [ingest-governance.md](../plans/compliance/ingest-governance.md)
— the **engine + the `drop` action**.

Ingest governance lets a customer apply ordered rules (drop / sample
/ redact) to rows *before* they hit a shard — for PII control and
cost. You build the rule spine and the first, simplest action; the
plan's later riders (sampling, redaction, and
[log-to-metric](../plans/log-to-metric.md)'s `count` action) all
hang off what you ship.

**In scope (your feature):**
- The `IngestRule` model + YAML parsing + save-time validation.
- A rule-eval engine with hot reload, evaluated in the drain
  goroutine — implement the **`drop`** action (rows matching a rule
  are discarded pre-shard).
- Metering: surface dropped-row counts (the columns / `/etc/usage`
  surface the plan describes) so a customer can see what governance
  removed.
- A drain-path benchmark proving the eval cost is acceptable on the
  hot path (Paul's rule: hot paths get the fast library / the
  measurement, not a hand-wave).
- Tests: a matching row is dropped and metered; a non-matching row
  passes through unchanged.

**Deferred:** the `sample` and `redact` actions, the web UI editor +
dry-run preview, the `count` action (log-to-metric), and audit-log
entries. Order the engine so actions are pluggable.

**Start from:**
- The ingest drain (where parsed rows are routed to shards — see
  [features.md](features.md) → Log querying for the ingest path, and
  `coordinator/coordinator.go`'s `StartDrain` for the existing route logic).
- `store/` for the rule model, patterned on existing per-customer
  config.

**Steps:**
1. Failing test: a customer with a `drop` rule ingests a matching
   line; assert it never lands on a shard and the drop is counted.
2. Model + YAML parse + validation.
3. Eval engine + the `drop` action in the drain; make it green.
4. Hot reload + its test.
5. Metering surface + benchmark.

**What you'll learn:** the ingest hot path under multi-tenancy, how
per-customer config reaches the drain, and the performance discipline
the ingest path demands. This is the broadest task here — it touches
ingest, store, metering, and the testing/benchmarking bar all at
once.

---

## Where to go next

Once you've cleared a few rungs, the [plans README](../plans/README.md)
has the full backlog with an ease-vs-value table and a
dependency-ordered roadmap. Good "graduation" plans — bigger than
these tasks but well-specified — include `shard-roll`,
`weftd-disk-spool`, and the rest of any plan you took a slice from
here.
