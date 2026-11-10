---
id: man-passwd
title: passwd
shell:
  const: ManPasswd
  topics:
    - "man passwd"
    - "man adduser"
    - "man deluser"
    - "man groups"
  banner: "PASSWD(5)"
---

## NAME

passwd — the org team roster and its group-based permissions

## SYNOPSIS

```
cat /etc/passwd
adduser <email>
deluser <email>
usermod -aG|-rG <group> <user>
groups [<user>]
id [<user>]
```

## DESCRIPTION

A weft org's members and their permissions are modeled as Unix-style
group membership. `/etc/passwd` is the read-only roster — one row per
member with their role, status, and groups:

```
USER             ROLE     STATUS    GROUPS
alice@acme.com   admin    active    configmgr  nsmgr  payments-r  payments-w
bob@acme.com     member   active    payments-r  payments-w
carol@acme.com   member   invited   —
payments-cron    cron     system    payments-r
```

Change team membership with `adduser` and `deluser` (admin-only), or from
the dashboard's Members tab. Change a member's groups with `usermod`.

As shown above, each namespace automatically has a system user,
`<namespace>-cron`, listed with role `cron` and status `system`.

## GROUPS

A group is one of these:

- **`configmgr`** — edit access to `/etc`.
- **`nsmgr`** — create and manage namespaces.
- **`<namespace>-r`** — read (query) the logs under `/log/<namespace>`,
  and the definitions, output and history of its crons.
- **`<namespace>-w`** — manage the namespace's crons, tokens and sources.
  Independent of read: a member can have one, the other, or both.

Every namespace has both groups. There is no bare `<namespace>` group.

## COMMANDS

- **`adduser <email>`** — invite someone to the org via email
- **`deluser <email>`** — remove a member from the org entirely
- **`usermod -aG <group> <user>`** — add a user to a group
  **`usermod -rG <group> <user>`** removes them
- **`groups [<user>]`** — list a user's groups (yours if omitted)
- **`id [<user>]`** — show a user's role and groups (yours if omitted)

`adduser`/`deluser` are admin-only.  `usermod -aG beta-r alpha-cron` requires
both  `beta-r` and `alpha-w`.

## SEE ALSO

help builtins, help index
