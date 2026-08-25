# org_permissions becomes authoritative

> **Status: landed** (2026-07-30, unpushed). Migrations 070–072, the notices
> queue, `/api/notices`, the `setOrgBit` fix and the authoritative-read flip are
> all in with tests. Where implementation diverged from what is written below,
> see "What landed differently" at the end — that section is the current truth.

## What this changes

Today "an admin may write /bin, /cron.d, /etc" is a *derived* fact. Nothing
stores it; `BuildSession` recomputes it from `memberships.role` on every
request, `resolveGrantableMember` refuses to grant to an admin because there is
nothing to grant to, and any question of the form "who can write /cron.d" is a
union of `org_permissions` rows and admin memberships.

After this plan, `org_permissions` holds a row for every active member,
including admins, and that row is the only answer. Reads are a single select
against one table with no role test. Role reverts to meaning what its name says
— who may manage members and grants — and stops being a second, implicit
permission system running alongside the explicit one.

Promotion to admin materializes the four bits as *explicit grants*. Demotion
leaves them in place and tells the operator who caused it. That is the decision
that makes the whole thing work: because demotion keeps them, a bit in the row
means "explicitly granted" at all times, in every state, so there is no
provenance to track and nothing for a trigger to guess. The alternative
(demotion clears them) needs to know which bits predated the promotion, and that
information was never recorded.

The retention is announced, not silent. Demoting Dana emits a notice to the
operator naming exactly what she kept and how to take it back.

## What this supersedes

`docs/plans/terminal-team-management.md` states at `:78` that owners "keep
implicit full access and are never [represented in groups]", and renders them in
`/etc/passwd` as `(all — implicit)` at `:131`. Both are void. Admins appear in
groups like anyone else, and the `/etc/passwd` render loses its special case —
which was one of the motivations, since that fork was going to be paid at every
rendering site forever.

## Migrations

Three, in dependency order. 070 is written but unlanded and needs revising;
071 and 072 are new.

### 070 — namespace creation moves to org_permissions (revise)

Exists at `store/migrations/070_namespace_creation_to_org_permissions.sql`.
Two changes:

- **Drop `AND m.role <> 'admin'` from the backfill.** It was written on the
  premise this plan discards. Under (a) admins hold real rows, so an admin who
  held the global flag gets a real `can_create_namespaces` bit.
- **Rewrite the comment block** that justifies the exclusion, and the one that
  describes demotion as landing an admin on default-deny. Neither is true now.

The `ON CONFLICT ... WHERE deleted_at IS NULL` tombstone guard stays. It is
still correct: a tombstoned row must not be revived, because reviving it
restores the other bits it carries.

### 071 — user_notices grows a delivery queue

`user_notices` is currently a suppression ledger: `(user_id, notice_key, kind,
seen_at NOT NULL DEFAULT now())`, one row per user per key, and a row's
existence means "already shown". Nothing can enqueue a notice for later,
which is what a trigger firing outside any request needs to do.

Every notice instance gets its own row. The UI asks for unseen rows; delivering
them marks them seen. That is the whole model — there is no suppression layer,
no per-key policy, and no uniqueness. A notice that shouldn't fire twice is a
notice whose emitter shouldn't call it twice.

```sql
ALTER TABLE user_notices DROP CONSTRAINT user_notices_pkey;  -- was (user_id, notice_key)
ALTER TABLE user_notices ADD COLUMN id BIGSERIAL PRIMARY KEY;
ALTER TABLE user_notices ALTER COLUMN seen_at DROP NOT NULL;
ALTER TABLE user_notices ALTER COLUMN seen_at DROP DEFAULT;
ALTER TABLE user_notices ADD COLUMN params JSONB;
ALTER TABLE user_notices ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX idx_user_notices_pending
    ON user_notices (user_id) WHERE seen_at IS NULL;
```

`seen_at IS NULL` means **pending delivery**; non-null means shown. Existing
rows predate this and are genuinely seen, so they keep their timestamps and need
no backfill.

