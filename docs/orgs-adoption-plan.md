# Weft adoption of `saaskit/orgs` — continuation context

Status: **not started.** This is a handoff doc for the next session. The shared
library and the vimgym consumer are **done, committed, and green**; weft is the
remaining consumer to adopt the shared org/membership/role shape.

## Where we are (already shipped)

Two commits on `main` (both repos):

- **`saaskit@0a7d5c0`** — new package **`saaskit/orgs`**: a self-standing
  organization/membership/role core. Read `saaskit/orgs/orgs.go` first — it is
  small and is the contract weft adopts.
  - Identity-agnostic: `user_id` is an **opaque string**, no FK to any users
    table. Queries bind it as a string, which Postgres accepts against a UUID
    *or* TEXT column — so weft's text/JWT user ids fit unchanged.
  - Runs on **`*sql.DB`** (no ORM dep in saaskit). A gorm app passes
    `gormDB.DB()`.
  - `org.id` is **UUID** (`gen_random_uuid()`); `SchemaSQL` is the canonical
    neutral DDL (organizations + memberships, TEXT user_id, no users FK).
  - API: `New(*sql.DB)`, `CreateOrg(ctx,name,creatorUserID)` (creator→admin),
    `AddMember`/`RemoveMember`, `Role`/`IsAdmin` (the access primitive),
    `AdminOrgFor`, `Get`, `Members`.
  - 6/6 DB tests green (isolated in their own pg schema).

- **`olympics@ff6f761`** — the **reference adoption**. Look at it to see the
  intended integration shape:
  - `pkg/team/team.go` gets an `orgs.Store` via `orgs.New(db.DB.DB())` and layers
    app-specific *reporting* on top (joins its own tables). orgs owns
    org/membership; the app owns everything that joins app tables.
  - `migrations/013_create_teams.sql` — the app applied the schema with its own
    migration tooling and *tightened* the neutral shape (UUID user_id + FK to its
    users table). weft will do the equivalent with its own migration tooling.
  - `cmd/server/team_api.go` — HTTP handlers using `AdminOrgFor` for access
    scoping.

## Dockerized test workflow (use this — Docker is the sanctioned path here)

olympics added `scripts/dockertest.sh` (a replace-based `go.work` runner mirroring
`dev-server.sh`, so a cold container with no git auth resolves private
warp/saaskit locally). weft's dev stack differs; replicate the pattern:

```
# bring up a postgres, then run go test with SAASKIT/WEFT test DB URL pointed at it
docker compose up -d postgres    # or weft's equivalent
docker compose run --rm -e <WEFT_TEST_DB_URL>=... server bash -c \
  'export PATH=$PATH:/usr/local/go/bin && cd /workspace/weft && go test ./store/... -count=1'
```

Note: the golang image needs `export PATH=$PATH:/usr/local/go/bin` under `bash`,
and a login shell (`bash -l`) drops it — that cost us a cycle.

## The load-bearing reality of weft's tenancy (read before planning)

**Weft's `Customer` already *is* its org**, and weft already has a mature,
security-critical multi-tenancy layer. This is a *reconciliation*, not greenfield.

- **`store/customer.go`** — `Customer` is `gorm.Model` → **`uint` autoincrement
  PK**, `Name` (uniqueIndex), `DisplayName` ("human-readable org/team name"), with
  FK children `Namespace`, `AWSAccount`, `AgentToken` (each `CustomerID uint`).
- **`store/tenant.go`** — **structural row-level tenant isolation via gorm
  callbacks.** Enrolled models are auto-scoped by the `customer_id` column on
  every `db.WithContext(ctx)` query; a ctx with no customer **fails closed**
  (`ErrNoCustomerInContext`). Entry points: `WithCustomer(ctx, customerID uint)`,
  `WithoutTenantScope(ctx)`. `tenantColumn = "customer_id"`. Migration **053**
  made `customer_id` uniform across tenant tables.
  - Enrolled models (`store/store.go` `Open()` → `s.tenant.enroll(...)`):
    `Dashboard`, `ShellFunction`, `OrgPermission`, `NotifyConfig`,
    `PagerDutyIntegration`, `CronWatch`, `TmpFile`, `Subscription`, … (read the
    full list in `store/store.go`).
- **`store/store.go`** — `Open(dsn)` runs **goose migrations** (`store/migrations/*.sql`
  authoritative, *no* AutoMigrate), opens gorm, installs tenant scoping. Get the
  `*sql.DB` for `orgs.New` via `gormDB.DB()`.
- **Pre-existing overlapping concepts** — weft ALREADY has: `store/team_groups.go`,
  `store/permissions.go` (`OrgPermission`, enrolled + "Org" terminology already in
  use), `store/subscription.go` (billing — `orgs.Org` also carries stripe/seat
  columns). **These must be reconciled with `orgs`, not duplicated beside it.**
