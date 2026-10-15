# Schema rollout: ship it, put every producer on a schema, delete the fallback

Status: plan, 2026-10-15. Phase 1's missing pieces are built (below); nothing here has been run against production.
Companion to `schema-extraction-wizard.md` (what was built); this is how it gets out.

## Goal and end state

1. The wizard, probe, extract specs and detection ship.
2. Every log producer reads its values through an `extract` spec (or a JSON path / explicit
   whole-line regex), never through a legacy extractor.
3. `ingest/legacy.go` and every legacy branch in `parse.go` are deleted, and the schema
   validator refuses the legacy form so it cannot come back.

## What is different about this rollout

- **Server-side parsing is instant; weftd parsing is not.** Push sources (Heroku, Render,
  CloudWatch/Kinesis) are parsed by the API (`api/adapter.go`), so they pick up the new parser
  the moment the API deploys. weftd parses *client-side* with the parser baked into its
  binary, so a new-form schema is only as good as the agent reading it.
- **An old agent fails silently.** It ignores `extract`, reads a negative index as nothing, and
  logs `schema install failed, falling back to no-parse`. Rows keep flowing, with no level, no
  trace id, and an ingest-time timestamp. Nothing alarms.
- **The server cannot see agent versions.** `GetSourceStatusRequest` carries only a token and a
  service. The fleet is already mixed: pinned in `.kevcons.yaml` today are csa, vimgym,
  vimgym-gitcore and olympics at v0.20.0, and grant_management, musclebones and sgokit at v0.23.0,
  against 0.27.0 in the tree. Auto-upgrade (v0.20.0+) moves living agents toward `latest`
  within ~a day or on restart, but a pinned `.deb` is what a rebuild installs.
- **Stored schemas are the other half.** Legacy-form schemas (positional field with no `extract`,
  a `*_pattern` refining a field, `envelope: kevcons`) live in `log_groups.schema_yaml`.

So the order is: make it visible, ship the server, upgrade the agents, *then* change schemas,
then forbid the old form, then delete.

## Phase 0: prepare (before any merge)

- [ ] **Split the working tree into reviewable commits.** It holds this feature plus unrelated
      in-flight work (syslog server, adapters, manual pages). Suggested cuts: (1) migration 094 +
      sampling/resample (store, api, coordinator, weftd sampler); (2) ingest: extract engine,
      layout inference, probe, detection; (3) API endpoints (probe, resample, configure `extract`);
      (4) CLI wizard; (5) browser wizard; (6) fixtures + docs. Run `make docs`; `make collapse`
      only after the deploy (see Phase 1).
- [ ] **Hand-test both UIs** against the local stack with the fixtures
      (`dev/demo/schema_fixtures.py`). The CLI picker and the browser wizard have not been driven by
      hand since the last changes; only API-level and pty/jsdom checks exist.
- [ ] **Take the baseline** (see "Equivalence check" below) for every currently configured source,
      so Phase 1 can prove it changed nothing.
- [ ] **Decide the three questions at the end of this document.**

## Built so far (uncommitted, 2026-10-15)

- `agent_version` + `instance` on `GetSourceStatusRequest`; weftd sends them on every status poll;
  the coordinator records them per host in `log_group_agents` (migration 095), refreshed at most
  every 10 min unless the version changes.
- `ingest.MinAgentVersionForExtract = "0.28.0"`; `handleConfigureSource` returns 409 (naming the
  oldest version and what is needed) for an extract-form schema on a weftd source whose recently
  seen (7 days) agents include one older, or none reported. Push sources and legacy-form schemas
  are exempt. A test pins weftd's own `Version` to be at least that minimum.
- `weft source list` AGENTS column; `GET /api/sources` carries `agents {count, oldest, newest}`.
- `weft source migrate` (in the CLI rather than `weft-admin`, since it needs only the user's
  login): converter `ingest.MigrateLegacy`, replay `ingest.CompareSchemas`, backup-first apply.
- Versions bumped: weftd 0.28.0, weft 0.8.0.

## Phase 1: ship the server, the CLI and the web (additive)

