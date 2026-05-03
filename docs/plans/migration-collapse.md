# Plan: collapse migrations into store/schema.sql post-deploy

## Status

Step 1 (weft side) shipped. Step 2 (kevcons `post_deploy` section)
deferred to the kevcons-specialist.

## Why

On 2026-05-02 a single deploy attempt cycled through four distinct
migration failures, each surfacing only when the AMI's user-data
ran `weft-migrate` against the production database in flight:

1. **Version-number collision after merge.** Both local and a remote
   PR landed migrations numbered `038_*`. Goose marks versions, not
   files, as applied — production DB had `038_subscription_comp_reason`
   in `goose_db_version`, so the new AMI (with `038_multi_org_membership`
   in that slot) silently skipped 38, then tried to re-apply 39's schema
   which was already in place.
2. **Goose `DO $$...$$;` parsing.** The multi-org migration had a
   PL/pgSQL block whose embedded `;` tripped goose's default statement
   splitter. Needed `-- +goose StatementBegin/End` annotations.
3. **Phantom column.** The same migration's user-id remap referenced
   `notify_configs.created_by_user_id`, a column that's never existed
   in any migration.
4. **Real orphan data in prod.** The migration's defensive
   `RAISE EXCEPTION` correctly fired on a `team_members` row pointing
   at a deleted customer.

Tests didn't catch any of these because `go test ./store/` runs
against a fresh DB and goose's library API, not the same parsing path
the prod binary runs. Three of the four bugs would have been caught
by **any** test that runs `weft-migrate` end-to-end against a DB
shaped like prod.

## Architecture

```
store/
  schema.sql                ← pg_dump of prod schema + goose_db_version
  migrations/
    .gitkeep                ← placeholder, lets //go:embed all:migrations
                              survive an empty post-collapse repo
    NNN_*.sql               ← only post-collapse deltas live here
```

`store.Migrate(dsn)` flow:

1. If the target DB has no `goose_db_version` table, apply
   `schema.sql` once. The dump carries both the DDL and the
   `goose_db_version` data (`pg_dump --data-only --table=goose_db_version
   --inserts`), so after this runs goose's max version equals the
   collapsed high-water mark.
2. `goose.Up(db, "migrations")` applies any post-collapse deltas.

A virgin DB after step 1 is **byte-for-byte equivalent to a prod DB at
the time of the last collapse**: same tables, same indexes, same
ledger of applied migrations. Tests that previously simulated prod by
running 1–N migrations on a fresh DB now match prod by construction.

## Why a snapshot, not a remote read

Collapse is a **local, deterministic** function of the working tree:

```
schema.sql + migrations/  →  new schema.sql, empty migrations/
```

The collapse script spins up a transient postgres, applies the
existing `schema.sql`, runs `weft-migrate` (which embeds the current
`migrations/`), dumps the result, and deletes the consumed files.
No prod credentials, no network, no kevcons. Re-running on an
already-collapsed repo is a no-op (no migration files to fold in).

The implication for the production-prevention story: the collapse
itself doesn't watch prod. It captures whatever was the on-disk
state of `migrations/` plus `schema.sql` at the moment it ran.
When kevcons adds a `post_deploy` hook (step 2), the trigger is
"`kevcons deploy` succeeded," and the script's job is just to
locally re-derive schema.sql + clear migrations/. The deploy
status is the only evidence we need that the new migrations are
in prod's `goose_db_version`.

## Implementation

### Step 1 — weft repo (DONE)

- `store/schema.sql` (committed): the collapsed dump.
- `store/migrate.go`: `applyCollapsedSchema(db)` runs schema.sql on
  virgin DBs (probed by `goose_db_version` table presence). `Migrate`
  calls it before `goose.Up`. The migrations FS uses
  `//go:embed all:migrations` so an empty post-collapse directory
  with only `.gitkeep` still satisfies the embed.
- `scripts/collapse-migrations.sh`: docker-builds `Dockerfile.postgres`
  (so pg_partman / pg_cron extensions are available), starts the
  container on a non-default port, applies current schema.sql + runs
  weft-migrate, dumps, removes consumed migration files, leaves a
  `.gitkeep` behind.
- `Makefile`: `make collapse` invokes the script.

### Step 2 — kevcons (TODO; for kevcons-specialist agent)

Add a `post_deploy:` section to `.kevcons.yaml` parsing. Mirror the
existing `prebuild:` shape:

```yaml
post_deploy:
  - name: Collapse migrations now that prod is at the new high-water mark
    run: make collapse
    commit:
      paths: [store/schema.sql, store/migrations/]
      message: "store: collapse migrations through the just-deployed AMI"
      on_error: warn  # collapse failure must not roll back the deploy
```

The `commit:` block is the real work — kevcons should `git add` the
named paths, generate a commit with the supplied message (using the
caller's git identity), and push. If the working tree is dirty
beforehand, refuse and log. If the push fails (e.g., remote moved),
log and exit with a notice; the operator can re-run `make collapse`
manually.

### Step 3 — runtime invariant check (optional follow-up)

Add a sanity check in `store.Migrate`: after `goose.Up`, query
`goose_db_version` for any version recorded as applied that doesn't
correspond to a file in `migrations/` AND wasn't included in
`schema.sql`'s ledger range. This catches the renumbering-collision
smell — a NEW file with a low version number that goose silently
skipped because it thought it was already applied.

## Gotchas

- **Custom postgres image.** The dev/prod postgres has pg_partman and
  pg_cron extensions added via `Dockerfile.postgres`. The collapse
  script must build that image, not pull stock postgres — migrations
  002+ require partman.
- **goose_db_version data needs `--inserts`.** `pg_dump --data-only`
  defaults to `COPY` syntax, which works but is less readable in code
  review. `--inserts` produces `INSERT INTO goose_db_version VALUES`
  rows. Both work at apply time.
- **Empty migrations directory.** `//go:embed migrations/*.sql` errors
  when no .sql files exist. Use `//go:embed all:migrations` (the
  `all:` prefix includes hidden files), and keep a `.gitkeep`.
- **Re-running `make collapse` after a successful collapse** produces
  an empty diff: schema.sql is unchanged, no migration files exist to
  fold in. Idempotent and safe.
- **Conflicting PRs.** If two PRs each add a new migration (e.g., 042
  and 043), the second to merge needs `git rebase` against main —
  same as today. Collapse doesn't introduce a new merge hazard; it
  just prevents the worse class of "renumbering across a merge"
  hazards from existing in the first place.
- **Schema drift.** If someone hand-edits prod's schema (DDL outside
  a migration), the next collapse picks up the drift and bakes it
  into schema.sql — silently. That's a separate problem (don't do
  out-of-migration DDL); collapse just records reality.
- **Data-shape bugs aren't caught.** The 4th bug from 2026-05-02
  (orphan team_members) is data, not schema. Collapse captures schema
  only. The right control there is a separate runtime invariants
  check: log warnings about data states (orphans, dangling FKs)
  rather than blocking migrations on them.

## Cross-link

- `CLAUDE.md` — note the new bootstrap path (`schema.sql` first,
  migrations second).
- The kevcons-specialist gets pointed at this doc when picking up the
  `post_deploy:` section work.
