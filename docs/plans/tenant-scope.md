# Plan: pluggable tenancy and permissions (shape + permissions adapter)

## Status

🟡 In progress. Written 2026-11-09. Phase 1 and the Scoper/Authorizer seams
are in (`1091269b`, `45f4f4e5`, `8b466a3b`, `7d287b05`, `e0eefc74`,
`d579a916`). Namespace-owned crons are in (`2e68cd81`, `cad75572`,
`e21764ba`, `30c74d55`, `7fa2c00f`, `61fdbf6c`): home namespaces, the
`-r`/`-w` groups and cron users, `ns/name` references, restricted dashboard
tiles, and the `/bin` dependency rule. Phases 3-5 are not started.

## Why

Two motivations, one design:

1. **Hosted weft's isolation is enforced ad hoc.** The tenant boundary is
   a `customer_id` SQL predicate on shared shards, plus a scattering of
   Go-side checks that disagree about what an empty/zero identity means.
   Several paths (live tail, cron fire, source endpoints) enforce a
   *different* scope than the historical query path. See "Current gaps".
2. **A future open-source build** wants tenancy to be an adapter: a
   single-tenant default with no orgs, the hosted org/team model as one
   implementation, and room for someone else to write (say) an SSO-group →
   namespace mapping, without touching the planner.

The deliverable for (2) is not more implementations. It is **an interface
plus a written contract plus a conformance suite**, so that a third party
can implement tenancy correctly without reading the planner.

There is no quick-fix phase. Every current gap is a symptom of
permission logic that isn't behind an interface: 79 `ActiveOrgID` /
`activeOrg()` sites in `api/`, and 87 owner/permission/superadmin
checks across 15 files. Patching them in place would just be redone
when the adapter lands. The gaps close in the phase that builds the
seam they belong to (see "Current gaps").

## Concepts

- **Coordinate shape**: the columns that locate a row, chosen **at
  compile time**. The open-source default is 4 fields (namespace, env,
  service, kind). Hosted weft uses 5, adding a tenant (`customer_id`).
  The default build has no tenant anywhere: no column, no partition,
  no code path. The tenant is a hosted-only concept.
- **Partition columns**: the subset of the shape that owns storage
  (role `RolePartition`): caches, per-key tables, hive paths, metering
  and retention key on them. Hosted: tenant. Default: none.
- **Actor**: a user acting within a partition (today's `Session`:
  user, active org, owner and permission bits). It's what reads and
  actions are judged against.
- **Agent**: an ingest credential (a `wft_` token bound to a namespace
  and optionally an env). It's what ingest stamps come from. Agents
  never read or act. Keeping these as two types means an agent can't
  be passed where read permission is checked.
- **Namespace**: the unit of ownership for **data and artifacts**.
  Logs live in a namespace, and so do crons and their output (see
  "Crons are namespace-owned"). Nothing runs on borrowed user authority.
- **Scope**: what a principal may *read*, as a set of coordinate tuples,
  computed per request. The type is static, the value is dynamic.
- **Permissions adapter**: the runtime seam that decides everything
  about a principal. It has two halves:
  - the **Scoper** decides data visibility (scopes)
  - the **Authorizer** decides actions (writes and administration)

  Hosted implements it over today's org/team tables. The default build
  has one implicit owner who may do everything.

## Interface

The shape is a **build input to the coordinator only**. It's generated
into code before compile, with no type parameters and no reflection.
Every other binary (shard, export, compute, the cold-scan Lambda, weftd,
the CLI, the SDKs) is the same in every build. Shards receive their
storage layout as data from the coordinator, the same way they already
receive SQL for reads.

### The shape spec

```yaml
# shape.yaml: the open-source default (committed)
coordinates: [namespace, env, service, kind]   # base columns, by name
```

```yaml
# hosted shape.yaml (private repo)
coordinates: [namespace, env, service, kind]
added:
  - name: customer_id
    type: int64         # travels to shards; see "Column types" below
    role: partition     # owns storage: caches, per-key tables, hive paths
    pinned: true        # stamped from the ingest token, never agent-supplied
    required: true      # never a wildcard in a scope tuple or stamp
```

There are two kinds of columns:

- **Base columns** are the row vocabulary every build shares:
  namespace, env, service, kind, region, cluster, k8s_namespace and so
  on. They're `LogRow` fields, because weftd and the ingest adapters
  fill them in. A shape *selects* from them and assigns roles.
- **Added columns** never appear in `LogRow` in any build, hosted
  included. The coordinator stamps them into its own generated row
  wrapper. They reach shards only as per-batch partition values (see
  below). Added columns must be `pinned`. An added column that agents
  had to fill in would need a custom weftd, and that's out of scope.

### What the generator emits

`make generate SHAPE=path/to/shape.yaml` (a Go `text/template` tool,
run before `go build`) produces one package, **`shape/`**, imported only
by the coordinator:

- `type Coord struct{...}`, with one field per coordinate
- `Columns`, `Get(i)` and `With(i, v)` as generated switches
- `type Row struct{ *pb.LogRow; Added }`, the coordinator's stamped row,
  plus `FromRow(Row) Coord` as direct field access, with no reflection