`params` carries the facts, not the prose — `{"member":"dana@x.com","groups":
["binmgr","configmgr"]}`. The registry in `coordinator/notices.go` keeps owning
presentation, because the browser and the TUI render differently and a trigger
should not be writing either one's copy.

Store changes that follow (`store/notices.go`):

- `EmitNotice(userID, key, kind, params)` — insert, `seen_at` NULL.
- `PendingNotices(userID)` / `MarkNoticesSeen(ids)` — the drain pair.
- `HasSeenNotice` and the old `MarkNoticeSeen` are deleted. Their callers are
  `NoticeQueue.Maybe` and the graduation-banner gate at `api/handlers.go:756`;
  the latter becomes "is there a pending row with this key", which the generic
  pending fetch already answers.

`NoticeQueue` (`coordinator/notices.go`) shrinks to a per-request dedup set plus
an insert — it keeps `sessionSeen` so a pipeline forking a goroutine per stage
doesn't write 500 identical rows for one command, and loses everything else.

### 072 — materialize admins, add the trigger

1. Insert an all-bits-true row for every active admin with no row, and set all
   four true on those that have one.
2. `AFTER INSERT OR UPDATE OF role, status ON memberships` trigger:
   - → `role = 'admin'` and `status = 'active'`: upsert all four bits true.
   - → `role = 'member'` from `'admin'`: **leave the bits alone**, and insert a
     `member_retained_grants` notice addressed to
     `current_setting('weft.acting_user_id', true)`, with the member and the
     retained group names in `params`. Skip the notice when the GUC is empty —
     an out-of-band change has no operator to inform.
   - → `status = 'removed'`: soft-delete the row. This is lifted out of
     `RemoveMember`'s transaction (see below).

The trigger is the sole writer of admin-derived state. `RemoveMember`'s manual
cleanup goes away, which removes the two-writers-one-row hazard rather than
creating it.

## The GUC is a footgun; wire it deliberately

The trigger reads the operator from `current_setting('weft.acting_user_id',
true)` — the pattern already used by `capture_cron_watch_history()` and
`capture_shell_function_history()` (migration 068), set by `store/shell.go:84`,
`store/dashboard.go:216`, `coordinator/wit.go:415`.

Nothing on the membership write path sets it today, and setting it is not
optional plumbing: `set_config(..., true)` is transaction-local, so it must run
**on the same connection** as the membership write. `store.Orgs` is built over
the pooled `*sql.DB` (`store/store.go:59`), so a naive `set_config` lands on a
different connection and the trigger sees nothing — silently, with no error and
no notice. The role-change path must go through the transactional org store
(`store/store.go:139`, `orgs.New(sqlTx)`), issuing `set_config` as the first
statement in that transaction.

A test must assert the notice actually lands, because the failure mode here is
silence.

## Delivery

The TUI needs no new wire at all. Notices already ride the query response —
`CoordinatorExecuteResponse.notices` in the proto, rendered by `RenderNotice` in
`cmd/weft/runner/runner.go:194`. `NoticeQueue` gains a `Drain()` that pulls the
user's pending rows at the top of `runLocal` and appends them to whatever the
request was already going to return. No proto change, no new renderer.

The browser has no guaranteed shell request, so it needs a poll: `GET
/api/notices` returning pending rows and `POST /api/notices/{id}/seen`. The SPA
already has the shape for this in `GraduationBanner.tsx`.

Both paths mark on delivery. The old mark-on-flush hazard — a notice burned by a
connection that dropped before it rendered — is now bounded rather than
permanent: the row survives, so the worst case is one missed render of one
instance, not the loss of the only copy a user was ever going to get.

## Go changes

