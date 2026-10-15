# Schema extraction wizard

Status: Phases 1-4 and fixtures landed (uncommitted) and exercised live (CLI in a pty, browser with a real mouse drag) on 2026-10-15. LLM-assisted retry built (see below).

## Goal

When a source is pending, the user can (a) force a resample, and (b) be walked
through *every* extractable metavar (timestamp, level, trace_id, span_name,
span_id, parent_span_id, duration_ns) with a decision tree, not just the
timestamp:

1. Show the best candidate, highlighted. Accept -> next field.
2. Reject -> "Do you expect this value on these lines?" No -> skip the field.
3. Yes -> "Is it on the line shown?" No -> next line (after N lines, offer resample).
4. Yes -> the user highlights it. Server generates an extraction expression.
5. Apply it to the rest of the sample with the real parser; show the results.
   The user flags lines where it failed or is wrong; regenerate; repeat.

## Delta found (before work)

- Resample: server only matched `status = configured`; weftd only resampled
  configured sources; weftd samples the **head** of the file (startup lines);
  direct/kinesis sources never refilled because `sample_collected` stayed >= target.
- Browser `SchemaEditor`: only the timestamp is highlighted; level/trace/span are
  bare inputs under "advanced". CLI `setup.go`: one `huh` select per field.
- `validateTagField` drops any candidate hitting < 50% of eligible lines, so a
  trace id on a minority of lines never reaches either UI.
- Classification runs once; nothing re-runs it with a user hint.

## Phases

1. **Resample while pending** (done): `Store.RequestResample`, API 200/404/409,
   `GetSourceStatus` reports `ResampleRequested` for sampling/pending, weftd
   resamples fresh (tail, not head) while waiting for configuration, kinesis
   configured resample accumulates, browser "collect a new sample" button, CLI
   `weft source resample` works on pending sources.
2. **Probe endpoint** `POST /api/sources/{ns}/{app}/probe`: highlight -> regex
   (single capture group, left-context anchored), run via `ingest.NewParser` over
   the sample, return per-line value/miss; with flagged lines, retry (deterministic
   widening first, LLM with positive/negative examples if that fails).
3. **Decision tree** shared by both UIs. Browser: per-field highlighter + stepper.
   CLI: a bubbletea line/column **cursor picker with vim keybindings**
   (h/j/k/l, w/b/e, 0/$, v to select, enter to accept, ? to skip) inside the
   wizard (bubbletea is fine in the wizard, not in the shell).
4. **Detection**: drop the 50% bar for tag fields and show the hit rate; send the
   classifier lines of varied shapes, not just the first 50.

## Fixtures

`dev/demo/schema_fixtures.py` pushes startup-chatter-then-traffic logs for four
services (`fx-tagged`, `fx-bracketed`, `fx-jsonl`, `fx-celery`) through the Heroku
drain endpoint. `startup` fills the 50-line sample with lines that have no trace
ids; `traffic` is what a resample then collects.

## Ship notes

- weftd changed (`SampleLines`/`WaitForConfiguration`): needs a version bump and a
  release for the fleet to auto-upgrade (see weftd release memory notes).
- No `schema.sql` change; migration 094 adds `log_groups.sample_updated_at`.

## What landed for Phases 2-3

- `ingest/probe.go`: `Probe` takes the user's marks (value highlighted on a line, or "none
  on this line"), searches candidate extractors simplest-first (JSON path, whole-line
  pattern for tag fields, positional field, positional field + pattern; level has no
  whole-line mode), and checks each with the real `Parser`. Flagged lines become hard
  constraints; lines the previous rule already got right are preserved where possible.
  Retry is deterministic widening/narrowing; LLM-assisted retry is NOT built.
- `POST /api/sources/{ns}/{app}/probe` (stateless, reads the stored sample).
- CLI: `cmd/weft/setup` -- `ExtractField`/`ExtractAll` (pure, UI-agnostic tree), a bubbletea
  vim cursor picker (`picker.go`) and review list (`review.go`), `terminalUI` (tui.go).
  The CLI runs `ingest.Probe` in-process; the browser calls the endpoint.
- Browser: `ExtractionWizard.tsx` (same tree), used by `SchemaEditor` when given a `probe`.
  Selection is by mouse/double-click, not vim keys.
- Also fixed: the browser never sent `timestamp_fields`, `json_start_field` or `envelope`
  to configure, so two-column timestamps, prefix_json and kevcons-envelope sources
  were wrong or rejected.