- `Layout()`, the wire descriptor of the partition columns (names,
  types, roles) sent to shards in `Configure`
- `TableKey(Coord) string`, the per-key table name (today's
  `logtable.Name`, moved here). For the hosted shape it must produce
  byte-identical names, and a golden test pins that.
- `Fingerprint`, a hash of the spec

**`weft.proto` is the same in every build.** There's no proto
templating. Shape-dependent values cross the wire only as typed-by-
`Layout` positional values, at batch granularity.

Weft's hand-written coordinator code (intersection, the SQL and matcher
backends, routing, pruning, cron lowering) uses `shape.Coord` directly.

**The default shape's generated output is committed.** `go build` on a
clean checkout works with no generate step, and CI fails if it's stale
(the same rule as `make docs`).

### The permissions adapter

```go
package tenancy

// Actor and Agent come from the Identity adapter (auth); the
// permissions adapter never parses tokens itself.
type Actor interface {
    // Partition is the opaque control-plane partition (today: org ID)
    // that store calls are scoped by. Default build: a constant. See
    // open question 4.
    Partition() StorePartition
}
type Agent interface {
    Partition() StorePartition
}

// Scoper decides data visibility.
type Scoper interface {
    // IngestStamp: pinned coordinates set, the rest Any.
    IngestStamp(ctx context.Context, a Agent) (shape.Coord, error)
    // ReadScope: what an actor may read right now. Called per request,
    // never cached across requests.
    ReadScope(ctx context.Context, a Actor) (shape.Set, error)
    // NamespaceScope: what an artifact homed in ns may read right now,
    // i.e. ns itself plus its active cross-namespace grants. ns ==
    // OrgLevel gives the org-level artifact scope. Called at every cron
    // fire.
    NamespaceScope(ctx context.Context, p StorePartition, ns NamespaceRef) (shape.Set, error)
}

// Authorizer decides actions. Resource optionally carries a coordinate
// or namespace for namespace-scoped actions.
type Authorizer interface {
    Authorize(ctx context.Context, a Actor, act Action, r Resource) error
}
```

Actions are a closed enum covering today's checks:

- manage `/bin` and `/etc`
- manage crons *in a namespace* (`ManageCron(ns)`), replacing the
  org-wide `CanManageWatches` bit
- add a namespace's cron user to another namespace's read group
  (`usermod -aG y-r x-cron`), which needs membership in `y-r` and `x-w`
- create namespaces
- create tokens in a namespace
- configure, resample or delete a source
- team and billing changes
- superadmin operations

Both halves are runtime interface values, wired in the coordinator's
and API's `main`. The shape fixes the *type* of a scope; the Scoper
computes its *value* per request.

**The dividing rule:** anything that exposes coordinates or log content
goes through `ReadScope`. That covers queries and live tail, and also
the source endpoints (a source *is* a coordinate) with their sample
lines, schemas and field facts. Anything that changes state goes through
`Authorize`. Nothing else in weft reads `IsOwner`, `org_permissions`,
`namespace_permissions` or `ActiveOrgID`.

### Crons are namespace-owned

**The leak this fixes.** Data is namespace-scoped
(`namespace_permissions.can_read`), but crons are org-scoped. `/cron.d`
is one flat directory per org: crons are unique by (org, name) with no
namespace, `CronMount.Authorize` (`mounts/cron.go:24`) lets every member
read it, and writes are gated by the org-wide `CanManageWatches`.
Dashboards render cron output with no namespace check. A cron reads
namespace-scoped data and publishes the result org-wide, so any cron
manager can launder a namespace they can read into output every member
can see. Making crons run "as their owner" would only narrow this. It
would also need a stored owner reference resolved at every fire,
because the cron has no identity of its own.

**The model.** Every cron has a home namespace, and its authority comes
from that namespace, not from a person:

- **Path:** `/cron.d/<ns>/<name>`.
- **Reads:** the cron fires as its namespace's cron user, `<ns>-cron`
  (see "Groups and the cron user"), and reads what that user's groups
  cover: its own namespace, plus any other namespace's read group it has
  been added to. A cron in X that also reads Y needs
  `usermod -aG y-r x-cron`. Nothing is resolved per person at fire time,
  and "the owner left" isn't a case. Removing the membership fails the
  cron's next fire visibly.
- **Output, definition and history** live in the home namespace.
  Readers need `ReadScope` to cover the namespace. That applies to
  `cat /cron.d/<ns>/<name>`, `weft::describe` and friends, the MCP cron
  tools, and dashboard tiles. Output is never more visible than the
  data that went into it.
- **Writes:** creating, editing or deleting is
  `Authorize(actor, ManageCron(ns))`, which is membership in `<ns>-w`.
- **No new org-level crons.** The mount refuses to create a cron directly
  in `/cron.d`, with a message pointing at `/cron.d/<ns>/`, and
  `ActionPublishOrgWide` guards the rest. Existing org-level crons keep
  firing as the system identity and only an owner can edit or delete them,
  until an owner moves them (`mv /cron.d/x /cron.d/<ns>/x` re-homes one as
  the same row, keeping history and results; it is refused if the cron
  reads namespaces the new home's cron user can't). There is no automated
  migration. When the last org-level cron is gone, the system-identity fire
  path can go with it.