Everything in this phase is backward compatible by construction: the parser reads legacy-form
schemas exactly as before (`TestWeftdParseBudget` and the legacy parse tests pin it), migration
094 adds a nullable column, and the new endpoints are new.

Also in this phase, so the fleet becomes visible and protected:

- [ ] **Report the agent version.** Add `agent_version` to `GetSourceStatusRequest` (proto field,
      weftd fills it from `Version`); record the last version seen per source on `log_groups`
      (a small migration) and show it in `weft source list`. Old agents send nothing and read as
      "unknown".
- [ ] **Guard the configure path.** `handleConfigureSource` refuses an `extract`-form schema for a
      weftd source whose last seen agent version is below `MinAgentVersionForExtract` (or unknown),
      with a message naming the version it needs. Push sources are never guarded (the API parses
      them). This turns the silent failure into a clear error at configuration time.
- [ ] **Bump `cmd/weftd/version.go` and `cmd/weft/version.go`** in the same PR (the wire
      tripwire, `scripts/check-wire-bump.sh`, requires it for changes under `api/`, `cmd/weft/`,
      `cmd/weftd/`, `weftd/`, `proto/`). Do not use `no-wire-change`; this is a wire change. Open a
      PR rather than pushing to `main`: a direct push skips the tripwire and still auto-deploys.

Deploy mechanics (from experience): CI passes, then `deploy.yml` posts to the status service, then
`release.yml` fires some tens of minutes later and rolls the coordinator ASG; the post-deploy
`weft-admin roll-all` then relaunches shards one at a time (about 45 min for four). Confirm with
`kevcons instance refresh-status` reaching `Successful`, not by the Actions run alone. After a
successful deploy run `make collapse` so `store/schema.sql` absorbs migrations 094+.

**Gate to leave Phase 1:** refresh `Successful`; baseline unchanged (below); `weft source list`
shows versions populating as agents poll (every source's `GetSourceStatus` call reports one).

## Phase 2: upgrade the agents

The same version bump publishes the weftd `.deb` (`release.yml`). Then:

- [ ] Living agents self-upgrade within ~24h, or immediately on `systemctl restart weftd`.
      Confirm with the new version column, not by assumption.
- [ ] **Repin** each app's `weft.version` in its `.kevcons.yaml` to the new release (never to an
      unpublished one: `kevcons build` 404s) and rebuild at its next deploy, so replaced instances
      come up on the new binary instead of the old pin: csa, vimgym, vimgym-gitcore, olympics,
      grant_management, musclebones, sgokit (plus any producer not found in `~/dev`).
- [ ] Apps whose agent cannot self-upgrade (config fatals before the updater starts) need a rebuild;
      the version column makes them stand out.

**Gate to leave Phase 2:** every weftd source reports `>= MinAgentVersionForExtract`; no "unknown".

## Phase 3: put every producer on a schema

Start with an **inventory** (read-only) of `log_groups` per org: source type, status, and schema
form: (a) no schema, (b) legacy form (no `extract:`; or `envelope:` set; or a `*_pattern` refining
a positional field), (c) new form. Useful classifications over `schema_yaml`:

```sql
-- schemaless
select app, source_type from log_groups where status='configured' and coalesce(schema_yaml,'')='';
-- legacy form: has a positional field reference but no extract section
select app from log_groups where schema_yaml ~ '_field: "?[0-9]' and schema_yaml !~ 'extract:';
-- names an envelope
select app from log_groups where schema_yaml ~ 'envelope: (?!none)';
```

(`kevcons db query` tunnels through SSM and hangs intermittently: one query per call, in the
background.) Then, per source, by producer:

| Producer | Parsed by | Action | Gate |
|---|---|---|---|
| Heroku / Render / CloudWatch (Kinesis) sources | API, instantly | resample if needed, run the wizard (CLI or browser), verify | parse-rate check below |
| weftd sources, kevcons apps | agent, client-side | after Phase 2: `weft source resample`, wizard, verify | agent version gate (enforced by the guard) |
| weftd sources with a legacy `envelope:` | agent | re-wizard without an envelope: the prefix is read as ordinary fields. Check `instance` (the envelope stamped it) is still right | per source, by hand |
| anything schemaless | n/a | wizard | n/a |

