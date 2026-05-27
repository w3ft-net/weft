# Collapse the three shard types into one

Today a single shard is represented by **three overlapping types** across
two packages, with hand-written mapping between them:

| Type | Package | Role | Times | Lock | Live handle | Behavior |
|---|---|---|---|---|---|---|
| `ShardRecord` | `store` | gorm row model | strings (RFC3339) | — | — | — |
| `ShardEntry` | `logdb` | in-memory runtime | `time.Time` | `mu` | `shard.Shard` | — |
| `ShardSM` (+`ShardSMDeps`) | `logdb` | FSM behavior | (holds `*ShardEntry`) | — | — | `stateless` |

`ShardRecord` and `ShardEntry` duplicate ~15 fields. The Record↔Entry
mapping is written by hand in at least five places — `registry.persist`,
`LoadFromStore`, `RefreshFromStore`, `MintShard`, and every narrow store
write (`SetShardLaunchResult`/`SetShardParquetPath`/`ClearShard*`) which
must write the row *and* separately mirror onto the entry. `ShardSM` is
1:1 with `ShardEntry` (one SM per entry, holding its pointer) and its
`ShardSMDeps` re-bundles collaborators the provisioner already holds.

## Why this is the real problem

The mapping seam is where this session's bugs lived:

- **The two-mutex data race** — `ShardEntry` was written by the SM under
  `ShardSM.mu` but read by the registry under `ShardRegistry.mu`, two
  locks over the same fields. (Fixed by giving `ShardEntry` its own mutex,
  but that's a patch on a split that shouldn't exist.)
- **`State` double-write** — `writeState` writes `shard_records.state`
  *and* mirrors `entry.State`, by hand, because they're two types.
- **Addr clobber** — the heartbeat path sets `entry.Addr` in memory but
  nothing writes the column, so `RefreshFromStore` (Record→Entry) wipes it
  each tick. A single type with one persist path can't drift like this.

The split was driven by a convention — "models live in the `store`
package" — not by a real boundary. `store`'s actual job is owning the one
DB connection, migrations, and tenant-scoping; it does not need to *define*
every model. The FSM already proves this: `logdb` imports `gorm` and writes
shard rows through `store.DB()`. The model can live with the behavior and
runtime state that operate on it.

## Target: one `logdb.Shard`

A single type in `logdb` that **is** the row, the runtime object, and the
state machine:

```go
type Shard struct {
    mu sync.RWMutex // guards the mutable persisted+runtime fields

    // ── persisted (gorm-tagged; maps to table shard_records) ──
    ID             string `gorm:"primaryKey"`
    BucketID       int64  `gorm:"not null;column:bucket_id"`
    Tier           string
    State          ShardState `gorm:"not null"`
    WindowStart    time.Time  // see "TEXT times" decision
    WindowEnd      time.Time
    CreatedAt      time.Time
    Addr           string
    InstanceID     string
    EBSVolumeID    string
    AZ             string `gorm:"column:az"`
    StripeIndex    *int   `gorm:"column:stripe_index"`
    ParquetPath    string
    RowCount       int64 `gorm:"default:0"`
    HeartbeatToken string
    LastHeartbeatAt *time.Time `gorm:"column:last_heartbeat_at"`
    LaunchAttempts  int        `gorm:"not null;default:0;column:launch_attempts"`
    IngestFailures  int        `gorm:"not null;default:0;column:ingest_failures"`

    // ── runtime only (gorm:"-") ──
    Services       []store.ShardService `gorm:"-"`
    Live           shard.Shard          `gorm:"-"` // gRPC/cold handle
    sm             *stateless.StateMachine `gorm:"-"`
    deps           *ShardDeps              `gorm:"-"` // shared, not per-shard
    launchInFlight atomic.Bool             `gorm:"-"`
}

func (*Shard) TableName() string { return "shard_records" }
```

- The FSM methods (`Fire`, `Evaluate`, the `OnEntry`/guard actions) become
  methods on `*Shard`. `ShardSM` disappears.
- `ShardSMDeps` becomes one shared `ShardDeps` the provisioner owns and
  every `Shard` references (a pointer), not a per-shard copy.
- The registry's map becomes `map[string]*Shard`.
- Persistence lives in `logdb` against `store.DB()`: load = a gorm
  `Find`/`First` into `*Shard`; save = `Save`/narrow `Update`. The
  Record↔Entry mapping functions are **deleted** — there's nothing to map.
- `store` keeps the connection, migrations, tenant-scoping, `Bucket`,
  heartbeats, and `shard_services`; it loses `ShardRecord` and the
  shard-row methods (which move to `logdb`).

The DB schema is unchanged — `TableName()` keeps the `shard_records`
table; only the Go representation collapses.

## Key design decisions (resolve before building)

