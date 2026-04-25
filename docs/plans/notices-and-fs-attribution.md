# Notices, user attribution, and a generic flash-message system

**Status:** plan.

The hard-coded `tmp_warning_seen_at` we just shipped works, but it's
not the right shape. Three things this plan does differently:

1. **Generalize the warning into a notices system.** A typed
   `Notice` payload that flows from anywhere in the request stack →
   coordinator response → API server → user's display surface
   (browser banner/modal, CLI prepended text). One mechanism, many
   notices.
2. **Push user identity through the request stack.** Currently the
   coordinator sees customer but not user. Once it sees user, every
   notice is per-user (the right scope from the start), and every
   shellfs write knows who's writing — which we use for the next
   piece.
3. **Show the creator user in `ls -al`.** Today `ls -l` shows
   mode/size/mtime/name. Adding a user column lets a team see
   "alice wrote this script, bob set up that watch, charlie's
   incident notes." Standard Unix muscle memory; expected.

These are tightly coupled — the foundation under all three is the
same plumbing (user identity flowing into ShellFS / the store) — so
they ship together.

## What's wrong with what we have

The phase-1 amendment added `tmp_warning_seen_at` on `customers`.
It's:

- **Per-customer**, not per-user. Originally chosen because
  customer was already plumbed and user wasn't. With user plumbing
  in place, per-user is obviously right (one warning per human, not
  one per team).
- **Hard-coded into the schema**, with a single-purpose column.
  Every future "first-use notice" would be another column. That's
  not a system; it's a pile of columns waiting to grow.
- **Delivered through the warp interpreter's stderr buffer**, by
  passing `errBuf` as a side channel into ShellFS. That works for
  the CLI case but mixes a structural notice into a stream meant
  for command output. The browser has no good way to render it as
  a banner because by the time the API sees the response, it's
  just text.

The fix: drop both the column and the side-channel mechanism in
favour of a structured Notice flowing through a typed channel.

## Foundation: user identity through the request stack

Today's flow:

```
api/shell.go  ──HTTP→  Coordinator.Query(pipeline, tr, customer)
                              │
                              ▼
                       runLocal(pipeline, tr, customer, custID, alertCtx)
                              │
                              ▼
                       shellfs.New(store, custID, …)
```

The JWT claims already carry `UserID` (see `handlers.go:469`,
`handlers.go:1090`, etc.); `handleShellExec` just doesn't pass it
along.

The change:

```
api/shell.go  ──HTTP→  CoordinatorQueryRequest{customer, user_id, …}
                              │
                              ▼
                       Coordinator.Query(pipeline, tr, customer, userID)
                              │
                              ▼
                       runLocal(pipeline, tr, customer, custID, userID, alertCtx)
                              │
                              ▼
                       shellfs.New(store, custID, …, WithUser(userID, isOwner))
```

Concrete edits:

- **`proto/weftpb/weft.proto`** — add `string user_id = 6;` to
  `CoordinatorQueryRequest`. Repeat for any other coordinator-scoped
  RPC that should attribute writes (probably none right now —
  watches run as the cron executor, not as a user).
- **`api/shell.go:86`** — populate `UserID: claims.UserID` on the
  outgoing request.
- **`coordinator/coordinator.go`** — `Query`/`query`/`runLocal`
  signatures grow a `userID string` parameter. `runLocal` resolves
  ownership via `s.Store.GetUserByID` once if needed (e.g., to know
  if the user is a customer owner for the existing
  `WithUser(userID, isOwner)` call).
- **`shellfs.New`** — already accepts `WithUser`; wire it in.

Watches (`Coordinator.QueryForWatch`) explicitly run *as the watch*,
not as a user — they pass empty `userID`. The store layer treats
empty userID as "system" for attribution and skips per-user notices.

## Piece 1: The Notices system

### Schema

```sql
CREATE TYPE notice_kind AS ENUM ('success', 'warning', 'error');

CREATE TABLE user_notices (
    user_id     TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notice_key  TEXT        NOT NULL,
    kind        notice_kind NOT NULL,
    seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, notice_key)
);
```

A row's existence means "this user has seen this notice." Missing
row = not seen. The `kind` column is denormalized from the
in-code registry so dashboards / debugging queries don't need to
join against a content table that doesn't exist; it also means a
mismatched key in flight (registry says one kind, DB row another)
is a loud signal that the registry changed mid-deploy. Title and
body still live in code, keyed on `notice_key`.

