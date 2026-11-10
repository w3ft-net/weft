---
id: manual-filesystem
title: File system
shell:
  const: ManFilesystem
  topics:
    - "man filesystem"
    - "man scripts"
    - "man tmp"
web:
  section: filesystem
  category: weft-shell
  order: 57
---

## The weft file system

The weft shell provides a synthetic file system for organizing and navigating
your logs, scripts, notes, and configuration. To whatever extent possible,
we've tried to make this feel like real Unix, so you don't have to learn new
things. Given that it's not exactly real, an overrview is warranted.

The shell exposes four mount points, detailed below.

| Mount | Purpose | Writeable by | Versioning |
|---|---|---|---|
| `/log` | Your logs, with a directory/file layout depdented on your [sources](connect-source.md) | nobody | no |
| [`/tmp`](#tmp) | Scratch space for your investigations staging results | whole org | no |
| [`/bin`](#custom-scripts-in-bin) | Reusable scripts in bash or Lua | any member, or `<namespace>-w` for each cron that runs the script | yes |
| [`/cron.d`](#crons-in-crond) | [Crons](crons.md): pipelines that run on a schedule. | `<namespace>-w`, per namespace | yes |
| `/etc` | [Dashboard](dashboards.md) and [Notification](alerts.md#notifications) configuration | `configmgr` | yes |
| `/etc/passwd` | [teams](team.md) roster | nobody | no |
| `/etc/usage` | plan usage and metering | nobody | no |

With the exception of `tmp`, all writeable destinations will validate their
contents and prevent writes if validation fails. Otherwise, [automatic
versioning](#version-history-with-git) applies.

## `/tmp`

This is your staging area for results of grepping and awking and awking your
logs. Without this, commands like `comm`, `join`, and `diff` aren't really
useful. So dump some filtered results in here like so:

```
cat /log/myco/api/main.log | awk 'LEVEL == "ERROR"' > /tmp/errs
```

You now have something a bit easier to poke at that the firehose of stdout your
application is spewing.

### Limitations

- **TTL.** We know your homedir hygiene is terrible, so `/tmp` files are
  deleted 48 hours after its last write. `>`, `>>`, `cp`, `mv` and `touch`
  reset the clock; `ls -l` shows the mtime. If you want to accumulate longer-term
  results, use [crons](crons.md) instead.
- **Bytes.** All tmp files are limited to 512 KB. Total tmp storage limits depend on your org's plan.
- **+rw.** Files in `/tmp` are not executable. If you want to run it, put it in `/bin`.

## Custom scripts in /bin

Anything you can type at the shell prompt can be saved as a named script under
`/bin/`. `/bin` is automatically on your path for shell sessions; a cron calls
a script by its path, `/bin/myscript`. Create scripts with `vi` or with
`cat - > myscript`.

If a script gets invoked by a cron, directly or indirectly, it can only be
changed by a user in the write group of every such cron's namespace, and can't
be deleted until no cron invokes it.

### Language support

The weft shell supports a subset of **bash** and **Lua**.
Scripts default to bash. If you're slinging Lua, add a .lua or a shebang

```
#!/usr/bin/env lua
-- /bin/report
local name = arg[1]   -- a cron, as "<namespace>/<name>"
local avg = weft.avg(name, "1h")
print(string.format("%s averaged %.2f over the last hour", name, avg))
```

Weft supports Lua 5.1, including the standard `table`, `string`,
`math`, and `coroutine` libraries, plus a restricted `os` (`os.clock`,
`os.date`, `os.time`, `os.difftime`).

On top of the standard library, weft adds:

| Global | Purpose |
|---|---|
| `arg` | Positional arguments — `arg[1]`, `arg[2]`, … |
| `print(...)` | Write to stdout (tab-separated args, newline) |
| `eprint(...)` | Same shape, writes to stderr |
| `io.write(...)` | Stdout without trailing newline |
| `io.read("*l" \| "*a" \| "*n")` | Read a line, all of stdin, or a number |
| `lines()` | Iterator over stdin lines — `for line in lines() do ... end` |
| `json.encode(v)` / `json.decode(s)` | JSON ↔ Lua table |
| `weft.*` | [aggregate and predicate functions](#FIXME) |

## Version history

Every save to a script in `/bin`, a cron in `/cron.d/*`, a dashboard in
`/etc/dashboards`, or `/etc/notify.yaml` is captured in a short, browseable
history. You access that history and manipulate it with the `git` command.

Weft `git` is a very limited subset of true git behavior and is thus named
simply to support your muscle memory. Supported commands (`git log`, `git
show`, `git diff`, `git blame` `git restore`) behave as you'd expect, but
there's a lot not there by design. `git --help` lists what is supported.
Unsupported commands and unsupported flags are refused.

Committing is automatic and across your entire org's file structure. There is
no branching, no staging area, and no remotes. Therefore there are no merge or
rebase commands either.

### Limits

Weft keeps each file's **latest six** versions (the live one plus the
five before it). Older versions are deleted.
