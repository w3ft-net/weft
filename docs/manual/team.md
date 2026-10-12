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
alice@acme.com   admin    active    binmgr  cronmgr  configmgr  nsmgr  payments  payments+tokens
bob@acme.com     member   active    binmgr  cronmgr  payments  payments+tokens
carol@acme.com   member   invited   —
```

The file is read-only. Manage the team with `adduser` and
`deluser` (admin-only), or from the dashboard's Members tab. Both write
the same underlying data. The admin is sometimes called the org owner;
the role is `admin` everywhere it appears.

## Permissions are group membership

Every permission is a group. There are two families:

**Manager groups** gate the org-wide directories and namespace management:

| Group | Grants |
|---|---|
| `binmgr` | edit `/bin` (functions) |
| `cronmgr` | edit `/cron.d` (crons) |
| `configmgr` | edit `/etc` (notify.yaml, dashboards) |
| `nsmgr` | create and manage namespaces |

**Namespace groups** gate log access, one per namespace:

| Group | Grants |
|---|---|
| `<namespace>` | read (query) `/log/<namespace>` |
| `<namespace>+tokens` | mint producer tokens for that namespace |

Read and produce are independent. A member can have either or both.
The `+tokens` facet uses `+`, which cannot appear in a namespace name.

Admins hold groups as other members do, and the roster lists the groups they have.

## Inviting and removing people

```
adduser dana@acme.com          # invite — sends an invite email
deluser dana@acme.com          # remove from the org entirely
```

The first argument must be an email address. `adduser --help` prints
usage and changes nothing. An unknown flag, a first argument that is not
an email, or extra arguments is a usage error.

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

`groups` and `id` with no argument describe you. For an email that
is not on the team they print `no such member: <email>`.

Group grants apply after a member has accepted their invite. If you `adduser`
someone to a group before they are on the team, they are invited at that point
(the email goes out), and you re-run the command after they accept. An
already-invited member who has not accepted yet gets an error.

`adduser`/`deluser` are admin-only; `groups` and `id` are readable by
any member.

See `man passwd` in the shell for the quick reference.
