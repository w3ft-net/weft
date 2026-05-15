# Plan: split coordinator query-audit stream + author weftd YAML config

## Problem

A class of crons (`compute-exit-127`, `shards-without-node`, `stream-errors`, …)
monitors `app=weft/coordinator` for a substring. The coordinator logs every
query it runs — including the cron's own probe pipeline — via `log.Printf`
to stderr → systemd journal → the single `weft/coordinator` (kind=main)
pseudofile. The probe and the string it searches for share one stream, so the
monitors detect themselves and alert every cycle forever. Verified: filtering
the probe lines (`grep -v 'compute='`) leaves zero genuine matches.

Root cause is a shared sink, not the cron definitions. weftd already supports
per-source `kind:` (cmd/weftd/main.go:102-121, path built at :292 as
`<ns>/<svc>/<kind>`), but production weftd config is rendered from env vars by
`deploy/weftd/weftd-config.sh` and never sets `kind:`, so everything collapses
into kind=main. A journal `unit:` source cannot be content-routed.

A second, independent bug compounds the noise: the affected crons use
`grep -c`, which exits non-zero on a zero count, and the cron alert
disposition treats that as an alert (healthy = alerting). Tracked here as a
separate workstream; it is not fixed by the stream split.

## Decision: no new env var; author YAML directly with anchors

Retire the `weftd-config.sh` env→YAML renderer. Ship an operator-authored
`/etc/weftd/config.yaml` and use YAML anchors + merge keys (`&`, `*`, `<<`) to
DRY genuinely repeated source fields. Note: yaml.v3 has **no** `!include`
directive; "YAML include" here means native anchors/aliases/merge keys, which
cover the repeated-fields case exactly. yaml.v3's plain `Unmarshal` ignores
unknown top-level keys (weftd uses plain `yaml.Unmarshal`, main.go:140), so an
`x-anchors:` holder block is safe to carry shared fragments without the
`Config` struct rejecting it — this is verified by a unit test in Workstream B.

## Workstream A — Coordinator: dedicated query-audit sink

1. Add a package-level audit logger in `coordinator` writing to a separate
   file (default `/var/log/weft/queries.log`, `O_APPEND|O_CREATE`), with a
   `--query-log` flag on the coordinator binary for the path. If the file
   can't be opened, fall back to stderr (never lose audit lines, never crash).
   This is coordinator config — not a weftd env var; separate concern.
2. Move all four `query:`-prefixed lines in `coordinator/coordinator.go`
   (1014, 1031, 1044, 1060) from `log.Printf` to the audit logger. The whole
   `query:` family is audit telemetry; moving it as a unit (not just the
   `compute=%v` line) keeps the rule simple and removes every self-match
   vector at once.
3. Ensure `/var/log/weft/` exists and is writable by the coordinator's
   service user (created by the coordinator at startup before opening the
   file). weftd runs as root (weftd.service `User=root`) so it can read it.

After this ships, the audit lines simply leave `weft/coordinator` (main).
Real coordinator events (genuine "compute exit 127", "stream error", "no
LAUNCHING row" — ordinary stderr logs) stay in coordinator-main, so the
existing crons that watch for *real events* keep working and **need no
repoint** — they just stop self-matching.

## Workstream B — weftd: authored YAML config (drop the renderer)

1. Remove `ExecStartPre=/usr/lib/weftd/weftd-config.sh` from
   `deploy/weftd/weftd.service`; weftd runs directly against
   `/etc/weftd/config.yaml`. Delete `weftd-config.sh` and the
   `/etc/default/weftd` env file + its conffile entry.
2. Ship `/etc/weftd/config.yaml.example` in the deb (well-commented,
   anchor-based). postinst creates `/etc/weftd/` and does **not** overwrite an
   existing `config.yaml` (config.yaml is host-specific and secret-bearing —
   the token lives in it — so it is operator-authored, not a packaged
   conffile with a baked default).
3. Example config demonstrates anchors only for genuinely repeated content
   (e.g. a shared `metrics`/`version` fragment via `&frag` / `<<: *frag`);
   do not invent repetition to justify anchors.
4. Add a weftd unit test asserting plain `yaml.Unmarshal` parses a config
   with an `x-anchors:` holder block + alias/merge usage into the expected
   `Config` (unknown key ignored, aliases resolved). This pins the design
   assumption stated above.
5. On the coordinator host's config, add the dedicated source:

   ```yaml
   - path: /var/log/weft/queries.log
     app: coordinator
     kind: queries
   ```

   producing `/log/weft/coordinator/queries.log`. New source onboards
   automatically via `onboardAndRun` (sample → schema → configured).

## Workstream C — Rollout (ordered)

1. Ship the coordinator change (Workstream A). Audit lines stop entering
   `weft/coordinator`-main; until weftd tails the new file they are simply
   absent from weft (acceptable — they were the noise). Real errors unaffected.
2. Bump weftd version per `docs/versioning.md` (declare + gate), build a real
   artifact (`go build -o weftd/weftd ./cmd/weftd/`), package the deb.
3. Author `/etc/weftd/config.yaml` on the coordinator host (anchors + the
   `kind: queries` source) and roll the new weftd to that host. Server's
   soft-tolerance version-skew window means this is staged, not flag-day.
4. Verify `/log/weft/coordinator/queries.log` populates and
   `/log/weft/coordinator.log` no longer contains `compute=[` probe lines;
   confirm the self-referential crons go quiet.
5. Repoint to `weft/coordinator/queries` **only** any monitor that
   legitimately needs the audit stream (query volume/latency dashboards).
   Event monitors stay on coordinator-main.

## Workstream D — `grep -c` alert inversion (separable)

Independent of the split. Investigate the cron alert disposition
(`alert/args.go` `--if-zero`/`--if-nonzero`, `coordinator/cron.go`) and the
affected cron definitions, then change the pipelines so a zero count is the
*healthy/ok* outcome and alerting is threshold-driven, not driven by
`grep -c`'s non-zero exit on no matches. Scope/spec this after reading the
cron runner; do not guess the fix in this plan.

## Out of scope

- Whether a real "compute exit 127" signal is actually emitted to
  coordinator-main at all (cron-correctness question, separate from churn).
- The shard reconciler — verified healthy (`ops=0`, steady state); not touched.
