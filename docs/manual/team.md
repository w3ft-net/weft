---
id: manual-team
title: Team & permissions
web:
  section: team
  category: getting-started
  order: 30
---

## The roster: /etc/passwd

Your org's members and what they can do live in `/etc/passwd`, a
read-only table you `cat` like any other file:

```
$ cat /etc/passwd
USER             ROLE     STATUS    GROUPS
alice@acme.com   owner    active    (all — implicit)
bob@acme.com     member   active    binmgr  cronmgr  payments  payments+tokens
carol@acme.com   member   invited   —
```

You never edit this file — the team is managed with `adduser` and
`deluser` (owner-only), or from the dashboard's Members tab. Both write
the same underlying data.

## Permissions are group membership

Every permission is a group. There are exactly two families:

**Manager groups** gate the three org-wide directories:

| Group | Grants |
|---|---|
| `binmgr` | edit `/bin` (functions) |
| `cronmgr` | edit `/cron.d` (crons) |
| `configmgr` | edit `/etc` (notify.yaml, dashboards) |

**Namespace groups** gate log access, one per namespace:

| Group | Grants |
|---|---|
| `<namespace>` | read (query) `/log/<namespace>` |
| `<namespace>+tokens` | mint producer tokens for that namespace |

Read and produce are independent — a member can have either or both.
The `+tokens` facet uses `+` rather than `:` so it can never collide
with a real namespace name.

Owners have implicit full access and never appear in a group.

## Inviting and removing people

```
adduser dana@acme.com          # invite — sends an invite email
deluser dana@acme.com          # remove from the org entirely
```

`adduser <email>` with no group sends an invite link. The person joins
when they accept it (or the next time they sign in with that email).

## Granting and revoking

Once someone is a member, add or drop their groups:

```
adduser dana@acme.com payments         # read payments logs
adduser dana@acme.com payments+tokens  # ...and mint producer tokens
adduser dana@acme.com cronmgr          # ...and edit /cron.d
deluser dana@acme.com payments         # revoke read

groups dana@acme.com                   # what dana can do
id dana@acme.com                       # role + groups
```

Group grants apply only once a member has accepted their invite — there
is no staging. If you `adduser` someone to a group before they're on the
team, they get invited then and there (the email goes out), and you
re-run the command after they accept. An already-invited member who
hasn't accepted yet gets a clear error, not a silent no-op.

`adduser`/`deluser` are owner-only; `groups` and `id` are readable by
any member.

See `man passwd` in the shell for the quick reference.