| File | Change |
|---|---|
| `store/permissions.go` | `OrgPermission` += `CanCreateNamespaces` |
| `auth/session.go` | `BuildSession` stops deriving bits from `isOwner`; reads the row unconditionally. `Session` += `CanCreateNamespaces`. `IsOwner` stays, for member-management gating only |
| `store/team_groups.go` | `managerGroupColumn` += `nsmgr`; `resolveGrantableMember` drops the owner-refusal; `RemoveMember` drops the `org_permissions` soft-delete |
| `api/handlers.go:2093` | `handleCreateNamespace` reads the session bit, loses its extra `GetUserByID` |
| `store/user.go` | `User.CanCreateNamespace` and `SetCanCreateNamespace` deleted |
| `mounts/mounts.go`, `api/fs.go` | `OrgPerms` += `CanCreateNamespaces`; `sessionPerms` carries it |
| `store/notices.go` | `EmitNotice` / `PendingNotices` / `MarkNoticesSeen`; `HasSeenNotice` and old `MarkNoticeSeen` deleted |
| `coordinator/notices.go` | `NoticeQueue` drops the seen-check, keeps per-request dedup; registry entry for `member_retained_grants` with `params` interpolation; `Drain()` |
| `api/notices.go` (new) | `GET /api/notices`, `POST /api/notices/{id}/seen` |
| `cmd/weft-demo-seed/main.go:204,340` | seeds an org-scoped grant instead of the user flag |

`store/team_perms.go` and the `/etc/passwd` render lose their admin special
case; check both against `terminal-team-management.md:131` when writing it.

## Tests

Per the failing-test-first rule, each of these lands red first:

1. **Demotion retains grants** — promote a member holding `configmgr`, demote,
   assert all four bits still true. This is the case that decided the design;
   it should exist as executable proof.
2. **Demotion notifies the operator** — same transition with
   `weft.acting_user_id` set, assert a pending `member_retained_grants` row
   addressed to the operator with the member and groups in `params`. Then the
   negative: GUC unset → no notice, no error.
3. **Admin reads its bits from the row** — `BuildSession` for an admin whose row
   was hand-cleared returns false bits. Proves role is no longer consulted.
4. **`deluser <admin> binmgr` works** — previously refused.
5. **Pending notices don't suppress themselves** — `HasSeenNotice` false while
   `seen_at IS NULL`; the 071 regression that would otherwise be invisible.
6. **Instances stack and drain once** — two emissions of the same key leave two
   pending rows; draining returns both and leaves nothing pending; a second
   drain returns nothing.

## Risks

- **`org_permissions.user_id` has no FK to `users`.** 072's materialization will
  happily create rows for membership rows pointing at users that don't exist —
  prod has four such orphans right now. Clean those first (below) or the
  materialization launders them into permission rows.
- **`setOrgBit` has a live bug** (`store/team_groups.go:121`): a soft-delete-
  scoped `First` misses a tombstoned row, falls through to `Create`, and
  violates `idx_orgperm_org_user`, which has no `deleted_at` predicate. So
  granting a group to a removed-then-re-added member errors today. 072 makes
  tombstones more common by handing removal to the trigger, so fix this in the
  same pass.
- **Reads get stricter.** Any path that assumed an admin works without a row now
  needs one. The 072 materialization covers everyone existing; the trigger
  covers everyone future; the gap is anything that creates a membership outside
  both, e.g. a direct SQL insert in a script.
- **Notice rows now accumulate.** A row per instance means `user_notices` grows
  with usage rather than plateauing at one row per user per key. Small and
  append-only, but it wants the retention treatment every other growing table
  here gets rather than being assumed bounded.
- **A chatty emitter is now a visible bug, not a silent no-op.** The seen-check
  used to absorb over-firing; nothing does now. `mounts/tmp.go:439` calls
  `Maybe(NoticeKeyTmpExpiry)` on every /tmp write and wants a look before this
  lands.

## Prerequisite: repair prod memberships

Unrelated to this refactor in cause, but it blocks it and blocks you.

`memberships` holds a row for `paul@graybeardcorp.com` in org `graybeard` whose
`user_id` is `80bf13ca-…`, a user that no longer exists; the live user id is
`0b2b36d4-…`. Migration 069's email-dedup collapsed two rows for that address
and its `created_at ASC` tiebreak kept the older, dead one. Result:
`OrgsFor()` returns nothing, `BuildSession` raises `ErrNoMembership`, and every
org-scoped endpoint 403s. Three further orphan memberships exist
(`paul@kevlarconsulting.com`, two for `jonathan+test@gmail.com`), plus an
organization with an empty name created 2026-07-30 03:10:48 UTC that nothing
references.

