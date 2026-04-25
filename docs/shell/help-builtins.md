---
id: help-builtins
title: Builtins
shell:
  const: HelpBuiltins
  topics:
    - "help builtins"
web:
  section: builtins
  order: 60
---

## Querying logs

- `cat /log/<app>.log` — stream all lines for an app
- `head /log/<app>.log` — first N lines (oldest)
- `tail /log/<app>.log` — last N lines; `tail -f` follows
- `apps` — list available apps

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
- `chart <watch>` — render recent watch results in the terminal (`man chart`)
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

## Version history (wit)

- `wit log <path>` — list versions; `-p` includes diffs
- `wit show <path> <ref>` — print a version's content
- `wit diff <path> <a>..<b>` — unified diff between two refs
- `wit checkout <path> <ref>` — make a past version the new HEAD
- `wit amend [-m MSG] <path> [<ref>]` — set a commit message

Refs: `HEAD`, `HEAD^`, `HEAD^N`, or a hash prefix (≥4 hex chars).
Tracked under `/bin` and `/cron.d` only; last 5 versions retained.

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