- Not yet: the timestamp *format* cannot be taught by highlighting an unknown layout (it
  only recognizes the known layouts); no detection tweaks (Phase 4); not exercised against a
  live stack.

## Phase 4 and the live pass (2026-10-15)

- `validateTagField` no longer applies the 50% coverage bar to trace/span fields (>= 2 hits
  only); `level` keeps it. The classifier prompt says these fields may be on only some lines.
  (The "send more varied lines" idea was moot: the sample is already just 50 lines.)
- Live pass found and fixed: a second resample while one is collecting reset progress
  (direct sources are `configured` in the DB, so the guard now also checks the flag);
  a resampled direct source vanished from the pending list at 0 lines (now listed as
  sampling; `sample_updated_at` is stamped at request time to keep it listed).
- Live pass used a second API on :8081 + vite on :5175 against the dev DB; the running
  dev coordinator container is old and creates direct sources WITHOUT the resample flag, so
  the first sample was kicked off with a manual `update log_groups set resample_requested_at`.
  Rebuild the coordinator for that to happen on its own.
- Throwaway data left in the dev DB: namespace `schema-fixtures` (acme org) with services
  fx-tagged (configured via CLI), fx-bracketed (configured via browser), fx-celery, fx-jsonl.

## One checker, many proposers (2026-10-15)

Detection and retry used to be two engines: `validateTagField`/`validateTimestampField`
checked the classifier's proposals with their own matching code, while the probe checked
user-built rules with the real `Parser`. They had already drifted (the timestamp verifier trimmed
differently from the parser; nothing checked that a proposed duration was an integer; a
JSON-path proposal was verified with a pattern the parser never applies).

Now there is one checker, `evalExtractor` (builds the schema the extractor would become and
runs the real `Parser`), and everything else is a *proposer* of candidate extractors:

- heuristics and the classifier at sample time -> `verifyTagProposal`/`verifyTimestampProposal`
  -> `evalExtractor` (strict about level patterns, so a wrong pattern cannot "verify" through
  the parser's whole-field fallback)
- the user's highlight and flagged lines -> `Probe`'s built-in search -> `evalExtractor`
- retry, when the built-in search finds nothing -> `ExtractorProposer` (Bedrock) -> the same
  `Probe` constraints (every mark satisfied). The model can widen what the probe can solve,
  never what it accepts. `ProbeResult.Source` says "rule" or "model" and both UIs say so.

The CLI wizard now calls the server's probe endpoint (it used to run `ingest.Probe` in-process),
so it gets the same engine and model help as the browser. `LocalProber` remains for tests.

Not unified: `detect.go`'s heuristic timestamp/level detectors still discover candidates with
their own scanning. They are proposers too, but their output is not re-run through
`evalExtractor` before being offered (the wizard shows it through `Probe` anyway).

## Live Bedrock findings (2026-10-15, Nova Lite, us-east-2)

- **Decode bug (pre-existing, fixed):** `UnmarshalSmithyDocument` ignores `json` struct tags, so
  every multi-word key (`field_index`, `extract_pattern`, `json_path`, `sample_value`,
  `span_fields`) decoded as zero. Every LLM proposal therefore arrived with nothing to locate
  it by and was dropped; only the heuristic `Detect` candidates (timestamp, level) ever reached
  the wizard, and trace/span candidates never did. Now decoded through JSON
  (`decodeToolInput`). Whether prod's classifier is enabled is in
  infrastructure-catalog/stacks/asg-weft-application (BEDROCK_CLASSIFIER_MODEL_ID).
- Tool schemas gained property descriptions and required fields, and the classify prompt now
  spells out field numbering for the first three lines (models miscount whitespace fields:
  Nova Lite said field 10 for field 9 and a 1-wide timestamp for a 2-wide one before this).
- Opt-in live tests: `BEDROCK_LIVE_MODEL_ID=us.amazon.nova-lite-v1:0 BEDROCK_LIVE_REGION=us-east-2
  go test -tags test ./ingest -run Live -v` (classify a minority trace id; retry for two
  spellings; raw tool input). Claude Haiku is not enabled on the kevlar account (needs the
  Anthropic use-case form).

## Extract specs and span-reporting extraction (2026-10-15)

Positional values used to be read with trims hardcoded in the parser (strip `[`, `]`, `:`), and the
probe re-derived highlights with a second copy of that logic. Now:

- `Schema.Extract` maps a value name to an `ExtractSpec` {prefix, suffix, chars, cut_at, fallback}.
  A value with an entry is read by `ingest/extract.go` (negative `*_field` counts from the end);
  a value without one is read by the untouched legacy extractors, so stored schemas behave exactly
  as before.
- The extractor returns *where* it matched. `Parser.ParseLineSpans` is the same code path as
  `ParseLine` plus the spans, so a highlight cannot disagree with what was extracted. The probe's
  `locate` (and its bracket/colon hack) is gone; JSON paths and legacy-form values still highlight
  by finding their text in the line.
- The probe derives a positional value's prefix/suffix from the highlight itself (what surrounds it
  inside its field), tries the index from the front and from the end, and prefers `cut_at` over a
  literal suffix when the suffix is content starting at a delimiter (`/MainProcess]` after `INFO`).
  Proposers without a highlight (heuristic detection, the model) write explicit default `chars`
  (`[]():`, `[]:` for timestamps) into the spec, so the default lives in the schema, not the parser.