- **`/bin` scripts a cron runs.** Any member may create or change a script
  no cron runs. A script a cron runs, directly or through other scripts,
  needs `x-w` for every such cron's namespace (an org-level cron that runs
  it makes it owner-only), and can't be deleted while a cron runs it. A
  cron may name a script that doesn't exist yet; creating it then needs the
  same group. A cron calls a script by path (`/bin/scan`), never by a bare
  or computed name, so the dependency set can be read statically. The
  coordinator stores it (`116_cron_bin_deps.sql`) and enforces the cron's
  read limit again at fire time.

### Groups and the cron user

Every namespace `x` has two groups and one system user, with Linux
semantics:

| | Is | Holds |
|---|---|---|
| `x-r` | group | read `x`'s logs, and `x`'s cron definitions, output and history |
| `x-w` | group | manage `x`: its crons, tokens and sources (this replaces `cronmgr` and `can_create_tokens`) |
| `x-cron` | user | the identity `x`'s crons fire as; it cannot log in or own a team seat |

Creating a namespace creates all three, with `x-cron` in `x-r`; that
membership is an ordinary row, so it shows in listings. Deleting the
namespace removes them and every membership that names them.

`usermod -aG <group> <user>` adds a user to a group, and `usermod -rG`
removes one (`adduser` only adds a team member). Cross-namespace access
is `usermod -aG y-r x-cron`. **You can only hand out what you hold:** the
caller must be in `y-r` and in `x-w`. `x-cron` can only be added to
`-r` groups, since nothing needs it to write. It holds no org bits and is
never an owner, so the Authorizer denies it every action. A membership
stays when the person who made it loses access; the listing shows who
made it.

**Namespace directories are virtual and automatic.** `ReadDir("/cron.d")`
returns one directory per namespace in the reader's `ReadScope`, plus
the org-level cron files. A namespace's cron directory exists as soon as
the namespace does, and is invisible to readers who can't read it.

- `mkdir` and `rmdir` under `/cron.d` fail with the mount's permission
  message. There is no special pointer to namespace creation.
- Deleting a namespace deletes its cron user and everything that user was
  granted elsewhere. It does not refuse because crons remain.
- Tab completion and `ls` need nothing special: completion goes through
  the mount's `ReadDir`, and `ProxyFS.Prefetch` warms `/cron.d`
  (`shell/proxyfs.go:147`) with one `walk=1` request that covers every
  namespace directory under it, so the first `ls` or tab inside one is
  warm and the cost doesn't grow with the number of namespaces.

**References.** Anything that names a cron takes `<namespace>/<name>`: the
`weft::` functions, `alert` references, dashboard tiles, the MCP cron
tools and the cron REST routes (`/api/crons/{ns}/{name}`). A bare name is
an error (`a cron is named by its namespace and name`), with no cwd
resolution. A reference to a cron in a namespace the caller can't read
reads as "not found". A cron firing reads another namespace's crons only if
its cron user holds that namespace's `-r` group. Existing stored bare
references aren't rewritten; owners fix them.

**Dashboards stay org-wide; tiles are checked one by one.** A dashboard
(`/etc/dashboards`) is readable by every member, including the YAML file
itself, which may name crons in namespaces the reader can't read. Only
what the tiles show is gated: each tile renders only if the viewer's
`ReadScope` covers that tile's cron home namespace. A denied tile comes
back `restricted: true` in its slot, keeping the title the author wrote
but with no cron path and no values, and when any tile is restricted
`yaml_body` is withheld from the API response. The layout is unchanged.
Saving a tile requires the editor to be able to read its `<ns>/<name>`
cron, else `cron "<ref>" does not exist`. This is the `handleGetDashboard`
`readSeries` loop gaining a scope check per tile. Redacting the YAML mount
is not planned.

**Existing crons.** There is no automated migration. A cron created before
namespace homes stays org-level until an owner moves it with `mv`.

**`required` replaces "zero means skip".** The coordinator validates
every stamp and scope tuple against the generated column list, and a
wildcard in a required column is a denial. That's the fail-closed rule
as a property of the shape, not scattered `if id == 0` checks.

### Shards are shape-agnostic

On reads the shard already knows nothing about the shape: it runs the
coordinator's SQL, and `resolveSource` (`shard/layout.go:235`) only
rewrites `FROM`. On writes and storage maintenance it currently knows
`customer_id` in six places. Each one becomes "do what the coordinator
or the volume says":

