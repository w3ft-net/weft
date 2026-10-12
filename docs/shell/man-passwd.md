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
alice@acme.com   admin    active    binmgr  cronmgr  configmgr  nsmgr  payments  payments+tokens
bob@acme.com     member   active    binmgr  cronmgr  payments  payments+tokens
carol@acme.com   member   invited   —
```

The file is read-only; mutate the team with `adduser` and `deluser`
(admin-only), or from the dashboard's Members tab. An admin (sometimes
called the org owner) holds groups like any other member and is listed
with them.

## GROUPS

A group is one of these:

- **`binmgr` / `cronmgr` / `configmgr`** — edit access to `/bin`,
  `/cron.d`, and `/etc` respectively.
- **`nsmgr`** — create and manage namespaces.
- **`<namespace>`** — read (query) the logs under `/log/<namespace>`.
- **`<namespace>+tokens`** — mint producer tokens for that namespace.
  Independent of read: a member can have one, the other, or both.

The `+tokens` facet uses `+` (not `:`) because `+` is illegal in a
namespace name, so it can never collide with a real namespace.

## COMMANDS

- **`adduser <email>`** — invite someone to the org. Sends an invite
  email; they join when they accept (or next sign in).
- **`adduser <email> <group>`** — add a member to a group. If the email
  isn't on the team yet, they're invited now (the email goes out) and
  the grant doesn't apply until they accept — re-run the command once
  they've joined. If they were invited but haven't accepted, you get a
  clear error rather than a silent no-op.
- **`deluser <email> <group>`** — revoke a group membership.
- **`deluser <email>`** — remove a member from the org entirely.
- **`groups [<email>]`** — list a member's groups (yours if omitted).
  Prints `<email>: group group ...`; an email not on the team gives
  `no such member: <email>`.
- **`id [<email>]`** — show a member's role and groups, as
  `uid=<email> role=<role> groups=a,b`.

Every command takes `--help` (prints usage, changes nothing). An unknown
flag, a first argument that isn't an email address, or extra arguments
is a usage error.

`adduser`/`deluser` are admin-only. Group grants apply only once the
member has accepted their invite; there is no staging.

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
