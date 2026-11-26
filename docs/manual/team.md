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
alice@acme.com   admin    active    configmgr  nsmgr  payments-r  payments-w
bob@acme.com     member   active    payments-r  payments-w
carol@acme.com   member   invited   —
payments-cron    cron     system    payments-r
```

Users are added or removed in the shell `adduser|deluser <email address>`, or
in the browser, **account** -> **team**. In either case, an invite will be
emailed. You must have the `admin` role to add or remove users. Groups are
granted separately, with `usermod` (see below).

## Groups

There are two kinds of group, plus a system user for each namespace.

**Manager groups** gate org-wide directories and namespace management:

| Group | Grants |
|---|---|
| `configmgr` | edit `/etc` (notify.yaml, dashboards/\*.yaml) |
| `nsmgr` | create and manage namespaces |

**Namespace groups** come in a pair for every namespace:

| Group | Grants |
|---|---|
| `<namespace>-r` | read `/log/<namespace>`, and the definitions, output and history of its crons |
| `<namespace>-w` | manage the namespace's crons, tokens and sources |

**Cron users.** Every namespace has a system user, `<namespace>-cron`, created
with the namespace, for the purposes of executing the cron with correctly
scoped access. If you need a cron to read logs or other cron results from other
namespaces, you can grant read access to one namespace to a system user for
another namespace (see below).

## Granting and revoking

Once someone is a member, add or drop their groups with `usermod`:

```
usermod -aG payments-r dana@acme.com   # read payments logs
usermod -aG payments-w dana@acme.com   # ...and manage its crons, tokens and sources
usermod -aG configmgr dana@acme.com    # ...and edit /etc
usermod -rG payments-r dana@acme.com   # revoke read
```

You manage read rights for a system user the same way. For example:

```
usermod -aG beta-r alpha-cron          # alpha's crons can now read beta
usermod -rG beta-r alpha-cron          # and take it back
```

You must hold `beta-r` and `alpha-w` to run the commands above.

The **account** page's Permissions grid edits members' groups too. For
inspecting what a user can do, use the shell:

```
groups dana@acme.com                   # what dana can do
id dana@acme.com                       # role + groups
groups alpha-cron                      # alpha-cron: alpha-r beta-r
id alpha-cron                          # uid=alpha-cron role=cron groups=alpha-r,beta-r
```