Per-customer notices, when we eventually need them ("your team is
at 90% of ingestion quota"), are a spray: insert one
`user_notices` row per active member of the customer. No schema
change needed — the per-user grain is the right primitive, and
the broadcast is just iteration.

### Notice content registry

Notices are coupled to product behaviour; defining them in code is
clearer than a content table that has to be kept in sync with the
events that fire them. New file `coordinator/notices.go`:

```go
package coordinator

type NoticeKind string

const (
    NoticeKindSuccess NoticeKind = "success"
    NoticeKindWarning NoticeKind = "warning"
    NoticeKindError   NoticeKind = "error"
)

type Notice struct {
    Key   string     `json:"key"`
    Kind  NoticeKind `json:"kind"`
    Title string     `json:"title"`
    Body  string     `json:"body"`
}

const (
    NoticeKeyTmpExpiry = "tmp_48h_expiry"
    // future keys land here
)

var noticeRegistry = map[string]Notice{
    NoticeKeyTmpExpiry: {
        Key:   NoticeKeyTmpExpiry,
        Kind:  NoticeKindWarning,
        Title: "/tmp files expire after 48 hours",
        Body:  "Files in /tmp are deleted 48 hours after their last edit. Touching a file (or writing to it) resets the clock. Save anything you want to keep as a watch under /cron.d.",
    },
}
```

`kind` is a hint for the frontend — it picks color/icon and
whether to render as toast vs banner vs modal. The backend doesn't
encode display logic. The /tmp expiry notice goes out as
`warning` because it reads as "heads up, this thing will happen";
it's not "your action succeeded" (that'd be `success`) and it's
not blocking (`error`).

Adding a notice = one entry in the registry + a call site that
triggers it. No migration.

### Trigger mechanism

A `*NoticeQueue` lives on the request scope; anything in the stack
can call `queue.Maybe(noticeKey)` and the queue handles the
once-per-user bookkeeping:

```go
type NoticeQueue struct {
    store    *store.Store
    userID   string  // empty = system; never queues
    queued   []Notice
    sessionSeen map[string]bool  // local short-circuit
    mu       sync.Mutex
}

// Maybe queues the notice for delivery iff this user hasn't seen
// it before. Idempotent within and across calls. No-op when
// userID is empty (cron executor, etc.).
func (q *NoticeQueue) Maybe(key string) {
    if q.userID == "" {
        return
    }
    q.mu.Lock()
    defer q.mu.Unlock()
    if q.sessionSeen[key] {
        return
    }
    seen, _ := q.store.HasSeenNotice(q.userID, key)
    if seen {
        q.sessionSeen[key] = true
        return
    }
    n, ok := noticeRegistry[key]
    if !ok {
        return
    }
    q.queued = append(q.queued, n)
    q.sessionSeen[key] = true
}

// Flush returns the queued notices, marks them seen in the store,
// and resets the queue. Called by the coordinator just before
// emitting the response.
func (q *NoticeQueue) Flush() []Notice { … }
```

The queue is constructed at the top of `runLocal` and passed
through to ShellFS via a new option:

```go
shellfs.WithNoticeQueue(noticeQueue)
```

ShellFS replaces `noteFirstTmpWrite()` (which we just wrote) with
`q.Maybe(coordinator.NoticeKeyTmpExpiry)` on every successful /tmp
mutation.

The seen-flag write happens at `Flush` time, just after the
notice has been added to the response. Failure mode if the user
closes the browser before the notice renders: they miss it. The
alternative (mark-on-dismiss) requires a round-trip we don't want
to engineer for v1.

### Wire shape

Add to `CoordinatorQueryResponse`:

```protobuf
message CoordinatorQueryResponse {
  repeated ResultRow rows = 1;
  string text = 2;
  repeated Notice notices = 3;     // NEW
}

message Notice {
  string key   = 1;
  string kind  = 2;  // "success" | "warning" | "error"
  string title = 3;
  string body  = 4;
}
```

`runLocal` calls `noticeQueue.Flush()` and sets the response's
`notices` field before returning. Notices are emitted only on the
*first* response in a streaming Query (follow mode); we don't
re-flush on subsequent rows.