Fix the live membership first, then decide on the orphans — they are exactly
what the 072 risk above is about.

**Update:** 072's materialization inner-joins `users`, so orphan memberships
fall out of it and cannot be laundered into permission rows. The prod repair is
still needed — `paul@graybeardcorp.com` still 403s on every org-scoped endpoint
— but it no longer blocks this migration.

## What landed differently

Eight deltas from the plan above, in descending order of how much they change
the design.

**1. There is no role-change path, so demotion is tested through SQL.**
`memberships.role` is written once at insert and never updated: invites hardcode
`RoleMember`, the owner is set at org creation, and nothing anywhere mutates it.
What varies for a user is *which membership is active* — the sidebar switcher
sets `X-Active-Org` and `BuildSession` resolves the row for that org, so a user
can be an admin in one org and a plain member in another without any role ever
changing. The GUC wiring in "The GUC is a footgun" therefore has nothing to
wire. `SetMemberRole` was deliberately **not** added; the trigger's demotion
branch is covered by `store/migration_072_admin_permissions_test.go`, which
drives the update and `set_config` in one transaction. When a promote/demote
surface is built, it must do the same — the pooled `*sql.DB` will silently drop
the GUC.

**2. Notices name their org.** `user_notices` is keyed by user, not by
(user, org), so a `member_retained_grants` notice can surface with any org
selected in the switcher. The trigger puts the org name in `params` and the
renderer leads with it ("In acme, dana@x.com is no longer an admin…"). No
`org_id` column and no delivery filtering — the notice says where it happened
rather than waiting for you to be looking at the right place.

**3. The mount write gates lost their `IsOwner` bypass.** `mounts/{bin,cron,
etc}.go` tested `!IsOwner && !CanManageX`. Leaving that would have kept role as
a second permission authority in the one place it matters most, so the eight
sites now test the flag alone. `mounts/log.go` keeps its `IsOwner` check — that
is namespace *read* visibility, backed by `NamespacePermission`, and out of
scope here. `denyWrite` now names the group to ask for rather than saying
"owner only", which had become the wrong remedy.

**4. `/tmp`'s expiry notice got a real gate.** `noteFirstWrite` fired on all six
`/tmp` mutations and relied on the suppression the queue no longer has; it would
have warned on every command touching `/tmp`. It is now `noteFileCreated(created
bool)`, emitting only on the write that gives a user their first `/tmp` file.
Renames and touches of existing files don't call it; `Mkdir` doesn't either,
since a directory holds nothing that can expire.

**5. The graduation banner became a two-renderer notice.** Moving it onto the
queue meant the CLI's `Drain()` — which takes every pending row — would consume
it before the browser rendered it. Rather than route by surface, it has a
registry entry: the shell renders it as a notice, the browser as the banner, and
whichever the user reaches first delivers it. `graduateOnboarding` is the
emitter and fires once, being CAS-guarded.

**6. `BuildTeamPerms` / `ApplyTeamPerms` include owners.** Omitting them would
make `/etc/perms` a partial view of state it claims to be authoritative over,
and applying such a doc would silently strip an admin. Listing an owner is no
longer an error, and missing-from-doc means default-deny for them too. Stripping
an owner's bits doesn't strip their role, so they can grant themselves back.

**7. `setOrgBit`'s revive clears the other bits.** The fix could have cleared
`deleted_at` and kept the row as-is, but that restores grants the member lost
when they were removed. Reviving a tombstone now zeroes all four and sets only
the requested bit: re-adding a member restores the membership, not the grants.

**8. The `/api/notices` endpoints have no SPA consumer yet.** The plan's table
listed only `api/notices.go`, and that is what was built —
`GET /api/notices` and `POST /api/notices/{id}/seen`, both session-scoped, with
the dismiss endpoint 404ing on another user's ids. The browser-side toaster that
polls them is not written; today the only browser surface consuming a notice is
the existing graduation banner.