For legacy-form schemas that convert exactly (positional field, no pattern) a migration tool
rewrites them to explicit specs. `weft source migrate` does this per source: it builds
the explicit schema, **replays the source's recent stored sample through both and diffs the
extracted rows**, and prints only the sources that are identical. Anything that differs
(`level_pattern` with its whole-field fallback, envelopes, multi-space timestamps) is listed for the
wizard instead. Apply goes through the API's configure path, never SQL: configure also evicts the
schema cache.

**Rules for every change in this phase**
- Before touching a source, save its current schema (`weft source schema ns/svc`) somewhere
  private. Not in git: dogfood account config never goes in the repo.
- Change one source at a time; verify; move on.
- Rollback for a source is `weft source configure ns/svc --file <saved>.yaml`.

**Equivalence / parse-rate check** (before and after, per source, over the same window):

```
weft sh -c "tail /log/NS/SVC/main.log | awk 'END{print NR}'"           # rows
# share of rows with LEVEL and with TS different from ingest time, and trace coverage
```
For weftd sources, also the agent's own line: `parse stats: N ok, M missing-level (X%)`.
A source is "done" when level coverage and timestamp accuracy are at least what they were, trace
ids correlate where expected, and nothing regressed in the dashboards/crons that read it.

**Gate to leave Phase 3:** the inventory shows zero (a) and zero (b) rows.

## Phase 3.5: forbid the old form

Before deleting anything, stop new legacy-form schemas from being created, or the inventory can
regress:

- [ ] `Schema.Validate` rejects a positional value with no `extract` entry, any `envelope:`, and a
      positional-refining `*_pattern`. (The configure endpoint and `weft source configure --file`
      both call it.)
- [ ] Soak for at least two weeks with parse stats clean and the inventory holding at zero.

## Phase 4: delete the fallback

Preconditions: inventory zero for legacy form; soak clean; every agent on a release that has the
Phase 3.5 validation (it does not matter that they still contain legacy code: nothing legacy remains
to read).

Delete in one PR:
- `ingest/legacy.go` (the envelope reader, `legacyFieldTrim`, `legacyTimestampTrim`);
- the legacy extractors in `parse.go` (`extractTimestamp`, `extractLevel`, `extractTraceID`,
  `extractSpanField` and the `*Re` fields they use, `strictLevel`, `needLegacyFields`, the
  `strings.Fields` split in `parse`);
- `Schema.Envelope` and its validation, and the test fixtures that exercise legacy reads
  (`TestIngestRoundTrip_*` with `Envelope: "kevcons"`, legacy cases in `parse_test.go`);
- the `Parser` collapses to the plan: every value is an `extract` value.

Then bump `cmd/weftd/version.go`, release, repin, rebuild (as Phase 2). The last release that
could read legacy schemas stays on the downloads page for rollback.

## Risks and how each is covered

| Risk | Cover |
|---|---|
| Old agent silently ignores a new-form schema | agent version on the wire + configure guard (Phase 1) |
| Schema change alters extraction on an existing source | baseline + equivalence replay + per-source rollback |
| Direct SQL edits leave a stale schema in the Valkey cache | all changes go through `ConfigureLogGroup` |
| Fatal-config agent can't self-upgrade | version column surfaces it; rebuild |
| Shard roll after deploy is slow and visible | expected (~45 min for 4); schedule accordingly |
| Legacy schemas reappear before deletion | Phase 3.5 validation |
| Regression discovered after Phase 4 | previous weftd release stays published; server side is revertable; saved schemas |

## Decisions needed

1. **The envelope.** Which prod sources use `envelope: kevcons`, and does anything read the
   `instance` it stamped? If yes, the migration must read `instance` another way (or accept the
   agent's hostname).
2. **The guard.** Is a hard refusal in configure right (recommended), or only a warning?
3. **Soak length** for Phase 3.5 (two weeks proposed) and whether the deletion rides a normal
   release or its own.

## Not yet verified

- The actual contents of production `log_groups` (no inventory has been run).
- Producers beyond the seven pinned apps in `~/dev`; sources created through the web.
- Whether anything outside weftd and the API parses rows with a schema (none found in the repo).