### Display surfaces

**Browser** (`api/shell.go` `handleShellExec`): after each
SSE event, check for notices on the response and emit a separate
event:

```json
{"type":"notice","key":"tmp_48h_expiry","kind":"warning",
 "title":"/tmp files expire after 48 hours",
 "body":"Files in /tmp are deleted …"}
```

The frontend owns the rendering decision keyed on `kind` —
typical mapping is success-as-toast, warning-as-banner,
error-as-modal, but the backend doesn't enforce that. New
rendering choices land entirely in frontend code without a
schema or proto change.

**CLI** (`weft sh`): notices arrive as a `notice` event on the
gRPC stream. The CLI prepends them to the next chunk of output
in a clearly-marked block:

```
─── /tmp files expire after 48 hours ──────────────────────────
Files in /tmp are deleted 48 hours after their last edit.
Touching a file (or writing to it) resets the clock. Save
anything you want to keep as a watch under /cron.d.
────────────────────────────────────────────────────────────────

(actual command output follows)
```

CLI rendering varies on `kind` only by color (success green,
warning yellow, error red) — same block layout in all cases. The
CLI doesn't need a "dismiss" affordance; printed = delivered =
seen.

### Migrating away from `tmp_warning_seen_at`

Pre-launch, no production data, so:

1. New migration **`032_user_notices.sql`** creates `user_notices`.
2. Same migration drops `customers.tmp_warning_seen_at`. Per the
   no-legacy-support rule, no read-then-migrate dance.
3. `store.HasSeenTmpWarning` and `store.MarkTmpWarningSeen` get
   replaced with `store.HasSeenNotice(userID, key)` and
   `store.MarkNoticeSeen(userID, key)`.
4. ShellFS drops `WithSideChannel`; replaced by
   `WithNoticeQueue`.
5. `noteFirstTmpWrite` becomes one line:
   `q.Maybe(coordinator.NoticeKeyTmpExpiry)`.

The current 5 tests for the warning carry over — same behaviour,
new mechanism.

## Piece 2: User attribution on the FS

### Schema additions

Three tables grow a creator column:

```sql
ALTER TABLE shell_functions ADD COLUMN created_by_user_id TEXT;
ALTER TABLE cron_watches    ADD COLUMN created_by_user_id TEXT;
ALTER TABLE tmp_files       ADD COLUMN created_by_user_id TEXT;
```

Nullable, no FK (consistent with the rest of weft's per-user
tracking). NULL means "no attribution" — pre-existing dev rows or
writes from automated paths (cron executor, weftd ingest, etc.).

`tmp_file_lines` does *not* get a creator column. The file's
creator is a sufficient grain; per-line attribution would be
useful for "who appended what" but we don't have collaborative
edit tooling and the line table is high-volume.

### Store changes

Every `Save…` / `Write…` method on these tables grows a
`createdByUserID string` parameter. Empty string = no
attribution. Existing call sites:

- `SaveShellFunction(customerID, name, source)` →
  `SaveShellFunction(customerID, userID, name, source)`
- `SaveCronWatch(customerID, name, schedule, pipeline)` →
  `SaveCronWatch(customerID, userID, name, schedule, pipeline)`
- `WriteTmpFile`, `AppendTmpFile`, `Mkdir`, `CopyTmpFile`,
  `RenameTmpFile`, `TouchTmpFile` → all gain `userID`

