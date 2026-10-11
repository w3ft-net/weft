---
id: help-builtins
title: Builtins
shell:
  const: HelpBuiltins
  topics:
    - "help builtins"
web:
  section: builtins
  category: query
  order: 60
---

## Querying logs

- `cat /log/<app>.log` — stream all lines for an app
- `head /log/<app>.log` — first N lines (oldest)
- `tail /log/<app>.log` — last N lines; `tail -f` follows
- `ls /log` — list available apps (browse the namespace tree)

## Text pipeline

- `grep` — filter lines by pattern
- `awk` — field-oriented extraction / predicates
- `sed` — stream substitution
- `jq` — JSON filtering and projection (implies JSON source)
- `cut`, `paste` — column slicing and joining
- `sort`, `uniq` — ordering and de-duplication
- `wc` — line/word/byte counts
- `rev`, `tac` — reverse chars / reverse line order
- `tee`, `tr` — duplicate to file / translate chars
- `xargs` — build commands from stdin
- `less` — page the final output (see `man less`)

## Filesystem

- `ls [-l] [-a] [-h] [path]` — list `/bin`, `/cron.d`, `/etc`, `/log`, `/tmp`; `ls -l /cron.d/` adds a sparkline
- `cat /bin/<fn>` — view a saved function
- `cat /cron.d/<watch>` — view a watch definition
- `chart <cron>` / `... | chart [-x ... -y ...]` — render a saved cron or a pipeline as an in-terminal chart (`man chart`)
- `>` / `>>` — redirect pipeline output into `/tmp`
- `mkdir /tmp/<dir>` — organise scratch files into folders
- `cp <src> <dst>` / `mv <src> <dst>` — duplicate / rename (within `/tmp`)
- `touch <path>` — create empty / reset the 48h clock on a `/tmp` file
- `rm <path>` / `rm -rf <dir>` — delete a file or directory
- `vi <path>` — edit (vim keys)
- `cd <path>` — change the virtual cwd

## Working on saved results

- `diff <a> <b>` — line-level differences between two files
- `comm <a> <b>` — sorted-set merge: only-in-a, only-in-b, in-both
- `join <a> <b>` — relational join on a key column (`-t` / `-1` / `-2`)
- `paste <a> <b> [...]` — column-merge files line-by-line (`-d`)
- `wc <files>` — `-l` lines, `-w` words, `-c` bytes, `-m` chars, `-L` longest line

## Version history (git)

`git` is a subset of git over `/bin` and `/cron.d`; HEAD is the org's latest change.

- `git log [-p] [--oneline] [-- <path>]` — list versions; `-p` includes diffs
- `git show <rev>[:<path>]` — a commit, or a file's content at that revision
- `git diff <a> <b> [-- <path>]` — unified diff between two revisions
- `git blame <path>` — who last changed each line
- `git restore --source=<rev> <path>` — make a past version current (a new commit)
- `git revert <rev>` — undo one commit with a new commit
- `git commit --amend -m MSG` — reword the latest commit

Revisions: `HEAD`, `HEAD~N`, `<a>..<b>`, or a hash prefix (≥4 hex chars).
Tracked under `/bin` and `/cron.d` only; last 5 past versions retained.
Other git commands are refused as unsupported.

## Sharing

- `clipboard /bin/<name>` — copy a deep link to the script (current range + args) to the system clipboard; teammates open it in their shell pre-filled (`man clipboard`)

## Team (owners)

- `cat /etc/passwd` — the team roster: each member's role, status, and groups (read-only)
- `adduser <email>` — invite someone to the org (sends an invite email)
- `adduser <email> <group>` — add a member to a group; `deluser` to remove
- `deluser <email>` — remove a member from the org
- `groups <email>` / `id <email>` — show a member's groups

Groups are the permission model: `binmgr` / `cronmgr` / `configmgr`
(edit `/bin`, `/cron.d`, `/etc`), a namespace name (read its logs), or
`<ns>+tokens` (mint producer tokens for it). Owners have implicit full
access. See `man passwd`.

## Alerting

- `<pipeline> | alert <condition>` — see `help alerts` or `man alert`

## Shell

- `range` — show or set the query window
- `which <name>` — show where a command lives
- `type <name>` — identify a command
- `history` — past commands
- `clear` — clear the screen
- `exit` — quit the shell

> Each entry above is a command — `man <name>` where a page exists.
