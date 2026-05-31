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
adduser <email> [<group>]
deluser <email> [<group>]
groups [<email>]
id [<email>]
```

## DESCRIPTION

A weft org's members and their permissions are modeled as Unix-style
group membership. `/etc/passwd` is the read-only roster — one row per
member with their role, status, and groups:

```
USER             ROLE     STATUS    GROUPS
alice@acme.com   owner    active    (all — implicit)
bob@acme.com     member   active    binmgr  cronmgr  payments  payments+tokens
carol@acme.com   member   invited   —
```

The file is read-only; mutate the team with `adduser` and `deluser`
(owner-only), or from the dashboard's Members tab. Owners have implicit
full access and never appear in a group.

## GROUPS

A group is one of three things:

- **`binmgr` / `cronmgr` / `configmgr`** — edit access to `/bin`,
  `/cron.d`, and `/etc` respectively.
- **`<namespace>`** — read (query) the logs under `/log/<namespace>`.
- **`<namespace>+tokens`** — mint producer tokens for that namespace.
  Independent of read: a member can have one, the other, or both.

The `+tokens` facet uses `+` (not `:`) because `+` is illegal in a
namespace name, so it can never collide with a real namespace.

## COMMANDS

- **`adduser <email>`** — invite someone to the org. Sends an invite
  email; they join when they accept (or next sign in).
- **`adduser <email> <group>`** — add an existing member to a group.
- **`deluser <email> <group>`** — revoke a group membership.
- **`deluser <email>`** — remove a member from the org entirely.
- **`groups [<email>]`** — list a member's groups (yours if omitted).
- **`id [<email>]`** — show a member's role and groups.

`adduser`/`deluser` are owner-only. A member must have accepted their
invite before they can be added to groups.

## EXAMPLES

```
adduser dana@acme.com                  # invite
adduser dana@acme.com payments         # let dana read payments logs
adduser dana@acme.com payments+tokens  # ...and mint producer tokens
adduser dana@acme.com cronmgr          # ...and edit /cron.d
deluser dana@acme.com payments         # revoke read
deluser dana@acme.com                  # remove from the org
groups dana@acme.com
```

## SEE ALSO

help builtins, help index