- Plumbing: configure endpoint, CLI config map, browser form (`applyPatch`/`mergePatches`), and the
  probe response (`description`, one source of wording for the CLI; the browser mirrors it in TS
  with tests sharing the sentences).
- Benchmarks (real parser, 2 values): short line 485 -> 304 ns, long line 883 -> 317 ns with the
  early split (stop splitting at the highest front index); a negative index forces the full split
  (about the same as legacy on a 60-field line; lines over 32 fields outgrow the stack buffer).
  `TestWeftdParseBudget` caught the first version heap-allocating the offsets buffer.

Known gaps: old weftd agents ignore `extract` and negative indexes (they would read those values
as empty), so a weftd release should ship before wizard-built schemas are relied on for weftd
sources; the level heuristic does not recognize `INFO/ForkPoolWorker-1` (hyphen) so Celery levels
go through the tree; `timestamp_pattern` (a regex timestamp) is still not offered.

## No hardcoded knowledge in detection (2026-10-15)

Rule: nothing in detection, extraction, the wizards or the prompts may name a tool, framework,
provider or format, or carry a vocabulary. Where removing something left a gap, the algorithm grew.

- **Envelope**: detection and wizards know no envelopes (a "[time] [id]" prefix is just fields).
  The one reader left, for stored schemas that name one, is quarantined in `ingest/legacy.go`.
- **Timestamps**: `inferLayouts` (ingest/layout.go) deduces a Go layout from the texts: tokenize into
  digit/letter/punctuation runs, require one shape, assign calendar/clock roles by position and
  numeric range, keep only layouts `time.Parse` accepts for every line and that name a full date;
  day/month order is ranked by chronology. No table of formats. The punctuation around the text is
  the *frame* (common non-alphanumeric prefix/suffix) and goes into the spec, not the layout.
- **Levels**: no list of level words. `detectCategorical` offers every field whose leading word comes
  from 2-8 recurring values (+ optional delimiter), left to right; the model or the person says which
  one is the severity. Both wizards offer each candidate in turn (`detected_index`).
- **Trims**: proposals derive their frame from the lines (`derivedFrame`); no default character sets.
- **Prompts**: describe what each value is, not conventions or tools. The regex rule (one capturing
  group, others `(?:...)`) is stated; a proposal with several groups is normalized by the user's
  marks (`normalizeProposal`).
- **Browser**: `suggestFromDetection` prefills format only; per-value reading comes from the server.
- **Spec semantics**: `Prefix`/`Suffix` are *required* (a field lacking them has no value), so a value
  on only some lines is not read from other lines' fields. That makes position + framing sufficient
  for sparse values, and the probe now tries structure first (JSON path, then field position) and a
  regex only as the last resort. In the four fixture shapes (sparse tag, bracketed id, nested JSON,
  id inside a field) none needs a regex.

Where structure is *not* enough (and a regex or the model is used): the same value spelled two ways
on different lines; a value at no consistent position with no shared framing; two lines structure
cannot tell apart that a flag separates. A value inside a field behind a *varying* prefix (a task
name before "[") reads structurally only with a literal prefix today; an `after` operation (keep the
text after the last delimiter) would generalize it and is the next structural step.

Remaining hardcoding, all in `ingest/legacy.go`: the legacy envelope name and the legacy trims
(`[]():`, and the timestamp's `[` `]` `:`). Deleting it needs stored schemas migrated (and
weftd released); the envelope also stamped `instance`, which an explicit schema would have to
read as a value.