| Shard-side consumer | Today | After |
|---|---|---|
| DDL + appender | hardcoded `customer_id BIGINT` (`shard/shard.go:240`, `:489`); `r.CustomerId` per row (`:628`) | Partition columns from `shard.json`. Values arrive once per batch, are converted to their declared type once, and are appended as constants after the base columns. |
| Per-key table | `logtable.Name(customer, …)` per row (`shard/layout.go:59`) | `table_key` sent by the coordinator per batch |
| Export | `COPY … PARTITION_BY (customer_id, …)`, const at `shard/partition.go:54`, used at `shard/shard.go:1077` | `PARTITION_BY` built from the `shard.json` on the snapshotted volume |
| Archive stats | `GROUP BY customer_id` (`shard/shard.go:897`, `shard/compact.go:35`) | `GROUP BY` the partition columns; stats carry values positionally. The default shape has one group. |
| Retention compaction | per-customer cutoffs join (`shard/compact.go:88-103`); rewrite with const `PARTITION_BY` (`:226`) | Cutoffs keyed by partition values, sent by the shape-built retention janitor. Columns come from the archive's hive path, as the Lambda already does. |
| Field facts | `sourceKey{customer, …}` built per row (`shard/fieldfacts.go:230`) | Keyed on the batch's `table_key`, which `FieldFactDelta` echoes back for the coordinator to decode |
| Cold-scan Lambda | reads hive paths; SQL from the coordinator | Unchanged |

**Ingest batches carry one partition.** The coordinator already groups
rows by (customer, namespace, service) before routing them to a shard
(`coordinator/drain.go:78-104`). So each coordinator→shard batch gets
`table_key` plus `partition_values` (repeated string, positional per
`Layout`), sent once per batch. The default shape sends none.

**The volume describes itself.** `Configure` writes the layout and
fingerprint into `shard.json` (`shard/identity.go`). Export runs as
`weft-shard export` on a throwaway instance booted from a snapshot of
the volume (`logdb/shard_export.go`), so it can't ask the coordinator
mid-run. It doesn't need to: it reads the same `shard.json`. An archive
is always written in the layout its data was ingested with, even if the
coordinator has since been rebuilt with a different shape.

**A mismatch can't reinterpret data.** The coordinator refuses shards
that heartbeat a fingerprint it doesn't recognise, and a shape change
means new shards. Existing hosted volumes have no layout in
`shard.json`. They are adopted as the legacy layout (`customer_id
BIGINT`), which the hosted shape reproduces exactly.

**Column types are preserved end to end.** `customer_id` is `BIGINT`
in DuckDB and declared `BIGINT` for hive reads (`shard/cold_shard.go:147`).
Losing that to `VARCHAR` would make comparisons slower and pruning less
effective on both shards and cold scans. So `Layout` carries each
column's type, and shards convert the batch's string values back to
that type once per batch.

### Wire transition (hosted)

The deploy order is coordinator, then compute, then shard, and wire
changes are additive, so old receivers must stay correct.

1. **Additive.** The new coordinator sends both `LogRow.customer_id`
   (26) *and* the batch `table_key`/`partition_values`. New shards
   prefer the batch values. Old shards keep reading tag 26.
2. **Cut over.** Once every live shard reports the new version, the
   coordinator stops setting tag 26. The public proto keeps
   `reserved 26;` by number only.
3. **Dead-letter queue.** Stored entries are protobuf rows with tag 26
   already stamped (`coordinator/dlq.go:112`). New entries store the
   stamp alongside the row. Legacy entries are decoded by a legacy row
   type that lives in the hosted repo, until the queue holds none.

A cross-version test covers the new coordinator with old shards, and
the new coordinator with new shards, for both ingest and archive.

### Building with a shape

Only the **coordinator** is built from the generated tree. The **API**
also gets a custom `main`, but only to wire the Identity adapter, which
is a separate seam from the shape. Everything else is a stock release
binary.

A third party:

1. Checks out weft at a release tag.
2. Drops in `shape.yaml`, plus their Scoper and Identity packages.
3. Runs `make generate SHAPE=… && make build-coordinator`.

They then run stock shards, compute and weftd against it. Hosted weft
does exactly this in CI. It has to run before the AMI build, because the
`kevcons build` worker has no Go toolchain. If this becomes common, a
`weft-build` tool (in the spirit of xcaddy) can wrap the steps.

### Performance

There's no meaningful shard-side cost to the layout coming from the
coordinator instead of being compiled in:

- **SQL is the same work.** COPY, stats and compaction statements are
  strings DuckDB parses and plans. One built from a constant and one
  built from `shard.json` cost the same.
- **Per-row key building moves to per-batch.** The table key and the
  field-fact source key are known once per batch, instead of being
  rebuilt for every row as `layout.go:59` and `fieldfacts.go:230` do
  today.
- **The appender is unchanged per row.** `AppendRow` already takes
  `driver.Value`s, so `r.CustomerId` is converted to an interface value
  on every row today. A batch constant converted once is no slower. The
  rule: keep base columns written directly, and append the partition
  constants after them. Never drive the appender with a loop over a
  column list and a per-column switch on every row.
- **Column types must survive** (see above). That's the one way this
  could regress.

Before calling it done, benchmark both shard ingest throughput (before
and after, hosted shape) and the live matcher against today's
`RowMatcher`.

### Coordinates are IR

Scope does not compile on its own. The **source** of a query (the
pseudofile paths, globs, `warp.log`) and the **scope** are both
*coordinate sets*, and the planner works on their intersection:

```go
// package shape: hand-written, over the generated Coord
func Intersect(source, scope Set) Set // empty result = deny
```

The intersection is a pinned `coordNode` at the head of the IR. Passes
may not move, drop or merge it. Every consumer of "which rows can this
query see" is a backend over that one value:

