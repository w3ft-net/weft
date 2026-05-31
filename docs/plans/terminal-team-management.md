# Terminal team management & integration credentials

w3ft's premise is that anything you can do in the browser you can also
do from the shell. Two areas break that promise today, and both are
account administration rather than log work:

1. **Integration credentials** — Slack and PagerDuty are connected
   only through the browser (`Account.tsx` → Integrations,
   `POST /api/integrations/slack/connect`, `PUT /api/integrations/pagerduty`).
   `/etc/notify.yaml` can *route* to them by name, but the terminal
   can't *create* the credential.

2. **Team & permissions** — inviting/removing members and renaming the
   org are browser-only (`Account.tsx` `TeamCard`). Per-directory edit
   rights (`/bin` `/cron.d` `/etc`) have a browser checkbox grid
   (`PermissionsCard`, `Account.tsx:632`), but **per-namespace** access
   has no browser editor at all — the UI explicitly punts to
   `vi /etc/team.yaml` (the footnote at `Account.tsx:728`). So the
   richest permission surface that exists today is *already* the
   terminal, just as a raw YAML blob.

This plan closes both gaps and, while doing so, replaces the
free-form `/etc/team.yaml` editor with a Unix-flavored model:
namespaces and the three directory-manager roles become **groups**,
membership is mutated through `adduser`/`deluser`, and the roster is a
**read-only** `/etc/passwd`.

## Part A — `weft integration`

A subcommand, not a pseudofile, because these are **write-only
secrets** — same reasoning as `weft token create` showing the token
exactly once. A PagerDuty routing key has no business sitting readable
in `/etc`.

```
weft integration list                                   # what's connected + the names notify.yaml can reference
weft integration pagerduty add <name> --key <key>       # PUT  /api/integrations/pagerduty
weft integration pagerduty rm  <name>                   # DELETE /api/integrations/pagerduty/{name}
weft integration slack connect                          # OAuth, brokered terminal-side
weft integration slack rm                               # DELETE /api/integrations/slack
```

The naming follows the existing CLI grammar (`weft source`,
`weft token`, `weft namespace` — singular nouns).

- **PagerDuty** is a pure key paste → fully terminal. The `<name>` is
  the integration name `notify.yaml` references; `weft integration list`
  prints those names so the two surfaces stay legible together.
- **Slack** is OAuth, so authorizing a workspace inherently needs one
  browser redirect. We broker it exactly like `weft login`'s device
  approval: `connect` prints an authorization URL (and opens it),
  the existing `GET /api/integrations/slack/callback` completes the
  handshake, and the CLI polls until the workspace shows connected.
  Initiated and confirmed from the terminal; the browser is only the
  OAuth surface. This is consistent with the MO — `weft login` already
  works this way.
- **Webhook and email** destinations need no credential and are
  already fully terminal via `/etc/notify.yaml`. No change.

## Part B — Team management as Unix groups

### The model

A namespace is simultaneously a directory under `/log` and a **group**
of the same name. Everything that grants a permission is expressed as
group membership, and there are exactly two group families:

| Group | Grants | Backing primitive |
|---|---|---|
| `binmgr` | edit `/bin` | `OrgPermission.CanManageFunctions` |
| `cronmgr` | edit `/cron.d` | `OrgPermission.CanManageWatches` |
| `configmgr` | edit `/etc` | `OrgPermission.CanManageEtc` |
| `<ns>` | read (query) `/log/<ns>` | `NamespacePermission.CanRead` |
| `<ns>+tokens` | mint producer tokens for `<ns>` | `NamespacePermission.CanCreateTokens` |

No data-model change: these are the permission bits that already
exist (`store/permissions.go`), just given names. Owners
(`TeamMember.Role == "owner"`) keep implicit full access and are never
listed in any group — same as `team_perms.go:151` does today.

Deliberately **no `x` bit**. We considered a "traverse the namespace,
see service shapes, but not the log bodies" capability and dropped it;
read and produce are the only namespace facets.

The `+tokens` suffix uses `+` rather than `:` on purpose: `+` is
illegal in a namespace name (so `payments+tokens` can never collide
with a real namespace) and it survives a table/colon-delimited render.

### Command surface

`adduser`/`deluser`/`groups`/`id` are **shell builtins**, registered
the way `dashboard` and `chart` are (`shell/shell.go:119`), so they
work identically in the CLI shell and the wasm web shell. All four are
**owner-gated**, matching the rule that only owners may write
`/etc/team.yaml` today (`mounts/etc.go:123`).

