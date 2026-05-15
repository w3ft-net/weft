# Plan: weftd config is operator-authored YAML, not deb-generated

## Problem

On external customer hosts the `.deb`'s `weftd.service` runs
`ExecStartPre=/usr/lib/weftd/weftd-config.sh`, which re-renders
`/etc/weftd/config.yaml` from `WEFTD_*` env vars (`/etc/default/weftd`)
on **every weftd start**. The renderer is a fixed 2-source template
(one journal `unit:`, one glob `path:`); it cannot express `kind:`,
extra sources, per-source `app:`/`version:`/`metrics:`, or `dimensions:`
— all of which the weftd binary's `Config`/`SourceConfig` already
supports. Operators who hand-edit the generated file have it silently
clobbered on the next restart. Net: the supported configuration surface
is far narrower than the binary, and the only escape (env var sprawl)
was explicitly rejected.

This is the customer-host analogue of the routing problem that started
this work (weft's own hosts were handled in the
[[coordinator-query-audit-split]] plan via `deploy/startup.sh` + the
binary-level glob/explicit deconfliction).

## Decision

`/etc/weftd/config.yaml` is **operator-authored and authoritative**. The
deb stops generating it. Env is demoted to a single credential override.

Locked decisions:

1. **Token credential chain (AWS-shaped, env wins):**
   `WEFTD_TOKEN` (env) → `token:` (file). Env var, if set non-empty, is
   authoritative; otherwise the file value. This is the *only* env
   override and exists only because the token is a credential. Env
   surface is **token-only**. `WEFTD_ENDPOINT` is not part of the
   contract (it's a dev/local-testing affordance only; endpoint lives in
   the authored file).
2. **Packaging:** `config.yaml` is operator-owned — never shipped, never
   written, never merged by the package, and **not** a dpkg conffile.
   Ship `config.yaml.example` (plain sample/docs, not a conffile).
3. **Migration is passive inheritance** — no migration code. The
   renderer (not dpkg) is what overwrites `config.yaml` today; remove the
   renderer and the last-rendered file simply stays on disk (it was
   never dpkg-tracked) and weftd keeps reading it. Zero ingest gap.

## Changes

### Binary — `cmd/weftd/main.go`
- Token resolution chain: read `WEFTD_TOKEN` from env; if non-empty it
  wins, else use parsed `cfg.Token`. Keep the existing
  "token is required" fatal, evaluated **after** the chain (so a file
  with no `token:` but env set is valid, and vice versa).
- Missing/unreadable `/etc/weftd/config.yaml`: fail with an actionable
  fatal — `no /etc/weftd/config.yaml; copy and edit
  /etc/weftd/config.yaml.example`. (weftd already `log.Fatal`s on
  missing endpoint/token; this is message work + the missing-file case.)
- Unit test: env-overrides-file, file-fallback, both-empty → fatal.

### systemd unit — `deploy/weftd/weftd.service`
- Remove `ExecStartPre=/usr/lib/weftd/weftd-config.sh`.
- Keep `EnvironmentFile=-/etc/default/weftd` (optional `-`) so the
  token chain still receives `WEFTD_TOKEN` on inherited hosts.
- Crash-loop guard for un-configured fresh installs (Restart=always +
  fatal = tight loop): add `StartLimitIntervalSec`/`StartLimitBurst`
  (and/or a distinct exit code with `RestartPreventExitStatus`) so an
  unconfigured weftd goes `failed` and visible via `systemctl status`
  instead of spamming. **Open decision** — see below.

### Renderer + env file
- Delete `deploy/weftd/weftd-config.sh`.
- Rewrite `deploy/weftd/weftd.default`: keep it as the dpkg conffile but
  repurpose to env-override only — `WEFTD_TOKEN` (documented as the
  override in the credential chain) and a comment that all structural
  config now lives in `/etc/weftd/config.yaml`. Drop
  `WEFTD_APP/UNIT/LOG_PATH/HOST_GROUP/METRICS/SOCKET/ENDPOINT` (they only
  fed the dead renderer).

### deb packaging — `deploy/weftd/deb/DEBIAN/*`
- `postinst`: shrink to `mkdir -p /etc/weftd /var/run/weftd` + install
  `config.yaml.example`. Do **not** write `config.yaml`. `enable` weftd
  but see crash-loop decision re: whether to `start` on fresh install.
- `conffiles`: unchanged (`/etc/default/weftd` only). Explicitly do
  **not** add `config.yaml`.
- Ship `/etc/weftd/config.yaml.example`: well-commented, demonstrates
  multi-source with explicit `kind:`, and uses YAML anchors/merge keys
  (`&`/`*`/`<<`) to DRY genuinely repeated fields. Note: yaml.v3 has no
  `!include`; "DRY" = anchors only. An `x-anchors:` holder block is safe
  (weftd uses plain `yaml.Unmarshal`, which ignores unknown keys) —
  pin with a parse test.

### Docs
- `docs/manual/weftd.md`: replace the env-var configuration section with
  "author /etc/weftd/config.yaml" + the credential-chain note.

## Migration behavior (no code)

- **Existing host that has run weftd:** last-rendered `config.yaml` is on
  disk, not dpkg-tracked → untouched by upgrade; renderer gone → never
  overwritten again. weftd reads it, ingest continues. Operator edits
  now stick. They opt into richer config (kinds/extra sources) on their
  own schedule.
- **Fresh install:** no `config.yaml` → weftd fatals with the actionable
  message; operator copies `config.yaml.example`, sets token (in file or
  via `WEFTD_TOKEN`), starts weftd.

## Unconfigured-fresh-install behavior (decided)

`postinst` enables but does **not** `start` weftd — the operator starts
it after authoring `config.yaml`. Additionally add
`StartLimitIntervalSec`/`StartLimitBurst` to `weftd.service` so a host
started while still unconfigured caps its restarts and lands in
`failed` (visible via `systemctl status`) rather than a silent 5s
crash-loop. Both belt and suspenders: no auto-start on fresh install,
and a hard restart cap as the backstop.

## Out of scope
- weft's own EC2 hosts (`deploy/startup.sh`) — already handled.
- The prod cron path-migration regression (task #8) — unrelated.