- **User identity** — JWT (`auth/jwt.go`, `jwtauth.Claims`, user id is text). weft
  has its own magic-link path (`store.CreateMagicLink`). Good: matches orgs'
  opaque-string user_id.
- **Blast radius** — ~40 non-test files reference `CustomerID` / `scoped(` /
  `customer_id` across `api/`, `coordinator/`, `mounts/`, `planner/`,
  `shellfs/`, `store/`. Full list: `git grep -l 'CustomerID\|scoped(\|customer_id'`.

Constraint from the user: **weft has 0 paying customers → destructive is OK.** No
data backfill, no dual-write, no online migration. Drop and recreate freely.

## THE central decision the next session must make first

Re-keying `customer_id uint` → `org_id UUID` means touching the **callback-based
tenant-isolation layer** (`tenant.go` `tenantColumn`, every enrolled model, every
`WithCustomer(ctx, uint)` call site) — i.e. the security boundary of a
multi-tenant log service. Weigh two paths before writing code:

- **Path A — full re-key (what "refactor weft to the shared shape" literally
  asks).** `Customer`→`organizations` (UUID PK), `tenantColumn = "org_id"`,
  children FKs `CustomerID uint`→`org_id UUID`, `WithCustomer(ctx, uint)`→
  `WithOrg(ctx, string)`, adopt `orgs.Store` for org CRUD + memberships.
  Reconcile `team_groups`/`OrgPermission` into `memberships(role)`. Largest, and
  it rewrites the security-critical path — but it's the true unification.
- **Path B — bridge / thin adoption.** Keep weft's internal `uint customer_id`
  tenancy (it works and is security-load-bearing) and adopt `orgs` only for the
  *new/shared* membership + role surface, mapping org UUID ↔ customer uint at the
  edge. Much lower risk; less "unified."

Recommendation to surface to the user: **A** matches their stated intent and the
0-customers freedom, but confirm they accept rewriting `tenant.go`'s enforced
boundary before starting. If any hesitation, **B** delivers the shared shape at
the membership layer without disturbing log-tenant isolation.

## Path A execution sketch (if chosen)

1. **Migration (destructive)** under `store/migrations/NNN_*.sql`: drop
   `customers` + children (or recreate); create `organizations` (use
   `orgs.SchemaSQL` as the base, add weft-specific columns like the old
   `DisplayName`/`Name uniqueIndex`), re-point `namespaces`/`aws_accounts`/
   `agent_tokens` and every enrolled tenant table to `org_id UUID`.
2. **`store/tenant.go`**: `tenantColumn = "org_id"`; `WithCustomer(ctx, uint)` →
   `WithOrg(ctx, orgID string)`; update `guardCreate`/`scope`/`scoped`.
3. **Models**: `Customer`→`Organization` (drop `gorm.Model` uint PK; UUID string
   id). Children `CustomerID uint`→`OrgID string`.
4. **`orgs.Store`**: construct via `orgs.New(gormDB.DB())`; replace
   `FindOrCreateCustomer`/`GetCustomerBy*` with `orgs` calls where they're pure
   org CRUD; keep weft-specific bits (namespaces, tokens) in `store`.
5. **Memberships/roles**: fold `team_groups` + `OrgPermission` into
   `orgs.memberships(role)` (or keep weft roles atop orgs membership — decide).
6. **Call sites** (~40 files): mechanical `WithCustomer(ctx, id)`→`WithOrg(ctx,
   orgID)` and `uint`→`string` id threading. Compiler-driven; go one package at a
   time (`store` → `api` → `coordinator`/`mounts`/`planner`).
7. **Keep green**: `go build ./...` + `go test ./...` after each package. Weft
   must at minimum keep building (feedback: any warp/saaskit change must keep weft
   building). Watch `integration/harness.go` + `seed_helpers.go` (they seed
   customers).

## Versioning (task 5, do at the end)

Both consumers pin saaskit by version but resolve locally via `go.work` /
container replace, so local builds already see the new `orgs` package. For
release/CI: tag a new `saaskit` version and bump `require` in olympics
(currently `v0.2.0`) and weft (currently `v0.1.0`). See global CLAUDE.md
"Publishing a new version" (CodeArtifact) — but saaskit is consumed via
replace=>../saaskit in dev, so confirm how saaskit is actually distributed to CI
before bumping.

## Quick pointers

- Shared lib contract: `saaskit/orgs/orgs.go`
- Reference adoption: `olympics/pkg/team/team.go`, `olympics/migrations/013_create_teams.sql`
- Weft tenancy core: `weft/store/tenant.go`, `weft/store/store.go`, `weft/store/customer.go`
- Weft overlapping concepts: `weft/store/team_groups.go`, `weft/store/permissions.go`, `weft/store/subscription.go`
- Call-site inventory: `cd weft && git grep -l 'CustomerID\|scoped(\|customer_id'`
- Design doc for the whole team product: `olympics/docs/team-plan.md`