```
adduser  alice@acme.com                   # invite to org (no group)
deluser  alice@acme.com                   # remove from org entirely

adduser  bob@acme.com binmgr              # edit /bin
adduser  bob@acme.com cronmgr             # edit /cron.d
adduser  bob@acme.com configmgr           # edit /etc
adduser  bob@acme.com payments            # read payments logs
adduser  bob@acme.com payments+tokens     # + mint producer tokens for payments
deluser  bob@acme.com payments            # revoke read on payments

groups   bob@acme.com                     # binmgr cronmgr payments payments+tokens infra
id       bob@acme.com                     # uid=bob@acme.com role=member groups=...
```

`adduser <email>` with no second arg is the **invite** path
(`handleInviteTeamMember`), which closes the only genuinely
browser-only team capability. `adduser <email> <group>` on a member
who hasn't accepted yet **stages** the group to apply on accept (see
the migration note below).

### The roster: read-only `/etc/passwd`

`/etc/team.yaml` is removed. In its place, `/etc/passwd` is a
**read-only** pseudofile that renders as a table. It is never edited
directly — every mutation goes through `adduser`/`deluser` or the web
admin. (This is the real Unix discipline anyway: you don't `vi`
`/etc/passwd`, you run `useradd`.) Rendering a pseudofile as a table
rather than its bytes is the same move `ls -l /cron.d` already makes
when it appends a sparkline column.

```
$ cat /etc/passwd
USER             ROLE    STATUS    GROUPS
alice@acme.com   owner   active    (all — implicit)
bob@acme.com     member  active    binmgr  cronmgr  payments  payments+tokens  infra
carol@acme.com   member  invited   payments  (staged until accepted)
dave@acme.com    member  active    —
```

Every token in the GROUPS column is, by construction, a valid second
argument to `adduser`/`deluser` — what you read is exactly what you can
grant or revoke. Owners render as `(all — implicit)` and are absent
from the per-group lists. Not-yet-accepted invites show `STATUS =
invited` and any staged groups.

`groups <email>` and `id <email>` are single-user convenience reads
over the same store state.

## Data model & wiring

Nothing in `store/permissions.go` changes. The work is:

- **`store/team_perms.go`** loses its writeable role. `ApplyTeamPerms`
  becomes the engine behind `adduser`/`deluser` (one grant/revoke is a
  read-modify-write of the same underlying rows it already reconciles),
  and `BuildTeamPerms` becomes the `/etc/passwd` renderer. The YAML
  marshal/unmarshal path (`Marshal`, `ParseTeamPerms`, the
  `AccessLevel` enum) is deleted.
- **`mounts/etc.go`** drops `team.yaml` (the entry at `:62`, the read
  at `:75`/`:101`, and the owner-gated write at `:121`) and adds
  `passwd` as a read-only table render. Writes to `/etc/passwd` return
  the standard `denyWrite` used for other read-only nodes.
- **Builtins** `adduser`/`deluser`/`groups`/`id` are registered in
  `shell/shell.go` and routed to coordinator handlers (new
  `POST /api/team/groups` style endpoints, or fold into the existing
  `/api/team/*` + `/api/permissions` handlers — they already do the
  underlying invite/remove/grant work).

## Follow-ons (in scope, called out so they don't get dropped)

1. **Web admin must keep up.** With `/etc/team.yaml` gone, the browser
   `PermissionsCard` (`Account.tsx:632`) can no longer punt namespace
   perms to `vi`. It needs real controls for `<ns>` read and
   `<ns>+tokens` so the "mods via web admin" half of the promise holds.
   The existing bin/cron/etc checkbox grid stays; it's just the
   `binmgr`/`cronmgr`/`configmgr` groups under a different skin.

2. **Staged grants for pending invites.** `ApplyTeamPerms` currently
   hard-rejects `UserID == nil` (`team_perms.go:234`). To support
   `adduser carol@acme.com payments` before Carol has accepted, we need
   to stash the intended groups against the `TeamMember` row and apply
   them in the invite-accept path (`handleAcceptInvite`). The
   `/etc/passwd` table surfaces these as `(staged until accepted)`.

## Out of scope

- **Billing / Stripe** stays browser-only — defensible, and not what
  this plan is about.
- **Superadmin** (`/api/admin/*`) is internal tooling, intentionally
  excluded from any user-facing surface.