| Backend | Must be | Today |
|---|---|---|
| Shard SQL WHERE | **exact** | `addSourcePredicates` + `addCustomer`, per-column IN |
| Live-tail matcher | **exact** | hand-built `RowMatcher` from `plan.Source*` |
| Shard routing | superset | `ShardDescriptorsFor(nsID, service)` |
| Cold parquet pruning | superset | time only (`shard/cold_prune.go`); hive paths already carry customer/ns/svc/kind |
| Per-key table selection | superset | `sourceTable`, single-source only |
| Lowered cron plan | exact, re-intersected at fire | baked customer ID |
| Source/metadata endpoints (`/api/sources*`) | exact | org filter only, by name, no namespace check |

Superset means it can include rows that don't match but must never
drop one that does. Content filters (grep/awk/jq) stay as they are:
SQL-string `filterNode`s, with the interpreter as the authority for live
tail. Coordinates are a closed set of equality checks; content is an
open-ended language. That's the line between them.

Rules this gives us:

- **Every non-required column has three cases:** `Any`, `Null` and
  `Value(v)`. Today `""` means both "any env" (a 3-segment path) and
  "this row has no env". `Null` separates them. Storage keeps `''` for a
  missing value, so already-archived parquet needs no migration. The
  SQL backend renders `Null` as `col = ''`, and the live matcher does
  the same. The differential test covers `Null` explicitly. Required
  columns have only `Value(v)`.
- **Wildcards resolve against the scope.** `warp.log`, globs and an
  unnamed namespace mean "every tuple in scope", never "no predicate".
- **An empty intersection is a deny**, decided before routing, with an
  error that doesn't reveal whether an unreadable source exists.
- **Tuples, not per-column products.** `cat a/api b/web` reads exactly
  those two tuples, not `{a,b} × {api,web}`. The SQL backend may render
  compactly (shared-prefix grouping, or row-value IN if DuckDB plans it
  well; to be measured), but the IR stays a union.

Reference implementations:

- The default `shape.yaml` (4 base columns) + `tenancy/single`:
  - `ReadScope` returns `Set{Coord{}}` (all `Any`: everything).
  - `NamespaceScope` returns everything.
  - `Authorize` allows everything.

  This is the open-source default.
- Hosted: a `shape.yaml` adding a required, pinned, partition-role
  `customer_id`, plus an org/team Scoper and Authorizer
  (`saaskit/orgs`, `org_permissions`, `namespace_permissions`,
  owner, superadmin). Both live in the private hosted repo and never
  appear in public code.

## The contract