(For `Append` and `Touch` on an existing file: do we update the
creator? **No.** Creator is set at create time; touch/append
updates `updated_at`, not `created_by_user_id`. `mv` keeps the
original creator. `cp` records the user doing the copy as the new
file's creator.)

### ShellFS changes

`ShellFS` already accepts `WithUser(userID, isOwner)`. With the
foundation work, that's now actually populated. Each write path
passes `f.userID` through to the corresponding store method.

### `ls -al` rendering

The user column lives between mode and size:

```
-rw-r--r--  alice    214 B   2026-04-24 09:42  errs
drwxr-xr-x  bob        -     2026-04-22 17:03  incident-2026-04-22
-rw-r--r--  -          12 B  2026-04-23 11:05  legacy-row
```

`-` for NULL creator. Width-aligned per directory.

Mechanics:

- **Warp** (`pkg/bash/builtin.go` `builtinLs`): the long-form
  rendering loop reads `info.Sys()`, type-asserts to a
  `bash.OwnerInfo` interface (new), and renders `Owner()` as the
  user column. Implementations that don't supply owner info just
  render `-`.
- **ShellFS** (`vfsFileInfo`): `Sys()` returns a struct exposing
  `Owner() string`. ReadDir batches creator lookups: a single
  `SELECT id, name FROM users WHERE id IN (…)` joining the IDs from
  the rows, cached in a small map for the duration of ReadDir.
  No N+1 lookups.
- **Stat** (single file): one extra lookup; trivial.

`OwnerInfo` is added to `pkg/bash/fs.go` alongside `LangFS` etc.,
as an optional interface on `fs.FileInfo.Sys()`. Embedders that
don't supply it get the dash rendering for free.

### Resolving user IDs to display names

User identifiers are UUIDs — not friendly in `ls`. Display options:

1. **Email local-part** (`alice@example.com` → `alice`). Works
   when emails are personal; falls down with shared inboxes.
2. **`User.Name`** (the existing free-text field). Set during
   sign-up; can be empty.
3. **Fallback chain**: `Name` if non-empty, else email local-part,
   else first 8 chars of the UUID.

Use the fallback chain. Cheap to implement; matches what
`teamLabel` does in the dashboard already (worth confirming
parity in review).

Truncate to 12 chars in the `ls` column to keep the layout
readable; full identifier visible via `stat` once that exists.

## Tests

### Foundation
- `coordinator` test: gRPC request without `user_id` falls
  through to system attribution (no user on writes).
- `coordinator` test: gRPC request with `user_id` propagates to
  ShellFS, store, and back.

### Notices
- `store` tests: `HasSeenNotice` / `MarkNoticeSeen` round-trip,
  multi-tenancy isolation between users, idempotent
  `MarkNoticeSeen`.
- `coordinator` test: `NoticeQueue.Maybe` queues an unseen
  notice, skips a seen one, flushes once and not again.
- `shellfs` integration test: first /tmp write attaches the
  expiry notice to the response; second write doesn't; third
  write from a different user re-fires (per-user scope confirmed).
- `api` test: SSE stream includes the `notice` event;
  subsequent requests don't.
- `cmd/weft` (CLI) test: notice block prepended to the next
  chunk of stdout.

### User attribution
- Store tests: each `Save…` records the supplied userID; NULL
  permitted; `cp` sets creator to the copying user, not the
  source's creator.
- ShellFS test: ReadDir returns owner info on every entry,
  using the resolved display name; NULL renders as `-`.
- Warp test: `ls -l` long-form output includes the owner column,
  width-aligned, falling back to `-` when `Sys().Owner()` is
  empty or the FS doesn't implement `OwnerInfo`.

## Rollout

Five PRs, dependency-ordered:

1. **Foundation**: `user_id` in proto, plumbed through
   coordinator, exposed on ShellFS via `WithUser`. No behavior
   change yet (just data carried).
2. **Schema**: add `created_by_user_id` to the three tables.
   Store methods grow the parameter; existing callers pass
   userID where they have it.
3. **Notices**: new `user_notices` table, `coordinator/notices.go`,
   wire-shape changes in proto, `NoticeQueue` plumbing.
4. **Tmp warning migration**: drop `tmp_warning_seen_at`,
   replace `noteFirstTmpWrite` with the new queue call. Tests
   move over.
5. **`ls -al` user column**: `OwnerInfo` interface in warp,
   `vfsFileInfo.Sys()` populated, batch lookup in ReadDir,
   manual updates.

(1) and (2) can land together; (3), (4), (5) sequentially.

## Open questions

- **Notice TTL.** A user who hasn't logged in for a year and just
  came back: should the "tmp expires in 48h" notice fire again?
  Position: yes only if we add a `last_relevant_at` to the row
  and the registry entry says "re-fire after N days." Out of
  scope for v1.
- **Watch-executor writes.** The cron executor saves results with
  no user attribution. Cron results aren't in /bin /cron.d /tmp,
  so this is moot for the FS work — but if we ever want notices
  fired from watch context ("your watch alerted!"), we need a
  different scope (probably per-watch rather than per-user).
