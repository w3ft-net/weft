---
id: manual-team
title: Teams & permissions
web:
  section: teams
  category: getting-started
  order: 30
---

## /etc/passwd

Your org's members and what they can do is reflected on the accounts page, as
well as in the weft shell in `/etc/passwd`:

```
$ cat /etc/passwd
USER             ROLE     STATUS    GROUPS
alice@acme.com   admin    active    binmgr  cronmgr  configmgr  nsmgr  payments  payments+tokens
bob@acme.com     member   active    binmgr  cronmgr  payments  payments+tokens
carol@acme.com   member   invited   —
```

Users are added or removed in the shell `adduser|deluser <email address>`, or
in the browser, **account** -> **team**. In either case, an invite will be
emailed. You must have the `admin` role to add or remove users. 

## Groups

There are two main types of group:

**Manager groups** gate org-wide directories and namespace management:

| Group | Grants |
|---|---|
| `binmgr` | edit `/bin` (functions) |
| `cronmgr` | edit `/cron.d` (crons) |
| `configmgr` | edit `/etc` (notify.yaml, dashboards/\*.yaml) |
| `nsmgr` | create and manage namespaces |

**Namespace groups** control log access and token creation:

| Group | Grants |
|---|---|
| `<namespace>` | read `/log/<namespace>` |
| `<namespace>+tokens` | create producer tokens for that namespace |

A member can have either or both.

Admins hold groups as other members do, and the roster lists the groups they have.

## Granting and revoking

Once someone is a member, add or drop their groups:

```
adduser dana@acme.com payments         # read payments logs
adduser dana@acme.com payments+tokens  # ...and mint producer tokens
adduser dana@acme.com cronmgr          # ...and edit /cron.d
deluser dana@acme.com payments         # revoke read
```

There is no clicky version of this -- you must use the shell. Same for getting the deets on what groups a user belongs to:

```
groups dana@acme.com                   # what dana can do
id dana@acme.com                       # role + groups
```

(`groups` and `id` with no argument describe you.)