These are the invariants an implementation (and weft's own consumers)
must uphold. The conformance suite tests each one.

1. **There is no unscoped read.** Every shard query, cold-scan
   query, live-tail subscription and trace query is built from a
   `shape.Set` from the Scoper. Nothing in the planner, coordinator or
   shard accepts a raw customer ID, and a wildcard in a `required`
   column is a hard error, never a skip.
2. **Same Scope, same rows, on every path.** Historical SQL, live-tail
   in-memory matching, cold parquet scans and lowered cron plans
   must admit exactly the same rows for the same Scope. All of them are
   backends over the one intersected `coordNode` (see "Coordinates are
   IR"). The exact backends (SQL, matcher) are held equal by a
   differential test: generate random coordinate sets and rows, then
   assert DuckDB's evaluation of the SQL matches the Go matcher on the
   same rows. The superset backends (routing, pruning, table selection)
   get a property test that they never drop a matching row.
3. **Scopes only constrain shape columns.** Never payload or fields.
   Only `Pinned` columns are a trust boundary. Unpinned ones (today:
   service and kind, which agents supply) restrict *visibility*, but
   they are not a boundary between agents. The spec states this
   explicitly.
4. **Artifacts have no borrowed authority.** A cron's scope is
   `NamespaceScope(home)`, recomputed at every fire, never baked in at
   save time. Revoking a grant revokes the cron's access on its next
   fire. Artifact output is never readable by anyone who can't read the
   artifact's home namespace. (Shared links aren't artifacts in this
   sense: `handleGetSharedLink` returns only `{bin, range, args}`, and
   the viewer runs it as themselves.)
5. **Partition columns are the only cache/storage key.** Caches are
   never keyed on the rest of the scope. Weft guarantees scope is
   applied *after* any partition-keyed cache lookup.
6. **Partition columns come first.** The compiled SQL places
   `RolePartition` predicates first in WHERE (DuckDB zonemap pruning
   relies on this; see `planner/sql_test.go:43`).
7. **Errors fail closed.** Any Scoper or Authorizer error means the
   request is denied. There is no fallback scope.
8. **One gate per kind of access.** Anything that exposes coordinates
   or log content is filtered by `ReadScope`. Anything that changes
   state passes `Authorize`. No other code consults org or permission
   tables. A lint test enforces this, the same way
   `TestNoStdlibLogPrintsOutsideTerminalTools` enforces logging.

## Current enforcement points

Where the boundary is decided today, and what replaces each one.
References are as of `28e48315`.

| Point | Today | After |
|---|---|---|
| Caller identity (HTTP) | `auth/session.go:88` BuildSession → `ActiveCustomerName` (a **name**) | Identity adapter → `Actor` |
| Caller identity (gRPC) | `coordinator/service/server.go:60` `callerCustomer` returns name; `coordinator.go:471` `resolveCustomerID` name→ID, `""`→0 | `Actor` on ctx; no names on the read path |
| Shard SQL | `planner/sql.go:224` `addCustomer`, 0 = no predicate | SQL backend over the intersected `coordNode` |
| Namespace authz (logs) | Go-side `authorizeSources` (`coordinator.go:1096`) | Scope ∩ source, empty = deny |
| Namespace authz (traces) | SQL `namespace IN` (`planner.go:282`, `traces.go:55`) | Same intersection, same backends |
| Live tail | `RowMatcher{Customer,Namespace,Service,Kind}` (`coordinator/row_matcher.go:22`); no env, first namespace only | Matcher backend over the same `coordNode` |
| Shard routing | `ShardDescriptorsFor` (`logdb/provisioner.go:440`); nsID 0 = every shard | Route backend; "every shard in the partition" is explicit |
| Cold scan / Lambda | Same SQL forwarded; Lambda has no tenant knowledge | Same SQL backend, plus hive pruning on shape columns |
| Cron fire | `executeLowered` binds `lp.CustomerID` from stored JSON (`coordinator.go:726`); slow path runs as system identity `userID=""` | `NamespaceScope(home ns)` each fire |
| Ingest stamp | `server.go:701-715` forces customer + namespace, env only if token has one | `Scoper.IngestStamp(Agent)`, applied at the same choke point |
| Postgres | GORM callback on enrolled models (`store/tenant.go`), fails closed on 0 | Unchanged; this already matches the contract. Enroll the stragglers (below) |
| Redis tail fan-out | `tail:<cust>:...` channels (`coordinator/tail_fanout_redis.go:75`) | Keyed by partition columns (contract 5), then the matcher |
| Source endpoints | `api/sources.go`: org-only filters (`:164`, `:459`), no namespace check on list/pending/schema | Filtered by `ReadScope` (contract 8) |
| Action permissions | `IsOwner`/`Can*`/`AuthorizeNamespaceTokens`/`IsSuperadmin` across `api/`, `mounts/`, `auth/`, `coordinator/` (87 sites) | `Authorize(action, resource)` |
| Store partition | `session.ActiveOrgID` / `activeOrg()` (79 sites in `api/`) | `Actor.Partition()` |

## Current gaps

From an inventory of the code on 2026-11-09. Items marked ✅ were
re-verified by reading the code; the rest are reported but not yet
re-verified. **Closes in** names the phase whose seam fixes it. There
are no interim fixes.

| Gap | Closes in |
|---|---|
| 1 zero means everything | 2 (planner takes only a scope set; `required` columns) |
| 2 live tail ignores env | 3 (generated matcher, `Null` env) |
| 3 cron scope | 2 (done for namespaced crons; org-level crons still fire as the system identity until moved) |
| 4 source endpoints | 2 (filtered by `ReadScope`; done in `f48651e7`) |
| 5 unauthenticated compute | infra-interfaces plan |
| 6 un-enrolled tables | Deferred until the table-shape question is settled |
| 7 legacy mixed archives | not fixed by this plan |
| 8 multi-source product | 3 (tuple sets) |
| 9, 10 cleanup | 2 (9 done: `mounts.Config.CustomerName` is gone; 10 done: `nsIDCache` entries expire after a minute) |

1. ✅ **Zero means "everything" in the query path.** `resolveCustomerID("")`
   → 0 → no SQL predicate; `ShardDescriptorsFor(nsID=0)` → every shard.
   Today only `callerCustomer`'s empty-name check stands between a bug
   and an unscoped cross-tenant read. `store/tenant.go` fails closed
   on 0. The two conventions disagree. Not reachable today: every
   external query enters through `callerCustomer`, and crons pass
   `cust.Name` from `GetOrgByID`.
2. ✅ **Live tail ignores env.** `RowMatcher` has no env field, so
   `tail -f /log/ns/prod/svc.log` admits other envs' live rows while
   the historical batch filters them. It also matches only the first
   namespace of a multi-source read. (Within one tenant, so a scope
   bug, not a cross-tenant one.)
3. ✅ **Cron fast path binds the customer from the persisted plan JSON**
   (`lp.CustomerID`), not `watch.OrgID`, contrary to the comment at
   `planner/sql.go:170`. Cron reads are authorized only at save time
   (owners and Lua are exempt), and fires run as the all-reading system
   identity. Revoking a user's namespace access doesn't affect their
   crons.
4. ✅ **Source API endpoints skip namespace checks.** `handleListSources`
   loads all orgs' log groups and filters in Go **by name**
   (`api/sources.go:459-497`). `handlePendingSources` (`:164`) filters
   by org only and returns each source's `Sample`, which is real log
   lines, from namespaces the member can't read. Only the POST routes
   have middleware (`RequireEmailVerified`). **This is the one live
   exposure**, within an org. Tenants can't reach each other through it.
5. **Unauthenticated compute.** `cmd/compute` has no auth interceptor,
   and the Lambda runs any SQL it's sent. Both rely on network placement.
   Out of scope here (see the infra-interfaces plan), but contract 1 only
   holds if nothing else can reach them.
6. **Un-enrolled tenant tables:** `log_groups`, `namespace_permissions`,
   `shard_field_facts` (which uses `customer_id`, not `org_id`), `usage`,
   `cold_scan_daily`, `cron_results`. Each method passes the org by hand.
   `namespace_permissions` and `cron_results` have no `org_id` column, so
   enrolling them needs a migration and backfill. **Deferred:** the plan
   doesn't yet say how tenant tables should be shaped (`org_id` on every row,
   or reached through a scoped parent), so decide that first.