1. **TEXT times — DECIDED: migrate to `timestamptz`.** `window_start/end`
   and `created_at` are TEXT (RFC3339) today; `last_heartbeat_at` is
   already `timestamptz`. Phase 1 ships a migration that `ALTER`s those
   three columns to `timestamptz` (`USING col::timestamptz` — RFC3339
   strings cast cleanly), so the unified type holds them as native
   `time.Time` with no serializer. Per the migration-collapse rule the
   schema.sql collapse is a *post-deploy* step (the migration lands at
   version > the schema's max; `make collapse` runs after prod applies).

2. **`api/admin.go`'s list-shards.** It does a raw gorm query into
   `[]store.ShardRecord` and JSON-encodes it. Options: (a) `api` imports
   `logdb` and queries `[]logdb.Shard` (the runtime fields are `gorm:"-"`
   so they serialize as zero/omitted — fine); (b) `logdb`/provisioner
   exposes a read method returning a small admin DTO. **Recommend (a)** —
   `api` already imports many internal packages; the gorm:"-" fields just
   don't render.

3. **What else moves out of `store`? — DECIDED: shard model + row methods
   only.** The shard-row methods (`SaveShard`/`GetShard`/`ListShards`/
   `SetShard*`/`Clear*`/`Increment*`) move to `logdb`. `shard_services`
   helpers (`StampShardServices`, `ShardIDsServingService`,
   `ServicesForBucket`), the heartbeat table (`LiveShardHeartbeats`/
   `InsertHeartbeat`, `ShardHeartbeat`), and `Bucket`/`BucketSM` **stay
   put** — separate tables, smaller overlap, out of scope here.

4. **How far to fold behavior.** Fold `ShardSM` fully into `*Shard` (the
   type carries its own `stateless.StateMachine` and actions). `ShardDeps`
   stays as the one collaborator bundle (it's services, not shard data —
   not part of the overlap the user flagged).

## Phased migration (each phase compiles + `-race`-green)

**Phase 1 — merge the data (ShardRecord + ShardEntry → Shard). SHIPPED.**
- **1a** (`40506cf`): migration 062 (`timestamptz`) + `ShardRecord`'s
  three time fields → `time.Time` (matching `ShardEntry`). This deleted the
  string↔time mapping at every Record↔Entry boundary (`persist`/
  `LoadFromStore`/`RefreshFromStore` assign directly; the bucket SM compares
  `time.Time`). Both types still existed — the time-handling foundation,
  not the merge. **The schema.sql collapse is post-deploy** (run `make
  collapse` after prod applies 062).
- **1b** (`9313d64`): the type-merge proper. `logdb.Shard` is the unified
  type (gorm tags + `TableName()`, `gorm:"-"` on runtime fields); shard-row
  methods moved to `logdb.ShardStore`; `store.ShardRecord` deleted; registry
  holds `*Shard`; `api/admin.go` points at `logdb.Shard`. `ShardSM` still
  wrapped `*Shard` here — only the data types collapsed.

**Phase 2 — fold the behavior (ShardSM → Shard). SHIPPED.**
- The `stateless.StateMachine`, `Fire`, `Evaluate`, and all action/guard
  methods are now methods on `*Shard` (the `sm`/`deps`/`launchInFlight`
  fields live on the struct, `gorm:"-"`). `ShardSMDeps` → one shared
  `*ShardDeps` the provisioner builds once (sync.Once) and every shard
  references; `bindSM`→`ensureSM` (idempotent, double-checked) binds it.
  `ShardSM` is deleted; `State(ctx)`→`smState(ctx)` to dodge the `State`
  field. The `State(ctx)` accessor that read the DB stays (behavior
  unchanged — Evaluate is still DB-authoritative, not in-memory).
- The provisioner's `shardSMs` map is gone: the registry's
  `map[string]*Shard` IS the SM set. `LoadStateMachines`/`MintShard` bind
  via `ensureSM`; `FireShard`/`ProcessHeartbeat`/`shardByInstance`/the
  Evaluate loop drive registry entries directly (binding lazily). Bonus: a
  GONE shard now leaves no leaked SM (it's removed from the registry).

**Phase 3 — cleanup + verify. DONE (lock audit).**
- The "remove now-dead narrow store writes" item was moot: `SetShardState`/
  `SetShardLaunchResult`/`Clear*` are the live persistence layer on
  `ShardStore`, not dead. No mapping helpers or string-time parsing of the
  model remain (1a/1b removed them; the only `RFC3339` left is wire
  formatting for the Configure RPC's string fields).
- **Lock audit:** the per-`Shard` mutex (`sh.mu`) is now the single lock
  guarding a shard's mutable fields — both the FSM methods and the registry
  take it; the map is guarded by `r.mu`; order is `r.mu→e.mu` and no FSM
  method holds `sh.mu` across a registry call (`TouchHeartbeat`/`Remove`
  run lock-free of `sh.mu`). The original two-mutex race is structurally
  gone. `sm`/`deps` are published under `sh.mu` in `ensureSM`, which
  precedes every `Fire` (happens-before holds for the `go runLaunch()`
  goroutine too). The audit found one pre-existing gap — `AuthenticateShard`
  read `entry.HeartbeatToken` lock-free while `RefreshFromStore` rewrites it
  under `e.mu` each tick — now read under `e.mu`. Latent (same value
  written, so `-race` hadn't tripped it).
- Full suite green under `-tags test -race`; both integration tag variants
  compile.

## Risks / trade-offs

- **`logdb` owns a gorm model.** This is the deliberate reversal of the
  "models live in `store`" convention. `store` stays the single
  connection/migration owner; `logdb` owns the shard model + its
  persistence. Other model types are unaffected.
- **Big diff across `store` + `logdb` + `api` + tests.** Phase 1 alone
  touches every shard-row call site. Mitigated by the existing test
  coverage (unit + the new `-race` integration test) catching regressions.
- **Time-column migration.** The TEXT→`timestamptz` `ALTER` runs against
  prod data; the `USING col::timestamptz` cast must handle every existing
  value (all are RFC3339, written by `time.Format(time.RFC3339)`). Verify
  on a copy first; it's a rewrite-the-column `ALTER` (table-rewrite lock,
  brief at current row counts). schema.sql collapse is post-deploy.
- **Not a goal:** merging `Bucket`/`BucketSM` similarly (separate, smaller
  overlap), or the `shard_services`/heartbeat tables.

## Cross-references
- `docs/plans/asg-as-api-stateless.md` — the FSM work that exposed the
  mapping seams (the race, the addr clobber, the State double-write).