7. **Legacy archives mix tenants in one parquet file**, and the S3 prefix
   is per service label (`logdb/shardsm.go:963`). It's correct only
   because of the SQL predicate. It's fine under contract 1, but it means
   per-tenant deletion (data residency, GDPR erasure) of legacy archives
   is a rewrite, not a prefix delete.
8. ✅ **Multi-source reads are a per-column product.** `addSourcePredicates`
   (`planner/planner.go:526`) emits `namespace IN (…) AND service IN (…)
   AND kind IN (…)` (documented at `planner/parse.go:16`), so
   `cat /log/a/api/main.log /log/b/web/main.log` also admits `a/web` and
   `b/api` rows if those exist. That's within one tenant, so it's a
   correctness bug, not a leak. It's verified by reading the code, not by
   running it. Live tail checks only the first namespace, so it disagrees
   with the SQL in the other direction.
9. `mounts.Config.CustomerName` is set (`shellfs.WithCustomerName`) and
   never read. Delete it.
10. ✅ `nsIDCache` entries now expire (`nsIDTTL`, one minute), so a deleted and
   re-created namespace stops routing ingest to the old ID.

Related, tracked elsewhere: the coordinator log leaking cross-tenant query
content (`compliance/cross-tenant-coordinator-log.md`). Under this design,
an operator principal reading `/log/weft/*` is just another Scope. The fix
there is not to log tenant content into a stream whose scope is wider than
that tenant.

## Phases

There's no quick-fix phase; each gap closes when its seam lands (see
"Current gaps"). The one live exposure, source endpoints returning sample
lines across namespaces, closes in Phase 2.

**Phase 1: customer names off the read path (~1d).** No behaviour
change, except that an empty identity can no longer mean "no predicate".

- `callerCustomer` returns the session's `ActiveOrgID`. The gRPC handlers
  stop calling `GetOrgByName`.
- `Execute`, `ExecuteWithDir`, `ExecuteFollow`, `execute`, `runQuery`,
  `preflight`, `executeQuery`, `runLocal` and `rewriteQueryPipelines` take
  an org ID, not a name. `resolveCustomerID` is deleted. Names stay for
  display and log lines only, looked up where they're printed.
- Starts with a failing test: a zero org ID is rejected before it reaches
  the planner.
- The shape generator is **not** in this phase. It has no caller until
  Phase 4 (two shapes, `Layout`, `TableKey`, `Fingerprint`), so it moves
  there. `shape.Coord` and `Set` are hand-written for the hosted shape in
  Phase 2, where `ReadScope` first returns them. `Intersect` lands in
  Phase 3.

**Phase 2: permissions adapter and namespace-owned crons (~10d).**
Define `Actor` and `Agent` (with `Partition()`), `Scoper` and
`Authorizer`. Write the hosted
implementation in-tree, over today's tables: `org_permissions`,
`namespace_permissions`, owner, superadmin and `ReadableNamespaces`.
Then route every check through it:

- **Queries:** the coordinator gets `ReadScope` per request and hands
  the planner its existing inputs (customer ID, readable namespaces),
  derived from that set. Phase 3 replaces those inputs with the set
  itself. A `required` column can't be `Any`, so the customer ID can't
  be 0. Add a test that `resolveCustomerID("")` and `addCustomer(0)`
  are unreachable. `ShardDescriptorsFor` gets an explicit "every shard
  in the partition" option instead of nsID 0. *(Closes gap 1.)*
- **Source endpoints:** list, pending (sample lines) and schema are
  filtered by `ReadScope`, with the org filter done in SQL. Configure,
  resample, probe and delete go through `Authorize`. *(Closes gap 4.)*
- **Crons (~4d of this phase):** implement "Crons are namespace-owned".
  - ✅ `/cron.d/<ns>/<name>`, with virtual namespace directories in
    `CronMount.ReadDir`, and a single-request prefetch of `/cron.d` and its namespace directories.
  - ✅ Cross-namespace grants as cron-user group membership, not a table.
  - ✅ Namespaced crons fire as their cron user, bound to its scope, and a
    revoked grant fails the fire visibly.
  - ⬜ The system-identity fire path (`userID == ""`) remains for existing
    org-level crons, and goes when the last one is moved.
  - ✅ Per-tile scope checks in `handleGetDashboard`.
  - ✅ `<ns>/<name>` references across `weft::`, `alert`, dashboard tiles,
    the MCP tools and the REST routes.
  - ✅ The `/bin` dependency rule.
  - Dropped: the automated migration, the namespace/org-level name
    collision rule, and refusing to delete a namespace that has crons.
  - The shell help and manual pages for `/cron.d` and `weft::` were
    updated (`docs/audit/tenant-scope-changes.md`).

  *(Closes gap 3 for namespaced crons.)*
- **Actions:** the 87 `IsOwner`/`Can*`/`AuthorizeNamespaceTokens`/
  `IsSuperadmin` sites in `api/`, `mounts/`, `auth/` and
  `coordinator/` become `Authorize(action, resource)`. The 79
  `ActiveOrgID`/`activeOrg()` sites become `Actor.Partition()`.
- **Enforcement:** the lint test from contract 8 lands here.
- **Tables:** enroll the un-enrolled ones (gap 6). Delete
  `mounts.Config.CustomerName`, and make `nsIDCache` invalidate on
  namespace delete (gaps 9, 10).

Each gap fix starts with a failing test.

**Phase 3: coordinates as IR (~4d).**

- Replace `sourceInfo`'s per-column slices with a `shape.Set`. The
  planner takes the `ReadScope` set directly, intersects it with the
  source, and pins the result as `coordNode`.
- Write the backends: SQL, the live matcher (replacing `RowMatcher`'s
  coordinate half), routing, cold pruning, table selection and the
  source endpoints' filter.
- Add the differential and superset tests, including `Null` env.
- Logs and traces stop differing on namespace authz.
  `authorizeSources` goes away; the empty-intersection deny replaces
  it. *(Closes gaps 2 and 8.)*

**Phase 4: shape generator, shape-agnostic shards, and extract the
hosted adapter (~9d).**

- Write the generator and the `shape/` output (the hand-written
  `Coord`/`Set` from Phase 2 become its output), for the hosted and
  default shapes.

- `Configure` carries `Layout` and `Fingerprint` into `shard.json`.
  Volumes with no layout are adopted as the legacy `customer_id BIGINT`
  layout.
- Ingest batches carry `table_key` + `partition_values`. The shard
  appender writes typed per-batch constants. `logtable.Name` moves to
  the coordinator's `shape.TableKey`, with a golden test for
  byte-identical hosted names.
- Export, archive stats, compaction and field facts take their columns
  from `shard.json`, the hive path, or the coordinator's request, per
  the "Shards are shape-agnostic" table.
- Run the wire transition in three steps: additive, cut over, drain the
  dead-letter queue. Add the cross-version test (new coordinator ↔
  old/new shards, for ingest and archive).
- Benchmark shard ingest throughput before and after.
- Split `cmd/coordinator` and `cmd/api` into `app.Main(adapter, ...)`
  plus thin default `main`s.
- Move the hosted `shape.yaml` and the org/team Scoper and Authorizer
  into the private hosted repo. Its CI checks out weft at a tag,
  generates, and builds before the AMI step.
- Commit the default shape's generated output and `tenancy/single`. CI
  fails if the generated output is stale.

After this, the public tree has no tenant field or column, and no code
outside the adapter reads org or permission tables. `planner/`,
`coordinator/`, `shard/` and `logdb/` import no saaskit packages. Only
the coordinator is built per shape. Existing hosted data is unaffected:
the hosted shape produces today's column names, table names and
partition paths. A golden test pins that.

**Phase 5: spec + conformance suite (~2d).** `docs/tenancy-spec.md` (the
contract above, written for an outside implementer) plus
`tenancy/conformance`, a test harness any adapter runs against.

- **Scoper:** it ingests rows for ≥2 partitions and several
  envs/namespaces (including `Null` env), then asserts invariants 1–7
  across historical, live, cold, cron and source-endpoint paths.
- **Authorizer:** for every action, a principal the adapter denies is
  denied at every route that performs it.

It runs against several generated shapes in CI: the default, the
hosted one, and a deliberately odd test shape (e.g. `region` as an
extra coordinate, plus two added columns). That way the generator and
the hand-written algebra are proven shape-independent, not just right
for the two shapes we ship.

## Non-goals

- GCP/k8s/other infra. Those are a separate infra-interfaces plan
  (provisioner, block volume, object store, serverless scan, config
  source), which follows the same pattern: an interface, a contract,
  and a conformance suite, with only the AWS and Docker implementations
  shipping.
- Postgres RLS. The GORM callback already fails closed. Revisit only if
  raw-SQL escape hatches keep growing.
- Per-tenant shards or physical isolation. Scope works the same over
  shared or dedicated shards; that's a cells/pricing decision.

## Open questions

1. Should scopes constrain `service`/`kind`, given those are
   agent-supplied? Proposal: yes, for visibility, with the spec stating
   they are not a trust boundary between agents of the same tenant.
2. ~~A cron whose owner loses access?~~ **Resolved:** crons have no
   owner. They're namespace-owned, and their authority is the home
   namespace plus grants.
3. ~~Do cron output and dashboards inherit the cron's read scope?~~
   **Resolved:** output lives in the home namespace. Dashboards stay
   org-wide, with per-tile scope checks.
4. **Control-plane tenancy in Postgres.** The coordinate shape covers
   log rows only. About 12 enrolled tables plus several unenrolled ones
   carry `org_id`. Proposal: the open-source build keeps `org_id` with a
   single implicit org, since it's never visible to users and not on a
   hot path. The alternative is making the control-plane schema
   shape-dependent too, which is a much larger change.
